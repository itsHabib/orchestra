package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	// StdioTransport reads from os.Stdin in production; canceling the
	// context immediately is enough to make Run return cleanly without a
	// real client. The assertion is that it does not panic and does not
	// wedge.
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

// TestInProcessClient_FullProtocol exercises Initialize → ListTools → CallTool
// over the SDK's in-memory transport pair. Verifies that the JSON-RPC framing,
// tool definitions, and handler dispatch are wired correctly without
// requiring a forked subprocess.
func TestInProcessClient_FullProtocol(t *testing.T) {
	t.Parallel()

	srv := newProtocolServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientSession := connectInProcess(ctx, t, srv)

	assertToolsRegistered(ctx, t, clientSession)
	assertEmptyListRuns(ctx, t, clientSession)
	seedRegistry(t, srv)
	assertOneRunListed(ctx, t, clientSession)
	assertGetRunGhostIsError(ctx, t, clientSession)
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
	srv, err := New(&Options{
		Registry:      NewRegistry(filepath.Join(root, "mcp-runs.json")),
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   stateReader,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// seedRegistry registers a single demo run so list_runs / get_run have
// something to return. The workspace dir mirrors the one wired into the
// stateReader stub in newProtocolServer.
func seedRegistry(t *testing.T, srv *Server) {
	t.Helper()
	wsDir := filepath.Join(srv.WorkspaceRoot(), "demo")
	if err := srv.Registry().Put(context.Background(), &Entry{
		RunID:        "demo",
		WorkspaceDir: wsDir,
		RepoURL:      "https://github.com/x/y",
		StartedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("registry.Put: %v", err)
	}
}

func connectInProcess(ctx context.Context, t *testing.T, srv *Server) *mcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	serverSession, err := srv.MCP().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "orchestra-test", Version: "0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func assertToolsRegistered(ctx context.Context, t *testing.T, c *mcp.ClientSession) {
	t.Helper()
	tools, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		ToolListRuns: false,
		ToolGetRun:   false,
	}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("ListTools missing %q", name)
		}
	}
}

func assertEmptyListRuns(ctx context.Context, t *testing.T, c *mcp.ClientSession) {
	t.Helper()
	res, err := c.CallTool(ctx, &mcp.CallToolParams{Name: ToolListRuns})
	if err != nil {
		t.Fatalf("CallTool list_runs: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_runs reported error: %s", resultText(res))
	}
	out, err := decodeListRuns(res)
	if err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	if len(out.Runs) != 0 {
		t.Fatalf("runs: %d, want 0", len(out.Runs))
	}
}

func assertOneRunListed(ctx context.Context, t *testing.T, c *mcp.ClientSession) {
	t.Helper()
	res, err := c.CallTool(ctx, &mcp.CallToolParams{Name: ToolListRuns})
	if err != nil {
		t.Fatalf("CallTool list_runs (post-seed): %v", err)
	}
	if res.IsError {
		t.Fatalf("list_runs IsError: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "1 run") {
		t.Fatalf("fallback text missing count; text=%q", resultText(res))
	}
	out, err := decodeListRuns(res)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Runs) != 1 || out.Runs[0].RunID != "demo" {
		t.Fatalf("runs: %+v", out.Runs)
	}
}

func assertGetRunGhostIsError(ctx context.Context, t *testing.T, c *mcp.ClientSession) {
	t.Helper()
	res, err := c.CallTool(ctx, &mcp.CallToolParams{
		Name:      ToolGetRun,
		Arguments: map[string]any{"run_id": "ghost"},
	})
	if err != nil {
		t.Fatalf("CallTool get_run(ghost): %v", err)
	}
	if !res.IsError {
		t.Fatalf("get_run(ghost) should report IsError; text=%q", resultText(res))
	}
}

// decodeListRuns re-marshals the SDK-decoded interface{} back to JSON and
// then unmarshals it into the typed result. The MCP protocol carries
// structuredContent as JSON; the client-side SDK populates it as an
// untyped map, so a marshal-roundtrip is the cheapest path to a typed view
// and matches what a real chat-side LLM would do for assertions.
func decodeListRuns(r *mcp.CallToolResult) (ListRunsResult, error) {
	var out ListRunsResult
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}
