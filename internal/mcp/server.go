package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/artifacts"
)

// ServerName and ServerVersion advertise the MCP server identity to clients
// during the initialize handshake. Bumping ServerVersion when the tool surface
// changes is the cheapest signal to the parent Claude that a new capability
// landed.
const (
	ServerName    = "orchestra"
	ServerVersion = "0.5.0"
)

// shutdownGrace caps how long ServeHTTP waits for in-flight requests to drain
// after ctx cancellation before forcing the server down. Keep small — the
// parent Claude is expected to retry on transient errors.
const shutdownGrace = 5 * time.Second

// Server bundles the MCP protocol implementation, the run registry, and the
// pluggable subprocess + state-read dependencies. Construct via New; do not
// zero-value a Server.
type Server struct {
	mcp           *mcp.Server
	registry      *Registry
	spawner       Spawner
	stateReader   StateReader
	artifactStore ArtifactStoreFactory
	sessionEvents SessionEventsFactory
	workspaceRoot string
}

// ArtifactStoreFactory returns an artifacts.Store rooted at artifactsDir.
// Production callers wire DefaultArtifactStore (a thin wrapper around
// artifacts.NewFileStore); tests pass a stub that returns an in-memory or
// pre-seeded store.
type ArtifactStoreFactory func(artifactsDir string) artifacts.Store

// DefaultArtifactStore is the production [ArtifactStoreFactory]. It returns
// a [artifacts.FileStore] rooted at artifactsDir.
func DefaultArtifactStore(artifactsDir string) artifacts.Store {
	return artifacts.NewFileStore(artifactsDir)
}

// Options configures a Server. Zero-value fields fall back to the production
// defaults: registry at DefaultRegistryPath, workspace root at
// DefaultWorkspaceRoot, ExecSpawner with its default binary lookup, and
// DefaultStateReader.
//
// Tests inject stubs by setting the fields explicitly.
type Options struct {
	Registry      *Registry
	WorkspaceRoot string
	Spawner       Spawner
	StateReader   StateReader
	ArtifactStore ArtifactStoreFactory
	SessionEvents SessionEventsFactory
}

// New returns a Server with the v1 generic tool surface registered against
// the embedded SDK server. The returned value is ready for ServeStdio /
// ServeHTTP.
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
	artStore := opts.ArtifactStore
	if artStore == nil {
		artStore = DefaultArtifactStore
	}
	sessEvents := opts.SessionEvents
	if sessEvents == nil {
		sessEvents = DefaultSessionEventsFactory
	}

	s := &Server{
		mcp:           mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: ServerVersion}, nil),
		registry:      registry,
		spawner:       spawn,
		stateReader:   read,
		artifactStore: artStore,
		sessionEvents: sessEvents,
		workspaceRoot: root,
	}
	s.registerTools()
	s.registerResources()
	return s, nil
}

// MCP exposes the underlying SDK server. Useful for tests that drive the tool
// layer through an in-memory transport without forking a subprocess.
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Registry exposes the run registry. Callers should treat it read-only; the
// server is the canonical writer.
func (s *Server) Registry() *Registry { return s.registry }

// WorkspaceRoot exposes the configured root directory under which each new
// run gets a workspace at <root>/<run_id>/. Diagnostic-only.
func (s *Server) WorkspaceRoot() string { return s.workspaceRoot }

// ServeStdio runs the MCP server over stdio and blocks until ctx is done or
// the client disconnects. The SDK's StdioTransport reads from os.Stdin and
// writes to os.Stdout, which is the shape Claude Code attaches to.
func (s *Server) ServeStdio(ctx context.Context) error {
	if err := s.mcp.Run(ctx, &mcp.StdioTransport{}); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("mcp: stdio run: %w", err)
	}
	return nil
}

// ServeHTTP runs the MCP server over the streamable-HTTP transport. v0 has
// no authentication; the cmd layer prints a "no auth" warning before invoking
// this. ctx cancellation triggers a graceful shutdown via the http.Server's
// Shutdown method so in-flight requests complete.
func (s *Server) ServeHTTP(ctx context.Context, addr string) error {
	if addr == "" {
		return errors.New("mcp: serve http: empty address")
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp }, nil)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("mcp: http listen: %w", err)
		}
		return nil
	case <-ctx.Done():
		// Detach from the canceled parent so Shutdown gets a fresh
		// deadline for the drain — using the canceled ctx directly would
		// trip contextcheck and skip the drain entirely.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("mcp: http shutdown: %w", err)
		}
		return nil
	}
}
