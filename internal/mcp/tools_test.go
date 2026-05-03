package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/store"
)

// resultText concatenates every TextContent in r into a single string so
// IsError-path assertions can match on the human-readable fallback the SDK
// surfaces alongside the structured payload.
func resultText(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// stubSpawner records calls without forking. Returning a nil *os.Process is
// safe — Server.handle* call sites only dereference it after a non-nil check.
type stubSpawner struct {
	calls []Entry
	err   error
}

func (s *stubSpawner) Start(_ context.Context, e *Entry) (*os.Process, error) {
	if s.err != nil {
		return nil, s.err
	}
	if e != nil {
		s.calls = append(s.calls, *e)
	}
	return nil, nil
}

type stateRecord struct {
	dir   string
	state *store.RunState
	err   error
}

func stateReaderFn(records []stateRecord) StateReader {
	return func(_ context.Context, dir string) (*store.RunState, error) {
		for _, r := range records {
			if r.dir == dir {
				return r.state, r.err
			}
		}
		return nil, store.ErrNotFound
	}
}

func newTestServer(t *testing.T, sp Spawner, sr StateReader) *Server {
	t.Helper()
	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       sp,
		StateReader:   sr,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestHandleListRuns_Empty(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, out, err := srv.handleListRuns(context.Background(), nil, ListRunsArgs{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError")
	}
	if len(out.Runs) != 0 {
		t.Fatalf("len: %d, want 0", len(out.Runs))
	}
}

func TestHandleListRuns_DerivesStatus(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	wsDir := filepath.Join(root, "runs", "alpha")
	stateReader := stateReaderFn([]stateRecord{
		{
			dir: stateDir(wsDir),
			state: &store.RunState{
				Backend: "managed_agents",
				Agents: map[string]store.AgentState{
					"ship-foo": {Status: "running", SignalStatus: "done", SignalSummary: "shipped", SignalPRURL: "https://github.com/x/y/pull/1"},
					"ship-bar": {Status: "running", SignalStatus: "blocked", SignalReason: "ambiguous"},
				},
			},
		},
	})
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   stateReader,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := registry.Put(context.Background(), &Entry{
		RunID: "alpha", WorkspaceDir: wsDir, RepoURL: "https://github.com/x/y",
	}); err != nil {
		t.Fatalf("registry.Put: %v", err)
	}

	res, out, err := srv.handleListRuns(context.Background(), nil, ListRunsArgs{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError")
	}
	if len(out.Runs) != 1 {
		t.Fatalf("runs: %d", len(out.Runs))
	}
	got := out.Runs[0]
	if got.Status != RunStatusBlocked {
		t.Fatalf("status: got %q, want %q", got.Status, RunStatusBlocked)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("teams: %d", len(got.Agents))
	}
}

func TestHandleListRuns_ActiveOnlyFiltersDone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	wsDoneDir := filepath.Join(root, "runs", "done")
	wsRunningDir := filepath.Join(root, "runs", "running")
	stateReader := stateReaderFn([]stateRecord{
		{dir: stateDir(wsDoneDir), state: &store.RunState{
			Backend: "managed_agents",
			Agents:  map[string]store.AgentState{"a": {Status: "running", SignalStatus: "done"}},
		}},
		{dir: stateDir(wsRunningDir), state: &store.RunState{
			Backend: "managed_agents",
			Agents:  map[string]store.AgentState{"a": {Status: "running"}},
		}},
	})
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   stateReader,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, e := range []Entry{
		{RunID: "done", WorkspaceDir: wsDoneDir},
		{RunID: "running", WorkspaceDir: wsRunningDir},
	} {
		entry := e
		if err := registry.Put(context.Background(), &entry); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	_, out, err := srv.handleListRuns(context.Background(), nil, ListRunsArgs{ActiveOnly: true})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(out.Runs) != 1 || out.Runs[0].RunID != "running" {
		ids := make([]string, 0, len(out.Runs))
		for _, r := range out.Runs {
			ids = append(ids, r.RunID)
		}
		t.Fatalf("active_only: got %v, want [running]", ids)
	}
}

func TestHandleGetRun_NotFound(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, _, err := srv.handleGetRun(context.Background(), nil, GetRunArgs{RunID: "ghost"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError, got %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "ghost") {
		t.Fatalf("error text: %q", resultText(res))
	}
}

func TestHandleGetRun_RequiresRunID(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, _, err := srv.handleGetRun(context.Background(), nil, GetRunArgs{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on missing run_id")
	}
}

func TestHandleGetRun_HappyPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	wsDir := filepath.Join(root, "runs", "alpha")
	stateReader := stateReaderFn([]stateRecord{
		{
			dir: stateDir(wsDir),
			state: &store.RunState{
				Backend: "managed_agents",
				Agents: map[string]store.AgentState{
					"ship-foo": {Status: "running", SignalStatus: "done"},
				},
			},
		},
	})
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   stateReader,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := registry.Put(context.Background(), &Entry{RunID: "alpha", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, view, err := srv.handleGetRun(context.Background(), nil, GetRunArgs{RunID: "alpha"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if view.RunID != "alpha" || view.Status != RunStatusDone {
		t.Fatalf("view: %+v", view)
	}
}

// observabilityFixture builds an MCP server returning a single run with a
// fully-populated AgentState. Helper for the v3 RunView / AgentView
// observability tests.
func observabilityFixture(t *testing.T, eventAt time.Time) *Server {
	t.Helper()
	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	wsDir := filepath.Join(root, "runs", "obs")
	stateReader := stateReaderFn([]stateRecord{
		{
			dir: stateDir(wsDir),
			state: &store.RunState{
				Backend:    "managed_agents",
				Phase:      "implementation",
				PhaseIters: map[string]int{"implementation": 2},
				LastError:  "nope",
				Agents: map[string]store.AgentState{
					"engineer": {
						Status:                   "running",
						LastTool:                 "Bash",
						LastEventAt:              eventAt,
						LastError:                "billing_error: out of credits",
						InputTokens:              100,
						OutputTokens:             50,
						CacheCreationInputTokens: 25,
						CacheReadInputTokens:     400,
						ResultSummary:            "shipped PR #42",
						Artifacts:                []string{"design_doc"},
					},
				},
			},
		},
	})
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   stateReader,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := registry.Put(context.Background(), &Entry{RunID: "obs", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return srv
}

// TestHandleGetRun_RunLevelObservability pins the run-level fields the v3
// RunView extension surfaces from state.json (Phase, PhaseIters, LastError).
func TestHandleGetRun_RunLevelObservability(t *testing.T) {
	t.Parallel()

	eventAt := time.Date(2026, 5, 2, 12, 30, 0, 0, time.UTC)
	srv := observabilityFixture(t, eventAt)

	_, view, err := srv.handleGetRun(context.Background(), nil, GetRunArgs{RunID: "obs"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if view.Phase != "implementation" {
		t.Errorf("Phase = %q, want implementation", view.Phase)
	}
	if view.PhaseIters["implementation"] != 2 {
		t.Errorf("PhaseIters = %v", view.PhaseIters)
	}
	if view.LastError != "nope" {
		t.Errorf("LastError = %q", view.LastError)
	}
	// `teams` mirror still populated for v2 wire compat.
	if len(view.Teams) != 1 || view.Teams[0].Name != "engineer" {
		t.Errorf("Teams mirror missing or wrong: %+v", view.Teams)
	}
}

// TestHandleGetRun_AgentViewObservability pins the per-agent fields the v3
// AgentView extension surfaces from state.json (LastTool, LastEventAt,
// LastError, Tokens, ResultSummary, Artifacts).
func TestHandleGetRun_AgentViewObservability(t *testing.T) {
	t.Parallel()

	eventAt := time.Date(2026, 5, 2, 12, 30, 0, 0, time.UTC)
	srv := observabilityFixture(t, eventAt)

	_, view, err := srv.handleGetRun(context.Background(), nil, GetRunArgs{RunID: "obs"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(view.Agents) != 1 {
		t.Fatalf("Agents = %v", view.Agents)
	}
	a := view.Agents[0]
	if a.LastTool != "Bash" {
		t.Errorf("LastTool = %q", a.LastTool)
	}
	if !a.LastEventAt.Equal(eventAt) {
		t.Errorf("LastEventAt = %v, want %v", a.LastEventAt, eventAt)
	}
	if a.LastError != "billing_error: out of credits" {
		t.Errorf("LastError = %q", a.LastError)
	}
	if a.Tokens.InputTokens != 100 || a.Tokens.OutputTokens != 50 {
		t.Errorf("Tokens = %+v", a.Tokens)
	}
	if a.Tokens.CacheCreationInputTokens != 25 || a.Tokens.CacheReadInputTokens != 400 {
		t.Errorf("Cache tokens = %+v", a.Tokens)
	}
	if a.ResultSummary != "shipped PR #42" {
		t.Errorf("ResultSummary = %q", a.ResultSummary)
	}
	if len(a.Artifacts) != 1 || a.Artifacts[0] != "design_doc" {
		t.Errorf("Artifacts = %v", a.Artifacts)
	}
}

func TestRecoverHandler_TranslatesPanicToToolError(t *testing.T) {
	t.Parallel()

	wrapped := recoverHandler(func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		panic("boom")
	})
	res, _, err := wrapped(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("wrapped handler returned protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError result, got %+v", res)
	}
	if !strings.Contains(resultText(res), "boom") {
		t.Fatalf("error text missing panic value: %q", resultText(res))
	}
}

func TestDeriveStatus_PriorityOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		teams []TeamView
		want  string
	}{
		{"empty → running", nil, RunStatusRunning},
		{"all done", []TeamView{{Status: "running", SignalStatus: "done"}, {Status: "running", SignalStatus: "done"}}, RunStatusDone},
		{"any failed wins", []TeamView{{Status: "failed"}, {SignalStatus: "done"}}, RunStatusFailed},
		{"failed beats blocked", []TeamView{{Status: "failed"}, {SignalStatus: "blocked"}}, RunStatusFailed},
		{"failed beats blocked in either order", []TeamView{{SignalStatus: "blocked"}, {Status: "failed"}}, RunStatusFailed},
		{"blocked beats running", []TeamView{{Status: "running", SignalStatus: "blocked"}, {Status: "running", SignalStatus: "done"}}, RunStatusBlocked},
		{"some pending → running", []TeamView{{Status: "running", SignalStatus: "done"}, {Status: "running"}}, RunStatusRunning},
	}
	for _, tc := range cases {
		got := deriveStatus(tc.teams)
		if got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
