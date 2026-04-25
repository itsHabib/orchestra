package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
)

// recordingEvents is the cmd-level fake — distinct from the one inside
// internal/spawner because that one is package-scoped (unexported types).
type recordingEvents struct {
	calls       int
	failUntil   int
	failErr     error
	lastSession string
	lastParams  anthropic.BetaSessionEventSendParams
}

func (c *recordingEvents) Send(_ context.Context, sessionID string, params anthropic.BetaSessionEventSendParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsSendSessionEvents, error) {
	c.calls++
	c.lastSession = sessionID
	c.lastParams = params
	if c.calls <= c.failUntil {
		return nil, c.failErr
	}
	return &anthropic.BetaManagedAgentsSendSessionEvents{}, nil
}

// recordingEvents implements spawner.SessionEventSender — the narrow
// steering interface — so the test does not depend on the broader
// (and partly unexported) streaming surface.

func writeStateJSON(t *testing.T, dir string, state *store.RunState) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func swapSteeringFactory(t *testing.T, fn func(context.Context) (spawner.SessionEventSender, error)) {
	t.Helper()
	prev := steeringSessionEventsFactory
	steeringSessionEventsFactory = fn
	t.Cleanup(func() { steeringSessionEventsFactory = prev })
}

func resetMsgFlags(t *testing.T) {
	t.Helper()
	prevWS, prevTeam, prevMsg, prevNo := msgWorkspaceFlag, msgTeamFlag, msgMessageFlag, msgNoRetryFlag
	t.Cleanup(func() {
		msgWorkspaceFlag = prevWS
		msgTeamFlag = prevTeam
		msgMessageFlag = prevMsg
		msgNoRetryFlag = prevNo
	})
}

func resetInterruptFlags(t *testing.T) {
	t.Helper()
	prevWS, prevTeam := interruptWorkspaceFlag, interruptTeamFlag
	t.Cleanup(func() {
		interruptWorkspaceFlag = prevWS
		interruptTeamFlag = prevTeam
	})
}

func resetSessionsFlags(t *testing.T) {
	t.Helper()
	prevWS, prevAll := sessionsWorkspaceFlag, sessionsLsAllFlag
	t.Cleanup(func() {
		sessionsWorkspaceFlag = prevWS
		sessionsLsAllFlag = prevAll
	})
}

func TestRunMsg_HappyPath(t *testing.T) {
	resetMsgFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	writeStateJSON(t, ws, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Teams: map[string]store.TeamState{
			"alpha": {Status: "running", SessionID: "sess_42"},
		},
	})
	rec := &recordingEvents{}
	swapSteeringFactory(t, func(context.Context) (spawner.SessionEventSender, error) { return rec, nil })

	msgWorkspaceFlag = ws
	msgTeamFlag = "alpha"
	msgMessageFlag = "use the JSON store"

	if err := runMsg(context.Background()); err != nil {
		t.Fatalf("runMsg: %v", err)
	}
	if rec.calls != 1 || rec.lastSession != "sess_42" {
		t.Fatalf("rec=%+v, want one call against sess_42", rec)
	}
	if rec.lastParams.Events[0].OfUserMessage == nil {
		t.Fatal("OfUserMessage missing")
	}
	if got := rec.lastParams.Events[0].OfUserMessage.Content[0].OfText.Text; got != "use the JSON store" {
		t.Fatalf("text=%q, want 'use the JSON store'", got)
	}
}

func TestRunMsg_NoActiveRunReturnsSentinel(t *testing.T) {
	resetMsgFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	swapSteeringFactory(t, func(context.Context) (spawner.SessionEventSender, error) {
		t.Fatal("session client should not be constructed when no active run")
		return nil, nil
	})

	msgWorkspaceFlag = ws
	msgTeamFlag = "alpha"
	msgMessageFlag = "hi"
	err := runMsg(context.Background())
	if !errors.Is(err, spawner.ErrNoActiveRun) {
		t.Fatalf("err=%v, want ErrNoActiveRun", err)
	}
}

func TestRunMsg_TeamNotFound(t *testing.T) {
	resetMsgFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	writeStateJSON(t, ws, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Teams:   map[string]store.TeamState{"alpha": {Status: "running", SessionID: "sess_42"}},
	})
	swapSteeringFactory(t, func(context.Context) (spawner.SessionEventSender, error) {
		t.Fatal("session client should not be constructed when team is missing")
		return nil, nil
	})

	msgWorkspaceFlag = ws
	msgTeamFlag = "ghost"
	msgMessageFlag = "hi"
	err := runMsg(context.Background())
	if !errors.Is(err, spawner.ErrTeamNotFound) {
		t.Fatalf("err=%v, want ErrTeamNotFound", err)
	}
}

func TestRunMsg_TeamNotRunning(t *testing.T) {
	resetMsgFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	writeStateJSON(t, ws, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Teams:   map[string]store.TeamState{"alpha": {Status: "done", SessionID: "sess_42"}},
	})
	swapSteeringFactory(t, func(context.Context) (spawner.SessionEventSender, error) {
		t.Fatal("session client should not be constructed for non-running team")
		return nil, nil
	})

	msgWorkspaceFlag = ws
	msgTeamFlag = "alpha"
	msgMessageFlag = "hi"
	err := runMsg(context.Background())
	if !errors.Is(err, spawner.ErrTeamNotRunning) {
		t.Fatalf("err=%v, want ErrTeamNotRunning", err)
	}
	if !strings.Contains(err.Error(), `"done"`) {
		t.Fatalf("err=%v, want current status quoted", err)
	}
}

