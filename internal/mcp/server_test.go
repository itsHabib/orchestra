package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptypes "github.com/mark3labs/mcp-go/mcp"

	"github.com/itsHabib/orchestra/internal/store"
)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()

	srv, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if srv == nil {
		t.Fatalf("New: nil server")
	}
	if srv.MCP() == nil {
		t.Fatalf("MCP: nil")
	}
	if srv.Registry() == nil {
		t.Fatalf("Registry: nil")
	}
	if srv.Registry().Path() != DefaultRegistryPath() {
		t.Fatalf("default registry path: got %q, want %q",
			srv.Registry().Path(), DefaultRegistryPath())
	}
	if srv.WorkspaceRoot() != DefaultWorkspaceRoot() {
		t.Fatalf("default workspace root: got %q, want %q",
			srv.WorkspaceRoot(), DefaultWorkspaceRoot())
	}
}

func TestNew_RegistersFourTools(t *testing.T) {
	t.Parallel()

	srv, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools := srv.MCP().ListTools()
	want := []string{
		ToolShipDesignDocs,
		ToolListJobs,
		ToolGetStatus,
		ToolUnblock,
	}
	if len(tools) != len(want) {
		names := make([]string, 0, len(tools))
		for n := range tools {
			names = append(names, n)
		}
		t.Fatalf("tool count: got %d %v, want %d %v", len(tools), names, len(want), want)
	}
	for _, name := range want {
		if _, ok := tools[name]; !ok {
			t.Fatalf("missing tool %q in %v", name, tools)
		}
	}
}

func TestNew_OptionsRespected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	regPath := filepath.Join(root, "custom.json")
	wsRoot := filepath.Join(root, "workspaces")
	srv, err := New(&Options{
		Registry:      NewRegistry(regPath),
		WorkspaceRoot: wsRoot,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.Registry().Path() != regPath {
		t.Fatalf("registry path: got %q, want %q", srv.Registry().Path(), regPath)
	}
	if srv.WorkspaceRoot() != wsRoot {
		t.Fatalf("workspace root: got %q, want %q", srv.WorkspaceRoot(), wsRoot)
	}
}

func TestServeHTTP_RequiresAddress(t *testing.T) {
	t.Parallel()

	srv, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := srv.ServeHTTP(ctx, ""); err == nil {
		t.Fatalf("ServeHTTP empty addr: want error, got nil")
	}
}

func TestServeStdio_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	srv, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Stdio Listen reads from os.Stdin in production; canceling the
	// context immediately is enough to make it return cleanly without
	// a real client. The assertion is that it does not panic and does
	// not wedge.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- srv.ServeStdio(ctx) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("ServeStdio did not return within 2s of cancel")
	}
}

// TestInProcessClient_FullProtocol exercises Initialize → ListTools →
// CallTool over the mark3labs in-process transport. Verifies that the JSON-
// RPC framing, tool definitions, and handler dispatch are wired correctly
// without requiring a forked subprocess. The actual recipe spawn is stubbed
// — we only confirm the protocol path here. The live live-MA scenario
// belongs in test/integration/mcp_smoke/.
func TestInProcessClient_FullProtocol(t *testing.T) {
	t.Parallel()

	srv := newProtocolServer(t)
	client := connectInProcess(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	initializeClient(ctx, t, client)

	assertToolsRegistered(ctx, t, client)
	assertEmptyListJobs(ctx, t, client)
	callShipDesignDocs(ctx, t, client)
	assertOneRunListed(ctx, t, client)
	assertUnblockOnGhostIsError(ctx, t, client)
}

func newProtocolServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	wsDir := filepath.Join(root, "runs", "demo")
	stateReader := stateReaderFn([]stateRecord{
		{
			dir: stateDir(wsDir),
			state: &store.RunState{
				Backend: "managed_agents",
				Teams: map[string]store.TeamState{
					"ship-foo": {
						Status:        "running",
						SessionID:     "sess_test",
						SignalStatus:  "done",
						SignalSummary: "shipped",
						SignalPRURL:   "https://github.com/x/y/pull/1",
					},
				},
			},
		},
	})
	steerCalls := []steerCall{}
	srv, err := New(&Options{
		Registry:      NewRegistry(filepath.Join(root, "mcp-runs.json")),
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   stateReader,
		Steerer:       steererFn(&steerCalls),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func connectInProcess(t *testing.T, srv *Server) *mcpclient.Client {
	t.Helper()
	c, err := mcpclient.NewInProcessClient(srv.MCP())
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func initializeClient(ctx context.Context, t *testing.T, c *mcpclient.Client) {
	t.Helper()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := c.Initialize(ctx, mcptypes.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}

func assertToolsRegistered(ctx context.Context, t *testing.T, c *mcpclient.Client) {
	t.Helper()
	tools, err := c.ListTools(ctx, mcptypes.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		ToolShipDesignDocs: false,
		ToolListJobs:       false,
		ToolGetStatus:      false,
		ToolUnblock:        false,
	}
	for i := range tools.Tools {
		if _, ok := want[tools.Tools[i].Name]; ok {
			want[tools.Tools[i].Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("ListTools missing %q", name)
		}
	}
}

func assertEmptyListJobs(ctx context.Context, t *testing.T, c *mcpclient.Client) {
	t.Helper()
	res, err := c.CallTool(ctx, mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{Name: ToolListJobs},
	})
	if err != nil {
		t.Fatalf("CallTool list_jobs: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_jobs reported error: %s", resultText(res))
	}
}

func callShipDesignDocs(ctx context.Context, t *testing.T, c *mcpclient.Client) {
	t.Helper()
	res, err := c.CallTool(ctx, mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{
			Name: ToolShipDesignDocs,
			Arguments: map[string]any{
				"paths":    []any{"docs/foo.md"},
				"repo_url": "https://github.com/itsHabib/orchestra",
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool ship_design_docs: %v", err)
	}
	if res.IsError {
		t.Fatalf("ship_design_docs reported error: %s", resultText(res))
	}
}

func assertOneRunListed(ctx context.Context, t *testing.T, c *mcpclient.Client) {
	t.Helper()
	res, err := c.CallTool(ctx, mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{Name: ToolListJobs},
	})
	if err != nil {
		t.Fatalf("CallTool list_jobs (post-ship): %v", err)
	}
	if res.IsError {
		t.Fatalf("list_jobs (post-ship) IsError: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "1 run") {
		t.Fatalf("fallback text missing count; text=%q", resultText(res))
	}
}

func assertUnblockOnGhostIsError(ctx context.Context, t *testing.T, c *mcpclient.Client) {
	t.Helper()
	res, err := c.CallTool(ctx, mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{
			Name: ToolUnblock,
			Arguments: map[string]any{
				"run_id":  "ghost",
				"team":    "ship-foo",
				"message": "hi",
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool unblock(ghost): %v", err)
	}
	if !res.IsError {
		t.Fatalf("unblock(ghost) should report IsError; text=%q", resultText(res))
	}
}
