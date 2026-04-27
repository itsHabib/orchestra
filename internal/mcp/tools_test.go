package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcptypes "github.com/mark3labs/mcp-go/mcp"

	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
)

// stubSpawner records calls without forking. Returning a nil *os.Process is
// safe — handleShipDesignDocs only dereferences it after a non-nil check.
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

type steerCall struct {
	sessionID string
	message   string
}

// steererFn returns a Steerer that records calls into out. Tests that need
// the steerer to fail can post-wrap the result.
func steererFn(out *[]steerCall) Steerer {
	return func(_ context.Context, sid, msg string) error {
		*out = append(*out, steerCall{sessionID: sid, message: msg})
		return nil
	}
}

func newTestServer(t *testing.T, sp Spawner, sr StateReader, st Steerer) *Server {
	t.Helper()
	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       sp,
		StateReader:   sr,
		Steerer:       st,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func toolReq(args map[string]any) *mcptypes.CallToolRequest {
	return &mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{
			Arguments: args,
		},
	}
}

func resultText(r *mcptypes.CallToolResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(mcptypes.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestHandleShipDesignDocs_RequiresPaths(t *testing.T) {
	t.Parallel()

	sp := &stubSpawner{}
	srv := newTestServer(t, sp, stateReaderFn(nil), steererFn(&[]steerCall{}))

	res, err := srv.handleShipDesignDocs(context.Background(), toolReq(map[string]any{
		"repo_url": "https://github.com/x/y",
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on missing paths, got %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "doc path") {
		t.Fatalf("error text: %q", resultText(res))
	}
	if len(sp.calls) != 0 {
		t.Fatalf("spawner should not be called on validation failure")
	}
}

func TestHandleShipDesignDocs_RejectsHTTP(t *testing.T) {
	t.Parallel()

	sp := &stubSpawner{}
	srv := newTestServer(t, sp, stateReaderFn(nil), steererFn(&[]steerCall{}))

	res, err := srv.handleShipDesignDocs(context.Background(), toolReq(map[string]any{
		"paths":    []any{"docs/a.md"},
		"repo_url": "http://github.com/x/y",
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on http url, got %s", resultText(res))
	}
}

func TestHandleShipDesignDocs_HappyPath_RegistersEntry(t *testing.T) {
	t.Parallel()

	sp := &stubSpawner{}
	srv := newTestServer(t, sp, stateReaderFn(nil), steererFn(&[]steerCall{}))

	res, err := srv.handleShipDesignDocs(context.Background(), toolReq(map[string]any{
		"paths":              []any{"docs/feat-flag-quiet.md"},
		"repo_url":           "https://github.com/itsHabib/orchestra",
		"default_branch":     "main",
		"open_pull_requests": false,
		"concurrency":        2,
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if len(sp.calls) != 1 {
		t.Fatalf("spawner calls: got %d, want 1", len(sp.calls))
	}
	got := sp.calls[0]
	if got.RunID == "" || got.WorkspaceDir == "" || got.YAMLPath == "" {
		t.Fatalf("entry not populated: %+v", got)
	}
	// yaml file should exist on disk
	if _, statErr := os.Stat(got.YAMLPath); statErr != nil {
		t.Fatalf("yaml not written: %v", statErr)
	}
	// registry should have the run
	all, err := srv.Registry().List(context.Background())
	if err != nil {
		t.Fatalf("Registry.List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("registry size: got %d, want 1", len(all))
	}
}

func TestHandleShipDesignDocs_PropagatesSpawnFailure(t *testing.T) {
	t.Parallel()

	sp := &stubSpawner{err: errors.New("exec: not found")}
	srv := newTestServer(t, sp, stateReaderFn(nil), steererFn(&[]steerCall{}))

	res, err := srv.handleShipDesignDocs(context.Background(), toolReq(map[string]any{
		"paths":    []any{"docs/a.md"},
		"repo_url": "https://github.com/x/y",
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on spawn failure, got %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "exec") {
		t.Fatalf("error text: %q", resultText(res))
	}
}

func TestHandleListJobs_Empty(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil), steererFn(&[]steerCall{}))

	res, err := srv.handleListJobs(context.Background(), toolReq(nil))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	views, ok := res.StructuredContent.([]JobView)
	if !ok {
		t.Fatalf("structured content type: %T", res.StructuredContent)
	}
	if len(views) != 0 {
		t.Fatalf("len: %d, want 0", len(views))
	}
}

func TestHandleListJobs_DerivesStatus(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	wsDir := filepath.Join(root, "runs", "alpha")
	stateReader := stateReaderFn([]stateRecord{
		{
			dir: stateDir(wsDir),
			state: &store.RunState{
				Backend: "managed_agents",
				Teams: map[string]store.TeamState{
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
		Steerer:       steererFn(&[]steerCall{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := registry.Put(context.Background(), &Entry{
		RunID: "alpha", WorkspaceDir: wsDir, RepoURL: "https://github.com/x/y",
	}); err != nil {
		t.Fatalf("registry.Put: %v", err)
	}

	res, err := srv.handleListJobs(context.Background(), toolReq(nil))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	views, ok := res.StructuredContent.([]JobView)
	if !ok {
		t.Fatalf("structured content type: %T", res.StructuredContent)
	}
	if len(views) != 1 {
		t.Fatalf("views: %d", len(views))
	}
	got := views[0]
	if got.Status != RunStatusBlocked {
		t.Fatalf("status: got %q, want %q", got.Status, RunStatusBlocked)
	}
	if len(got.Teams) != 2 {
		t.Fatalf("teams: %d", len(got.Teams))
	}
}

func TestHandleGetStatus_NotFound(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil), steererFn(&[]steerCall{}))

	res, err := srv.handleGetStatus(context.Background(), toolReq(map[string]any{
		"run_id": "ghost",
	}))
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

func TestHandleGetStatus_RequiresRunID(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil), steererFn(&[]steerCall{}))

	res, err := srv.handleGetStatus(context.Background(), toolReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on missing run_id")
	}
}

func TestHandleUnblock_HappyPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	wsDir := filepath.Join(root, "runs", "alpha")

	stateReader := stateReaderFn([]stateRecord{
		{
			dir: stateDir(wsDir),
			state: &store.RunState{
				Backend: "managed_agents",
				Teams: map[string]store.TeamState{
					"ship-foo": {Status: "running", SessionID: "sess_xyz"},
				},
			},
		},
	})
	steerCalls := []steerCall{}
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   stateReader,
		Steerer:       steererFn(&steerCalls),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := registry.Put(context.Background(), &Entry{RunID: "alpha", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := srv.handleUnblock(context.Background(), toolReq(map[string]any{
		"run_id":  "alpha",
		"team":    "ship-foo",
		"message": "make it a --debug bool",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if len(steerCalls) != 1 {
		t.Fatalf("steerer calls: got %d, want 1", len(steerCalls))
	}
	got := steerCalls[0]
	if got.sessionID != "sess_xyz" || got.message != "make it a --debug bool" {
		t.Fatalf("steerer args: %+v", got)
	}
}

func TestHandleUnblock_TeamNotRunning(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	wsDir := filepath.Join(root, "runs", "alpha")

	stateReader := stateReaderFn([]stateRecord{
		{
			dir: stateDir(wsDir),
			state: &store.RunState{
				Backend: "managed_agents",
				Teams: map[string]store.TeamState{
					"ship-foo": {Status: "done", SessionID: "sess_xyz"},
				},
			},
		},
	})
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   stateReader,
		Steerer:       steererFn(&[]steerCall{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := registry.Put(context.Background(), &Entry{RunID: "alpha", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := srv.handleUnblock(context.Background(), toolReq(map[string]any{
		"run_id":  "alpha",
		"team":    "ship-foo",
		"message": "hello",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError when team is done")
	}
	if !strings.Contains(resultText(res), "running") {
		t.Fatalf("error text: %q", resultText(res))
	}
}

func TestHandleUnblock_RejectsLocalBackend(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	wsDir := filepath.Join(root, "runs", "alpha")

	stateReader := stateReaderFn([]stateRecord{
		{
			dir: stateDir(wsDir),
			state: &store.RunState{
				Backend: "local",
				Teams: map[string]store.TeamState{
					"ship-foo": {Status: "running", SessionID: "sess_xyz"},
				},
			},
		},
	})
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   stateReader,
		Steerer:       steererFn(&[]steerCall{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := registry.Put(context.Background(), &Entry{RunID: "alpha", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := srv.handleUnblock(context.Background(), toolReq(map[string]any{
		"run_id":  "alpha",
		"team":    "ship-foo",
		"message": "hello",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on local backend")
	}
}

func TestSteerableSessionID_Sentinels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		st   *store.RunState
		team string
		want error
	}{
		{
			name: "no state",
			st:   nil,
			team: "x",
			want: spawner.ErrNoActiveRun,
		},
		{
			name: "local backend",
			st:   &store.RunState{Backend: "local", Teams: map[string]store.TeamState{}},
			team: "x",
			want: spawner.ErrLocalBackend,
		},
		{
			name: "team missing",
			st:   &store.RunState{Backend: "managed_agents", Teams: map[string]store.TeamState{}},
			team: "x",
			want: spawner.ErrTeamNotFound,
		},
		{
			name: "team not running",
			st: &store.RunState{Backend: "managed_agents", Teams: map[string]store.TeamState{
				"x": {Status: "done"},
			}},
			team: "x",
			want: spawner.ErrTeamNotRunning,
		},
		{
			name: "no session",
			st: &store.RunState{Backend: "managed_agents", Teams: map[string]store.TeamState{
				"x": {Status: "running"},
			}},
			team: "x",
			want: spawner.ErrNoSessionRecorded,
		},
	}
	for _, tc := range cases {
		_, err := steerableSessionID(tc.st, tc.team)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: err=%v, want sentinel=%v", tc.name, err, tc.want)
		}
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
