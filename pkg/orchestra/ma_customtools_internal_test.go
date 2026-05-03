package orchestra

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/customtools"
	"github.com/itsHabib/orchestra/internal/event"
	"github.com/itsHabib/orchestra/internal/notify"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/memstore"
	"github.com/itsHabib/orchestra/internal/workspace"
)

// recordingHandler captures the inputs each Handle call receives so the
// dispatch test can assert the engine threaded the right team, run id, and
// tool input through to the handler.
type recordingHandler struct {
	def        customtools.Definition
	mu         sync.Mutex
	handleOpts []recordedHandle
	result     json.RawMessage
	err        error
}

type recordedHandle struct {
	team  string
	input json.RawMessage
	runID string
}

func (h *recordingHandler) Tool() customtools.Definition { return h.def }
func (h *recordingHandler) Handle(_ context.Context, run *customtools.RunContext, team string, input json.RawMessage) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handleOpts = append(h.handleOpts, recordedHandle{team: team, input: append(json.RawMessage(nil), input...), runID: run.RunID})
	if h.err != nil {
		return nil, h.err
	}
	return h.result, nil
}

func (h *recordingHandler) recorded() []recordedHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]recordedHandle, len(h.handleOpts))
	copy(out, h.handleOpts)
	return out
}

// runWithCustomToolEvents seeds the run-state, registers the supplied
// handler, swaps the MA team starter for a stub that emits the given events,
// and drives one team through runTeamMA.
func runWithCustomToolEvents(t *testing.T, h customtools.Handler, eventsToEmit ...spawner.Event) (*orchestrationRun, *fakeManagedSession) {
	t.Helper()
	customtools.Reset()
	if err := customtools.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(customtools.Reset)

	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{
		Project: "p",
		RunID:   "run_test",
		Backend: BackendManagedAgents,
		Agents:  map[string]store.AgentState{"alpha": {Status: "pending"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatalf("ws: %v", err)
	}

	cfg := &Config{
		Name:     "p",
		Backend:  Backend{Kind: BackendManagedAgents},
		Defaults: Defaults{TimeoutMinutes: 5},
		Agents: []Team{
			{Name: "alpha", Lead: Lead{Role: "Lead"}, Tasks: []Task{{Summary: "x"}}},
		},
	}
	cfg.ResolveDefaults()

	fake := &fakeManagedSession{id: "sess_alpha"}
	r := &orchestrationRun{
		cfg:        cfg,
		emitter:    event.NoopEmitter{},
		runService: runsvc.New(st),
		ws:         ws,
	}
	r.startTeamMAForTest = func(_ context.Context, _ *Team, _ *store.RunState, _ io.Writer) (managedSession, <-chan spawner.Event, error) {
		ch := make(chan spawner.Event, len(eventsToEmit))
		for _, ev := range eventsToEmit {
			ch <- ev
		}
		// Mark the team done so finalizeMATeam doesn't fail the run; the
		// dispatch path is what we're exercising, not the lifecycle finisher.
		if err := st.UpdateAgentState(ctx, "alpha", func(ts *store.AgentState) {
			ts.Status = "done"
			ts.ResultSummary = "ok"
		}); err != nil {
			t.Fatalf("seed done: %v", err)
		}
		close(ch)
		return fake, ch, nil
	}
	return r, fake
}

func TestDispatchCustomToolUseInvokesHandlerAndRelaysResult(t *testing.T) {
	ctx := context.Background()
	want := json.RawMessage(`{"ok":true}`)
	h := &recordingHandler{
		def: customtools.Definition{
			Name:        "echo",
			Description: "echo the input",
			InputSchema: map[string]any{"type": "object"},
		},
		result: want,
	}
	r, fake := runWithCustomToolEvents(t, h,
		spawner.AgentCustomToolUseEvent{
			BaseEvent: spawner.BaseEvent{ID: "evt_1", Type: spawner.EventTypeAgentCustomToolUse},
			ToolUse: spawner.ToolUse{
				ID:    "evt_1",
				Name:  "echo",
				Input: map[string]any{"hello": "world"},
			},
		},
	)
	if _, err := r.runTeamMA(ctx, 0, &r.cfg.Agents[0], &store.RunState{RunID: "run_test"}); err != nil {
		t.Fatalf("runTeamMA: %v", err)
	}

	rec := h.recorded()
	if len(rec) != 1 {
		t.Fatalf("expected one Handle call, got %d", len(rec))
	}
	if rec[0].team != "alpha" {
		t.Fatalf("team: want alpha got %s", rec[0].team)
	}
	if rec[0].runID != "run_test" {
		t.Fatalf("run_id: want run_test got %q", rec[0].runID)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec[0].input, &decoded); err != nil {
		t.Fatalf("parse handler input: %v", err)
	}
	if decoded["hello"] != "world" {
		t.Fatalf("handler input did not contain expected key/value: %v", decoded)
	}

	sent := fake.sentEvents()
	if len(sent) != 1 {
		t.Fatalf("expected one Send to relay result, got %d", len(sent))
	}
	if sent[0].Type != spawner.UserEventTypeCustomToolResult {
		t.Fatalf("wrong send type: %v", sent[0].Type)
	}
	if sent[0].CustomToolResult == nil {
		t.Fatalf("CustomToolResult is nil")
	}
	if sent[0].CustomToolResult.ToolUseID != "evt_1" {
		t.Fatalf("tool use id mismatch: %s", sent[0].CustomToolResult.ToolUseID)
	}
	gotResult, ok := sent[0].CustomToolResult.Result.(json.RawMessage)
	if !ok {
		t.Fatalf("result type: want json.RawMessage got %T", sent[0].CustomToolResult.Result)
	}
	if !equalJSON(t, gotResult, want) {
		t.Fatalf("result mismatch: got=%s want=%s", gotResult, want)
	}
	if sent[0].CustomToolResult.Error != "" {
		t.Fatalf("expected no error, got %s", sent[0].CustomToolResult.Error)
	}
}

func TestDispatchCustomToolUseHandlerErrorBecomesIsErrorResult(t *testing.T) {
	ctx := context.Background()
	h := &recordingHandler{
		def: customtools.Definition{
			Name:        "explode",
			InputSchema: map[string]any{"type": "object"},
		},
	}
	h.err = intentionalError("boom")
	r, fake := runWithCustomToolEvents(t, h,
		spawner.AgentCustomToolUseEvent{
			BaseEvent: spawner.BaseEvent{ID: "evt_x", Type: spawner.EventTypeAgentCustomToolUse},
			ToolUse:   spawner.ToolUse{ID: "evt_x", Name: "explode"},
		},
	)
	if _, err := r.runTeamMA(ctx, 0, &r.cfg.Agents[0], &store.RunState{RunID: "run_test"}); err != nil {
		t.Fatalf("runTeamMA: %v", err)
	}

	sent := fake.sentEvents()
	if len(sent) != 1 || sent[0].CustomToolResult == nil {
		t.Fatalf("expected exactly one custom_tool_result send: %+v", sent)
	}
	if sent[0].CustomToolResult.Error == "" || !strings.Contains(sent[0].CustomToolResult.Error, "boom") {
		t.Fatalf("error not propagated: %+v", sent[0].CustomToolResult)
	}
}

func TestDispatchUnknownToolRelaysIsError(t *testing.T) {
	customtools.Reset()
	t.Cleanup(customtools.Reset)
	ctx := context.Background()
	// Register a different tool so Lookup miss path is exercised.
	if err := customtools.Register(&recordingHandler{def: customtools.Definition{Name: "other"}}); err != nil {
		t.Fatalf("seed register: %v", err)
	}
	r, fake := runWithCustomToolEventsRaw(t,
		spawner.AgentCustomToolUseEvent{
			BaseEvent: spawner.BaseEvent{ID: "evt_y", Type: spawner.EventTypeAgentCustomToolUse},
			ToolUse:   spawner.ToolUse{ID: "evt_y", Name: "ghost"},
		},
	)
	if _, err := r.runTeamMA(ctx, 0, &r.cfg.Agents[0], &store.RunState{RunID: "run_test"}); err != nil {
		t.Fatalf("runTeamMA: %v", err)
	}
	sent := fake.sentEvents()
	if len(sent) != 1 || sent[0].CustomToolResult == nil {
		t.Fatalf("expected one is_error result, got %+v", sent)
	}
	if !strings.Contains(sent[0].CustomToolResult.Error, "no handler registered") {
		t.Fatalf("missing unknown-tool error: %+v", sent[0].CustomToolResult)
	}
}

type endToEndFixture struct {
	state         store.AgentState
	sent          []*spawner.UserEvent
	notifications []notify.Notification
	logPath       string
}

// runEndToEndSignal seeds the end-to-end signal_completion happy path: an MA
// stub emits one agent.custom_tool_use(signal_completion) event, the engine
// dispatches it through the registered handler, and the fixture captures the
// resulting state, send echoes, and notifications.
func runEndToEndSignal(t *testing.T) endToEndFixture {
	t.Helper()
	customtools.Reset()
	t.Cleanup(customtools.Reset)
	customtools.MustRegister(customtools.NewSignalCompletion())

	ctx := context.Background()
	st := seedAlphaRun(t)
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	cfg := alphaSingleTeamCfg()

	notifyPath := filepath.Join(ws.Path, "notifications.ndjson")
	captured := make([]notify.Notification, 0, 1)
	logSink := notify.NotifierFunc(func(_ context.Context, n *notify.Notification) error {
		if n != nil {
			captured = append(captured, *n)
		}
		return nil
	})

	fake := &fakeManagedSession{id: "sess_alpha"}
	r := &orchestrationRun{
		cfg:        cfg,
		emitter:    event.NoopEmitter{},
		runService: runsvc.New(st),
		ws:         ws,
		notifier:   notify.Compose(nil, logSink, notify.NewLog(notifyPath)),
	}
	r.startTeamMAForTest = endToEndStarter(ctx, t, st, fake)
	if _, err := r.runTeamMA(ctx, 0, &cfg.Agents[0], &store.RunState{RunID: "run_test"}); err != nil {
		t.Fatalf("runTeamMA: %v", err)
	}
	state, err := st.LoadRunState(ctx)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return endToEndFixture{
		state:         state.Agents["alpha"],
		sent:          fake.sentEvents(),
		notifications: captured,
		logPath:       notifyPath,
	}
}

func seedAlphaRun(t *testing.T) *memstore.MemStore {
	t.Helper()
	st := memstore.New()
	if err := st.SaveRunState(context.Background(), &store.RunState{
		Project: "p",
		RunID:   "run_test",
		Backend: BackendManagedAgents,
		Agents:  map[string]store.AgentState{"alpha": {Status: "pending"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return st
}

func alphaSingleTeamCfg() *Config {
	cfg := &Config{
		Name:     "p",
		Backend:  Backend{Kind: BackendManagedAgents},
		Defaults: Defaults{TimeoutMinutes: 5},
		Agents: []Team{
			{Name: "alpha", Lead: Lead{Role: "Lead"}, Tasks: []Task{{Summary: "x"}}},
		},
	}
	cfg.ResolveDefaults()
	return cfg
}

//nolint:contextcheck // ctx is the ambient orchestrator context the closure inherits, not a request-scoped value.
func endToEndStarter(ctx context.Context, t *testing.T, st *memstore.MemStore, fake *fakeManagedSession) startTeamMAFunc {
	t.Helper()
	return func(_ context.Context, _ *Team, _ *store.RunState, _ io.Writer) (managedSession, <-chan spawner.Event, error) {
		ch := make(chan spawner.Event, 1)
		ch <- spawner.AgentCustomToolUseEvent{
			BaseEvent: spawner.BaseEvent{ID: "evt_signal", Type: spawner.EventTypeAgentCustomToolUse, ProcessedAt: time.Now()},
			ToolUse: spawner.ToolUse{
				ID:   "evt_signal",
				Name: customtools.SignalCompletionTool,
				Input: map[string]any{
					"status":  "done",
					"summary": "Shipped feature X",
					"pr_url":  "https://github.com/o/r/pull/1",
				},
			},
		}
		if err := st.UpdateAgentState(ctx, "alpha", func(ts *store.AgentState) {
			ts.Status = "done"
			ts.ResultSummary = "ok"
		}); err != nil {
			t.Fatalf("seed done: %v", err)
		}
		close(ch)
		return fake, ch, nil
	}
}

func TestDispatchEndToEndWritesSignalState(t *testing.T) {
	fx := runEndToEndSignal(t)
	if fx.state.SignalStatus != "done" {
		t.Fatalf("signal_status: %s", fx.state.SignalStatus)
	}
	if fx.state.SignalSummary != "Shipped feature X" {
		t.Fatalf("signal_summary: %s", fx.state.SignalSummary)
	}
	if fx.state.SignalPRURL == "" {
		t.Fatalf("signal_pr_url empty")
	}
	if fx.state.SignalAt.IsZero() {
		t.Fatalf("signal_at zero")
	}
}

func TestDispatchEndToEndRelaysResultEcho(t *testing.T) {
	fx := runEndToEndSignal(t)
	if len(fx.sent) != 1 {
		t.Fatalf("expected one CustomToolResult send, got %d", len(fx.sent))
	}
	if fx.sent[0].CustomToolResult == nil {
		t.Fatalf("CustomToolResult is nil")
	}
	gotResult, ok := fx.sent[0].CustomToolResult.Result.(json.RawMessage)
	if !ok {
		t.Fatalf("result type: %T", fx.sent[0].CustomToolResult.Result)
	}
	if !strings.Contains(string(gotResult), `"status":"done"`) {
		t.Fatalf("result should contain the echoed status: %s", gotResult)
	}
}

// TestDispatchEndToEndPersistsArtifacts covers the full PR 3 dispatch path:
// an MA stub emits signal_completion(artifacts={...}), the engine routes the
// event to the registered customtools handler, the handler persists the
// artifact to <ws.Path>/artifacts/<agent>/<key>.json, and the agent's state
// gets the key appended.
func TestDispatchEndToEndPersistsArtifacts(t *testing.T) {
	customtools.Reset()
	t.Cleanup(customtools.Reset)
	customtools.MustRegister(customtools.NewSignalCompletion())

	ctx := context.Background()
	st := seedAlphaRun(t)
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	cfg := alphaSingleTeamCfg()

	fake := &fakeManagedSession{id: "sess_alpha"}
	r := &orchestrationRun{
		cfg:        cfg,
		emitter:    event.NoopEmitter{},
		runService: runsvc.New(st),
		ws:         ws,
	}
	r.startTeamMAForTest = func(_ context.Context, _ *Team, _ *store.RunState, _ io.Writer) (managedSession, <-chan spawner.Event, error) {
		ch := make(chan spawner.Event, 1)
		ch <- spawner.AgentCustomToolUseEvent{
			BaseEvent: spawner.BaseEvent{ID: "evt_signal", Type: spawner.EventTypeAgentCustomToolUse, ProcessedAt: time.Now()},
			ToolUse: spawner.ToolUse{
				ID:   "evt_signal",
				Name: customtools.SignalCompletionTool,
				Input: map[string]any{
					"status":  "done",
					"summary": "with artifacts",
					"pr_url":  "https://github.com/o/r/pull/2",
					"artifacts": map[string]any{
						"design_doc": map[string]any{"type": "text", "content": "## the doc"},
						"verdict":    map[string]any{"type": "json", "content": map[string]string{"k": "v"}},
					},
				},
			},
		}
		if err := st.UpdateAgentState(ctx, "alpha", func(ts *store.AgentState) {
			ts.Status = "done"
			ts.ResultSummary = "ok"
		}); err != nil {
			t.Fatalf("seed done: %v", err)
		}
		close(ch)
		return fake, ch, nil
	}

	if _, err := r.runTeamMA(ctx, 0, &cfg.Agents[0], &store.RunState{RunID: "run_test"}); err != nil {
		t.Fatalf("runTeamMA: %v", err)
	}

	state, err := st.LoadRunState(ctx)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	got := state.Agents["alpha"].Artifacts
	want := []string{"design_doc", "verdict"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("AgentState.Artifacts = %v, want %v", got, want)
	}

	for _, key := range want {
		path := filepath.Join(ws.Path, "artifacts", "alpha", key+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected artifact file at %s: %v", path, err)
		}
	}
}

func TestDispatchEndToEndFiresNotifications(t *testing.T) {
	fx := runEndToEndSignal(t)
	if len(fx.notifications) != 1 {
		t.Fatalf("notify fan-out count: want 1 got %d", len(fx.notifications))
	}
	got := fx.notifications[0]
	if got.Status != "done" || got.PRURL == "" {
		t.Fatalf("notification payload: %+v", got)
	}
	if _, err := readFile(fx.logPath); err != nil {
		t.Fatalf("notifications.ndjson missing: %v", err)
	}
}

// runWithCustomToolEventsRaw mirrors runWithCustomToolEvents but does not
// register a handler — used by the unknown-tool test to exercise the Lookup
// miss path without overwriting the prior register.
func runWithCustomToolEventsRaw(t *testing.T, eventsToEmit ...spawner.Event) (*orchestrationRun, *fakeManagedSession) {
	t.Helper()
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{
		Project: "p",
		RunID:   "run_test",
		Backend: BackendManagedAgents,
		Agents:  map[string]store.AgentState{"alpha": {Status: "pending"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	cfg := &Config{
		Name:     "p",
		Backend:  Backend{Kind: BackendManagedAgents},
		Defaults: Defaults{TimeoutMinutes: 5},
		Agents: []Team{
			{Name: "alpha", Lead: Lead{Role: "Lead"}, Tasks: []Task{{Summary: "x"}}},
		},
	}
	cfg.ResolveDefaults()

	fake := &fakeManagedSession{id: "sess_alpha"}
	r := &orchestrationRun{
		cfg:        cfg,
		emitter:    event.NoopEmitter{},
		runService: runsvc.New(st),
		ws:         ws,
	}
	r.startTeamMAForTest = func(_ context.Context, _ *Team, _ *store.RunState, _ io.Writer) (managedSession, <-chan spawner.Event, error) {
		ch := make(chan spawner.Event, len(eventsToEmit))
		for _, ev := range eventsToEmit {
			ch <- ev
		}
		if err := st.UpdateAgentState(ctx, "alpha", func(ts *store.AgentState) {
			ts.Status = "done"
			ts.ResultSummary = "ok"
		}); err != nil {
			t.Fatalf("seed done: %v", err)
		}
		close(ch)
		return fake, ch, nil
	}
	return r, fake
}

type intentionalError string

func (e intentionalError) Error() string { return string(e) }

func TestMarshalToolInputRoundtripsValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil falls back to empty object", nil, `{}`},
		{"empty raw falls back to empty object", json.RawMessage(``), `{}`},
		{"raw bytes pass through", json.RawMessage(`{"k":1}`), `{"k":1}`},
		{
			name: "decoded map is re-marshaled",
			in:   map[string]any{"hello": "world"},
			want: `{"hello":"world"}`,
		},
		{
			// Regression: a JSON string scalar like `"hello"` is decoded by
			// the spawner into a Go string `hello`. A previous shortcut
			// returned that as raw bytes — no quotes — so handlers tried to
			// unmarshal `hello` and erroneously failed. The fix forces a
			// re-marshal so the bytes round-trip through the JSON quoting.
			name: "string scalar is JSON-quoted",
			in:   "hello",
			want: `"hello"`,
		},
		{
			name: "number scalar is rendered as JSON number",
			in:   float64(42),
			want: `42`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := marshalToolInput(tc.in)
			if err != nil {
				t.Fatalf("marshalToolInput: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func equalJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("parse a: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("parse b: %v", err)
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return bytes.Equal(ab, bb)
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}
