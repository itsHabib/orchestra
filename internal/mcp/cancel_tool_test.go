package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
)

func TestHandleCancelRun_RequiresRunID(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, _, err := srv.handleCancelRun(context.Background(), nil, CancelRunArgs{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on missing run_id")
	}
}

func TestHandleCancelRun_NotFound(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, _, err := srv.handleCancelRun(context.Background(), nil, CancelRunArgs{RunID: "ghost"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on unknown run")
	}
}

// TestHandleCancelRun_AlreadyTerminalIsIdempotent verifies that
// cancel_run on a run whose every agent has reached a terminal state
// returns AlreadyDone=true with no signal — the kickoff doc's
// "idempotent, returns gracefully" guarantee.
func TestHandleCancelRun_AlreadyTerminalIsIdempotent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	wsDir := filepath.Join(root, "runs", "done")
	stateReader := stateReaderFn([]stateRecord{
		{
			dir: stateDir(wsDir),
			state: &store.RunState{
				Backend: "managed_agents",
				Agents: map[string]store.AgentState{
					"a": {Status: "done", SignalStatus: "done"},
				},
			},
		},
	})
	srv := newTestServer(t, &stubSpawner{}, stateReader)
	if err := srv.Registry().Put(context.Background(), &Entry{RunID: "done", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, out, err := srv.handleCancelRun(context.Background(), nil, CancelRunArgs{RunID: "done"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if !out.AlreadyDone {
		t.Fatalf("expected AlreadyDone=true on terminal run, got %+v", out)
	}
	if !out.CancelledAt.IsZero() {
		t.Fatalf("CancelledAt should be zero on no-op, got %v", out.CancelledAt)
	}
}

// TestHandleCancelRun_WritesCancellationFile pins the dedicated
// cancellation.json file the engine reads on signal receipt. The MCP
// server writes a separate file (instead of touching state.json) to
// avoid the cross-process read-modify-write race the round-2 review
// caught.
func TestHandleCancelRun_WritesCancellationFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	wsDir := filepath.Join(root, "runs", "live")
	// Seed state.json with one running agent so the run is not terminal.
	fs := filestore.New(stateDir(wsDir))
	seed := &store.RunState{
		Backend: "local",
		RunID:   "live",
		Agents:  map[string]store.AgentState{"a": {Status: "running"}},
	}
	if err := fs.SaveRunState(context.Background(), seed); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	stateReader := stateReaderFn([]stateRecord{
		{dir: stateDir(wsDir), state: seed},
	})
	srv := newTestServer(t, &stubSpawner{}, stateReader)
	if err := srv.Registry().Put(context.Background(), &Entry{
		RunID: "live", WorkspaceDir: wsDir, PID: 0, // PID 0 disables the kill path
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, out, err := srv.handleCancelRun(context.Background(), nil, CancelRunArgs{
		RunID:  "live",
		Reason: "user pressed stop",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if out.CancelledAt.IsZero() {
		t.Fatalf("expected CancelledAt to be set on a fresh cancel, got %+v", out)
	}

	// Confirm cancellation.json (not state.json) carries the request.
	cf, err := readCancellationFile(wsDir)
	if err != nil {
		t.Fatalf("readCancellationFile: %v", err)
	}
	if cf == nil {
		t.Fatal("cancellation.json should exist after cancel_run")
	}
	if cf.Reason != "user pressed stop" {
		t.Fatalf("Reason = %q", cf.Reason)
	}
	if time.Since(cf.RequestedAt) > time.Minute {
		t.Fatalf("RequestedAt = %v, want recent", cf.RequestedAt)
	}
}

func TestRunIsTerminal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		agents map[string]store.AgentState
		want   bool
	}{
		{"empty", nil, false},
		{"all done", map[string]store.AgentState{"a": {Status: "done"}}, true},
		{"all failed", map[string]store.AgentState{"a": {Status: "failed"}}, true},
		{"all canceled", map[string]store.AgentState{"a": {Status: "canceled"}}, true},
		{"mixed terminal", map[string]store.AgentState{
			"a": {Status: "done"},
			"b": {Status: "failed"},
			"c": {Status: "canceled"},
		}, true},
		// Copilot round-2: runIsTerminal must mirror deriveStatus's
		// fold so cancel_run's idempotent short-circuit matches what
		// list_runs / get_run report. SignalStatus="done" or
		// "blocked" with Status still "running" counts as terminal.
		{"signal done while status running", map[string]store.AgentState{
			"a": {Status: "running", SignalStatus: "done"},
		}, true},
		{"signal blocked while status running", map[string]store.AgentState{
			"a": {Status: "running", SignalStatus: "blocked"},
		}, true},
		{"any running without signal", map[string]store.AgentState{
			"a": {Status: "done"},
			"b": {Status: "running"},
		}, false},
		{"any pending", map[string]store.AgentState{"a": {Status: "pending"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runIsTerminal(&store.RunState{Agents: tc.agents})
			if got != tc.want {
				t.Fatalf("runIsTerminal = %v, want %v", got, tc.want)
			}
		})
	}
}
