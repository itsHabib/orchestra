package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// ServerName and ServerVersion advertise the MCP server identity to clients
// during the initialize handshake. Bumping ServerVersion when the tool
// surface changes is the cheapest signal to the parent Claude that a new
// capability landed.
const (
	ServerName    = "orchestra"
	ServerVersion = "0.1.0"
)

// shutdownGrace caps how long ServeHTTP waits for in-flight requests to
// drain after ctx cancellation before forcing the server down. Keep small —
// the parent Claude is expected to retry on transient errors.
const shutdownGrace = 5 * time.Second

// Server bundles the MCP protocol implementation, the run registry, and the
// pluggable subprocess + steering dependencies. Construct via New; do not
// zero-value a Server.
type Server struct {
	mcp           *server.MCPServer
	registry      *Registry
	spawner       Spawner
	stateReader   StateReader
	steerer       Steerer
	workspaceRoot string
}

// Options configures a Server. Zero-value fields fall back to the production
// defaults: registry at DefaultRegistryPath, workspace root at
// DefaultWorkspaceRoot, ExecSpawner with its default binary lookup,
// DefaultStateReader, and SessionSteerer for the steerer.
//
// Tests inject stubs by setting the fields explicitly.
type Options struct {
	Registry      *Registry
	WorkspaceRoot string
	Spawner       Spawner
	StateReader   StateReader
	Steerer       Steerer
}

// New returns a Server with all four tools registered against the embedded
// mark3labs MCPServer. The returned value is ready to ServeStdio / ServeHTTP.
func New(opts *Options) (*Server, error) {
	if opts == nil {
		opts = &Options{}
	}
	registry := opts.Registry
	if registry == nil {
		registry = NewRegistry(DefaultRegistryPath())
	}
	root := opts.WorkspaceRoot
	if root == "" {
		root = DefaultWorkspaceRoot()
	}
	spawn := opts.Spawner
	if spawn == nil {
		spawn = &ExecSpawner{}
	}
	read := opts.StateReader
	if read == nil {
		read = DefaultStateReader
	}
	steer := opts.Steerer
	if steer == nil {
		steer = SessionSteerer
	}

	mcpSrv := server.NewMCPServer(
		ServerName,
		ServerVersion,
		// listChanged=false: the orchestra tool surface is fixed at build
		// time, no runtime AddTool/DeleteTool. Setting this to true would
		// require us to push tools/list_changed notifications we never
		// generate.
		server.WithToolCapabilities(false),
		// Recovery: tool handler panics surface as JSON-RPC errors
		// instead of taking the whole MCP server (and its still-running
		// subprocess registry) down.
		server.WithRecovery(),
	)
	s := &Server{
		mcp:           mcpSrv,
		registry:      registry,
		spawner:       spawn,
		stateReader:   read,
		steerer:       steer,
		workspaceRoot: root,
	}
	regs := s.Tools()
	for i := range regs {
		mcpSrv.AddTool(regs[i].Tool, regs[i].Handler)
	}
	return s, nil
}

// MCP exposes the underlying mark3labs server. Useful for tests that want to
// drive the tool layer without going through stdio/HTTP.
func (s *Server) MCP() *server.MCPServer { return s.mcp }

// Registry exposes the run registry. Callers should treat it read-only;
// the server is the canonical writer.
func (s *Server) Registry() *Registry { return s.registry }

// WorkspaceRoot exposes the configured root directory under which each new
// run gets a workspace at <root>/<run_id>/. Diagnostic-only.
func (s *Server) WorkspaceRoot() string { return s.workspaceRoot }

// ServeStdio runs the MCP server over stdio and blocks until ctx is done or
// the client disconnects. Listens on os.Stdin / os.Stdout, which is the
// shape Claude Code attaches to per DESIGN §8.1.
func (s *Server) ServeStdio(ctx context.Context) error {
	stdio := server.NewStdioServer(s.mcp)
	if err := stdio.Listen(ctx, os.Stdin, os.Stdout); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("mcp: stdio listen: %w", err)
	}
	return nil
}

// ServeHTTP runs the MCP server over the streamable-HTTP transport. v0 has
// no authentication; the cmd layer prints a "no auth" warning before
// invoking this. ctx cancellation triggers a graceful shutdown via the
// server's Shutdown method so in-flight requests complete.
func (s *Server) ServeHTTP(ctx context.Context, addr string) error {
	if addr == "" {
		return errors.New("mcp: serve http: empty address")
	}
	httpSrv := server.NewStreamableHTTPServer(s.mcp)
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.Start(addr)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("mcp: http start: %w", err)
		}
		return nil
	case <-ctx.Done():
		// Detach from the canceled parent so Shutdown gets a fresh
		// deadline for the drain — using context.Background() would
		// trip contextcheck.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}
