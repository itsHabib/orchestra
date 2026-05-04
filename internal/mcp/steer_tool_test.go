package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
)

// stubSessionEvents records Send calls without hitting the real API. The
// SessionEventSender interface is the same one the production MA spawner
// uses, so the tests exercise the actual steer path through SendUserMessage.
type stubSessionEvents struct {
	mu    sync.Mutex
	sends []stubSend
	err   error
}

type stubSend struct {
	sessionID string
	params    anthropic.BetaSessionEventSendParams
}

func (s *stubSessionEvents) Send(_ context.Context, sessionID string, params anthropic.BetaSessionEventSendParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsSendSessionEvents, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	s.sends = append(s.sends, stubSend{sessionID: sessionID, params: params})
	return &anthropic.BetaManagedAgentsSendSessionEvents{}, nil
}

func (s *stubSessionEvents) calls() []stubSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stubSend, len(s.sends))
	copy(out, s.sends)
	return out
}

func steerTestServer(t *testing.T, sr StateReader, evs *stubSessionEvents) *Server {
	t.Helper()
	root := t.TempDir()
	registry := NewRegistry(filepath.Join(root, "mcp-runs.json"))
	srv, err := New(&Options{
		Registry:      registry,
		WorkspaceRoot: filepath.Join(root, "runs"),
		Spawner:       &stubSpawner{},
		StateReader:   sr,
		SessionEvents: func(context.Context) (spawner.SessionEventSender, error) {
			return evs, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestHandleSteer_ValidatesArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args SteerArgs
		want string
	}{
		{"missing run_id", SteerArgs{}, "run_id is required"},
		{"missing agent", SteerArgs{RunID: "r"}, "agent is required"},
		{"missing content", SteerArgs{RunID: "r", Agent: "a"}, "content is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := steerTestServer(t, stateReaderFn(nil), &stubSessionEvents{})
			res, _, err := srv.handleSteer(context.Background(), nil, tc.args)
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected IsError")
			}
			if !strings.Contains(resultText(res), tc.want) {
				t.Fatalf("text = %q, want contains %q", resultText(res), tc.want)
			}
		})
	}
}

func TestHandleSteer_RunNotFound(t *testing.T) {
	t.Parallel()
	srv := steerTestServer(t, stateReaderFn(nil), &stubSessionEvents{})
	res, _, err := srv.handleSteer(context.Background(), nil, SteerArgs{RunID: "ghost", Agent: "a", Content: "hi"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError")
	}
	if !strings.Contains(resultText(res), "ghost") {
		t.Fatalf("text = %q", resultText(res))
	}
}

func TestHandleSteer_LocalBackendDocumentedError(t *testing.T) {
	t.Parallel()
	wsDir := filepath.Join(t.TempDir(), "ws")
	sr := stateReaderFn([]stateRecord{{
		dir: stateDir(wsDir),
		state: &store.RunState{
			Backend: "local",
			Agents:  map[string]store.AgentState{"alpha": {Status: "running", SessionID: "sess_x"}},
		},
	}})
	evs := &stubSessionEvents{}
	srv := steerTestServer(t, sr, evs)
	if err := srv.Registry().Put(context.Background(), &Entry{RunID: "r", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, _, err := srv.handleSteer(context.Background(), nil, SteerArgs{RunID: "r", Agent: "alpha", Content: "go"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on local backend")
	}
	if !strings.Contains(resultText(res), "local steering not supported") {
		t.Fatalf("text should name the v3 limitation, got %q", resultText(res))
	}
	if len(evs.calls()) != 0 {
		t.Errorf("local backend should not call SendUserMessage; got %d calls", len(evs.calls()))
	}
}

func TestHandleSteer_AgentNotRunning(t *testing.T) {
	t.Parallel()
	wsDir := filepath.Join(t.TempDir(), "ws")
	sr := stateReaderFn([]stateRecord{{
		dir: stateDir(wsDir),
		state: &store.RunState{
			Backend: "managed_agents",
			Agents:  map[string]store.AgentState{"alpha": {Status: "done", SignalStatus: "done"}},
		},
	}})
	srv := steerTestServer(t, sr, &stubSessionEvents{})
	if err := srv.Registry().Put(context.Background(), &Entry{RunID: "r", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, _, err := srv.handleSteer(context.Background(), nil, SteerArgs{RunID: "r", Agent: "alpha", Content: "go"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError when agent not running")
	}
	if !strings.Contains(resultText(res), "not steerable") {
		t.Fatalf("text = %q", resultText(res))
	}
}

func TestHandleSteer_AgentMissing(t *testing.T) {
	t.Parallel()
	wsDir := filepath.Join(t.TempDir(), "ws")
	sr := stateReaderFn([]stateRecord{{
		dir: stateDir(wsDir),
		state: &store.RunState{
			Backend: "managed_agents",
			Agents:  map[string]store.AgentState{"alpha": {Status: "running", SessionID: "sess_x"}},
		},
	}})
	srv := steerTestServer(t, sr, &stubSessionEvents{})
	if err := srv.Registry().Put(context.Background(), &Entry{RunID: "r", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, _, err := srv.handleSteer(context.Background(), nil, SteerArgs{RunID: "r", Agent: "ghost", Content: "go"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError when agent missing")
	}
	if !strings.Contains(resultText(res), "ghost") {
		t.Fatalf("text should name the missing agent, got %q", resultText(res))
	}
}

func TestHandleSteer_HappyPath(t *testing.T) {
	t.Parallel()
	wsDir := filepath.Join(t.TempDir(), "ws")
	sr := stateReaderFn([]stateRecord{{
		dir: stateDir(wsDir),
		state: &store.RunState{
			Backend: "managed_agents",
			Agents:  map[string]store.AgentState{"alpha": {Status: "running", SessionID: "sess_alpha"}},
		},
	}})
	evs := &stubSessionEvents{}
	srv := steerTestServer(t, sr, evs)
	if err := srv.Registry().Put(context.Background(), &Entry{RunID: "r", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, out, err := srv.handleSteer(context.Background(), nil, SteerArgs{
		RunID: "r", Agent: "alpha", Content: "shift to plan B",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if out.Agent != "alpha" || out.RunID != "r" || out.Backend != "managed_agents" {
		t.Errorf("result = %+v", out)
	}
	calls := evs.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 Send call, got %d", len(calls))
	}
	if calls[0].sessionID != "sess_alpha" {
		t.Errorf("session id = %q, want sess_alpha", calls[0].sessionID)
	}
}

func TestHandleSteer_PropagatesSendError(t *testing.T) {
	t.Parallel()
	wsDir := filepath.Join(t.TempDir(), "ws")
	sr := stateReaderFn([]stateRecord{{
		dir: stateDir(wsDir),
		state: &store.RunState{
			Backend: "managed_agents",
			Agents:  map[string]store.AgentState{"alpha": {Status: "running", SessionID: "sess_x"}},
		},
	}})
	evs := &stubSessionEvents{err: errors.New("upstream 401")}
	srv := steerTestServer(t, sr, evs)
	if err := srv.Registry().Put(context.Background(), &Entry{RunID: "r", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, _, err := srv.handleSteer(context.Background(), nil, SteerArgs{RunID: "r", Agent: "alpha", Content: "x"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on send failure")
	}
	if !strings.Contains(resultText(res), "401") {
		t.Fatalf("error should surface the underlying message, got %q", resultText(res))
	}
}