func TestRunMsg_NoSessionRecorded(t *testing.T) {
	resetMsgFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	writeStateJSON(t, ws, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Teams:   map[string]store.TeamState{"alpha": {Status: "running"}},
	})
	swapSteeringFactory(t, func(context.Context) (spawner.SessionEventSender, error) {
		t.Fatal("session client should not be constructed without recorded session")
		return nil, nil
	})

	msgWorkspaceFlag = ws
	msgTeamFlag = "alpha"
	msgMessageFlag = "hi"
	err := runMsg(context.Background())
	if !errors.Is(err, spawner.ErrNoSessionRecorded) {
		t.Fatalf("err=%v, want ErrNoSessionRecorded", err)
	}
}

func TestRunMsg_LocalBackendBlocked(t *testing.T) {
	resetMsgFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	writeStateJSON(t, ws, &store.RunState{
		Project: "p",
		Backend: "local",
		Teams:   map[string]store.TeamState{"alpha": {Status: "running", SessionID: "sess_42"}},
	})
	swapSteeringFactory(t, func(context.Context) (spawner.SessionEventSender, error) {
		t.Fatal("session client should not be constructed for local backend")
		return nil, nil
	})

	msgWorkspaceFlag = ws
	msgTeamFlag = "alpha"
	msgMessageFlag = "hi"
	err := runMsg(context.Background())
	if !errors.Is(err, spawner.ErrLocalBackend) {
		t.Fatalf("err=%v, want ErrLocalBackend", err)
	}
}

func TestRunInterrupt_HappyPath(t *testing.T) {
	resetInterruptFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	writeStateJSON(t, ws, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Teams:   map[string]store.TeamState{"alpha": {Status: "running", SessionID: "sess_42"}},
	})
	rec := &recordingEvents{}
	swapSteeringFactory(t, func(context.Context) (spawner.SessionEventSender, error) { return rec, nil })

	interruptWorkspaceFlag = ws
	interruptTeamFlag = "alpha"

	if err := runInterrupt(context.Background()); err != nil {
		t.Fatalf("runInterrupt: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("calls=%d, want 1", rec.calls)
	}
	if rec.lastParams.Events[0].OfUserInterrupt == nil {
		t.Fatal("OfUserInterrupt missing")
	}
}

func TestRunSessionsLs_FiltersToSteerableByDefault(t *testing.T) {
	resetSessionsFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	writeStateJSON(t, ws, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Teams: map[string]store.TeamState{
			"alpha":   {Status: "running", SessionID: "sess_a", AgentID: "agent_a", LastEventID: "e1", LastEventAt: time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)},
			"bravo":   {Status: "pending"},
			"charlie": {Status: "done", SessionID: "sess_c"},
			"delta":   {Status: "failed"},
		},
	})

	sessionsWorkspaceFlag = ws
	sessionsLsAllFlag = false

	var buf bytes.Buffer
	if err := runSessionsLs(context.Background(), &buf); err != nil {
		t.Fatalf("runSessionsLs: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "alpha") {
		t.Fatalf("output missing steerable team alpha:\n%s", out)
	}
	for _, name := range []string{"bravo", "charlie", "delta"} {
		if strings.Contains(out, name) {
			t.Fatalf("default output should hide non-steerable team %q:\n%s", name, out)
		}
	}
}

func TestRunSessionsLs_AllShowsEverything(t *testing.T) {
	resetSessionsFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	writeStateJSON(t, ws, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Teams: map[string]store.TeamState{
			"alpha": {Status: "running", SessionID: "sess_a"},
			"bravo": {Status: "done", SessionID: "sess_b"},
			"delta": {Status: "failed"},
		},
	})

	sessionsWorkspaceFlag = ws
	sessionsLsAllFlag = true

	var buf bytes.Buffer
	if err := runSessionsLs(context.Background(), &buf); err != nil {
		t.Fatalf("runSessionsLs: %v", err)
	}
	out := buf.String()
	for _, name := range []string{"alpha", "bravo", "delta"} {
		if !strings.Contains(out, name) {
			t.Fatalf("--all output missing team %q:\n%s", name, out)
		}
	}
}

func TestRunSessionsLs_NoActiveRunExitsZeroEmpty(t *testing.T) {
	resetSessionsFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	sessionsWorkspaceFlag = ws

	var buf bytes.Buffer
	if err := runSessionsLs(context.Background(), &buf); err != nil {
		t.Fatalf("runSessionsLs: %v", err)
	}
	if !strings.Contains(buf.String(), "TEAM") {
		t.Fatalf("expected header even when empty:\n%s", buf.String())
	}
}

func TestRunSessionsLs_LocalBackendBlocked(t *testing.T) {
	resetSessionsFlags(t)
	ws := filepath.Join(t.TempDir(), ".orchestra")
	writeStateJSON(t, ws, &store.RunState{
		Project: "p",
		Backend: "local",
		Teams:   map[string]store.TeamState{"alpha": {Status: "running", SessionID: "sess_a"}},
	})
	sessionsWorkspaceFlag = ws

	var buf bytes.Buffer
	err := runSessionsLs(context.Background(), &buf)
	if !errors.Is(err, spawner.ErrLocalBackend) {
		t.Fatalf("err=%v, want ErrLocalBackend", err)
	}
}
