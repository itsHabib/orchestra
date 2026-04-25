package spawner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/memstore"
)

func TestManagedAgentsSession_StreamFirstOrdering(t *testing.T) {
	ctx := context.Background()
	seq := &callSeq{}
	sessions := &fakeSessionAPI{seq: seq}
	events := &fakeSessionEventsAPI{seq: seq, stream: &fakeStream{}}
	sp := newManagedAgentsSpawnerWithSessions(memstore.New(), newFakeAgentAPI(), newFakeEnvAPI(), sessions, events)

	pending, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:    AgentHandle{ID: "agent_1", Version: 2},
		Env:      EnvHandle{ID: "env_1"},
		TeamName: "alpha",
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	session, _, err := pending.Stream(ctx)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := session.Send(ctx, &UserEvent{Type: UserEventTypeMessage, Message: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	want := []string{"new", "stream", "send"}
	if got := seq.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("call order=%v, want %v", got, want)
	}
}

func TestManagedAgentsTranslator_EndTurnWritesSummaryAndDoneState(t *testing.T) {
	ctx := context.Background()
	st := seededSessionStore(t)
	var log bytes.Buffer
	var summaryTeam, summaryText string
	events := &fakeSessionEventsAPI{
		stream: &fakeStream{events: []managedEvent{
			rawEvent(`{"id":"e1","type":"session.status_running","processed_at":"2026-04-19T12:00:00Z"}`),
			rawEvent(`{"id":"e2","type":"agent.message","processed_at":"2026-04-19T12:00:01Z","content":[{"type":"text","text":"final summary"}]}`),
			rawEvent(`{"id":"e3","type":"session.status_idle","processed_at":"2026-04-19T12:00:02Z","stop_reason":{"type":"end_turn"}}`),
		}},
	}
	sp := newManagedAgentsSpawnerWithSessions(st, newFakeAgentAPI(), newFakeEnvAPI(), &fakeSessionAPI{}, events)

	pending, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:     AgentHandle{ID: "agent_1", Version: 7},
		Env:       EnvHandle{ID: "env_1"},
		TeamName:  "alpha",
		LogWriter: &log,
		Store:     st,
		SummaryWriter: func(teamName, text string) error {
			summaryTeam = teamName
			summaryText = text
			return nil
		},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	_, ch, err := pending.Stream(ctx)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(ch)

	state, err := st.LoadRunState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alpha := state.Teams["alpha"]
	if alpha.Status != "done" || alpha.ResultSummary != "final summary" {
		t.Fatalf("unexpected state: %+v", alpha)
	}
	if alpha.AgentID != "agent_1" || alpha.AgentVersion != 7 || alpha.SessionID != "sess_1" {
		t.Fatalf("handles not persisted: %+v", alpha)
	}
	if alpha.LastEventID != "e3" {
		t.Fatalf("LastEventID=%q, want e3", alpha.LastEventID)
	}
	if summaryTeam != "alpha" || summaryText != "final summary" {
		t.Fatalf("summary=(%q,%q), want alpha/final summary", summaryTeam, summaryText)
	}
	if lines := strings.Count(strings.TrimSpace(log.String()), "\n") + 1; lines != 3 {
		t.Fatalf("log lines=%d, want 3\n%s", lines, log.String())
	}
}

func TestManagedAgentsTranslator_DedupesBeforeLogAndState(t *testing.T) {
	ctx := context.Background()
	st := seededSessionStore(t)
	var log bytes.Buffer
	events := &fakeSessionEventsAPI{
		stream: &fakeStream{events: []managedEvent{
			rawEvent(`{"id":"e1","type":"span.model_request_end","processed_at":"2026-04-19T12:00:00Z","model_usage":{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`),
			rawEvent(`{"id":"e1","type":"span.model_request_end","processed_at":"2026-04-19T12:00:00Z","model_usage":{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`),
		}},
	}
	sp := newManagedAgentsSpawnerWithSessions(st, newFakeAgentAPI(), newFakeEnvAPI(), &fakeSessionAPI{}, events)
	pending, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:     AgentHandle{ID: "agent_1", Version: 1},
		Env:       EnvHandle{ID: "env_1"},
		TeamName:  "alpha",
		LogWriter: &log,
		Store:     st,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ch, err := pending.Stream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)

	state, err := st.LoadRunState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alpha := state.Teams["alpha"]
	if alpha.InputTokens != 10 || alpha.OutputTokens != 4 || alpha.CacheCreationInputTokens != 3 || alpha.CacheReadInputTokens != 2 {
		t.Fatalf("usage double-counted or missing: %+v", alpha)
	}
	if lines := strings.Count(strings.TrimSpace(log.String()), "\n") + 1; lines != 1 {
		t.Fatalf("duplicate was logged; lines=%d log=%s", lines, log.String())
	}
}

func TestManagedAgentsTranslator_ReconnectBackfillsWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	st := seededSessionStore(t)
	var log bytes.Buffer
	events := &fakeSessionEventsAPI{
		streams: []eventStream{
			&fakeStream{events: []managedEvent{
				rawEvent(`{"id":"e1","type":"session.status_running","processed_at":"2026-04-19T12:00:00Z"}`),
				rawEvent(`{"id":"e2","type":"span.model_request_end","processed_at":"2026-04-19T12:00:01Z","model_usage":{"input_tokens":5,"output_tokens":2}}`),
			}, err: errors.New("temporary stream drop")},
		},
		pager: &fakePager{events: []managedEvent{
			rawEvent(`{"id":"e2","type":"span.model_request_end","processed_at":"2026-04-19T12:00:01Z","model_usage":{"input_tokens":5,"output_tokens":2}}`),
			rawEvent(`{"id":"e3","type":"agent.message","processed_at":"2026-04-19T12:00:02Z","content":[{"type":"text","text":"after reconnect"}]}`),
			rawEvent(`{"id":"e4","type":"session.status_idle","processed_at":"2026-04-19T12:00:03Z","stop_reason":{"type":"end_turn"}}`),
		}},
	}
	sp := newManagedAgentsSpawnerWithSessions(
		st,
		newFakeAgentAPI(),
		newFakeEnvAPI(),
		&fakeSessionAPI{},
		events,
		WithManagedAgentsConfig(ManagedAgentsConfig{
			MaxListPages:          1,
			AgentLockTimeout:      time.Second,
			EnvLockTimeout:        time.Second,
			SessionEventSeenLimit: 16,
			APIMaxAttempts:        2,
			APIRetryBaseDelay:     time.Nanosecond,
			APIRetryMaxDelay:      time.Nanosecond,
		}),
	)
	pending, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:         AgentHandle{ID: "agent_1", Version: 1},
		Env:           EnvHandle{ID: "env_1"},
		TeamName:      "alpha",
		LogWriter:     &log,
		Store:         st,
		SummaryWriter: func(string, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ch, err := pending.Stream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)

	state, err := st.LoadRunState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alpha := state.Teams["alpha"]
	if alpha.Status != "done" || alpha.ResultSummary != "after reconnect" {
		t.Fatalf("unexpected state: %+v", alpha)
	}
	if alpha.InputTokens != 5 || alpha.OutputTokens != 2 {
		t.Fatalf("usage was replayed or lost: %+v", alpha)
	}
	if alpha.LastEventID != "e4" {
		t.Fatalf("LastEventID=%q, want e4", alpha.LastEventID)
	}
	if got := logEventIDs(t, log.String()); !reflect.DeepEqual(got, []string{"e1", "e2", "e3", "e4"}) {
		t.Fatalf("logged ids=%v, want [e1 e2 e3 e4]\n%s", got, log.String())
	}
}

func TestManagedAgentsTranslator_RequiresActionFails(t *testing.T) {
	ctx := context.Background()
	st := seededSessionStore(t)
	events := &fakeSessionEventsAPI{
		stream: &fakeStream{events: []managedEvent{
			rawEvent(`{"id":"e1","type":"session.status_idle","processed_at":"2026-04-19T12:00:00Z","stop_reason":{"type":"requires_action","event_ids":["tool_1"]}}`),
		}},
	}
	sp := newManagedAgentsSpawnerWithSessions(st, newFakeAgentAPI(), newFakeEnvAPI(), &fakeSessionAPI{}, events)
	pending, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:    AgentHandle{ID: "agent_1", Version: 1},
		Env:      EnvHandle{ID: "env_1"},
		TeamName: "alpha",
		Store:    st,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ch, err := pending.Stream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)

	state, err := st.LoadRunState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alpha := state.Teams["alpha"]
	if alpha.Status != "failed" || alpha.LastError != "tool confirmation requested; not supported in v1" {
		t.Fatalf("unexpected state: %+v", alpha)
	}
}

func TestManagedAgentsTranslator_SummaryWriteFailureFailsTeam(t *testing.T) {
	ctx := context.Background()
	st := seededSessionStore(t)
	events := &fakeSessionEventsAPI{
		stream: &fakeStream{events: []managedEvent{
			rawEvent(`{"id":"e1","type":"agent.message","processed_at":"2026-04-19T12:00:00Z","content":[{"type":"text","text":"summary"}]}`),
			rawEvent(`{"id":"e2","type":"session.status_idle","processed_at":"2026-04-19T12:00:01Z","stop_reason":{"type":"end_turn"}}`),
		}},
	}
	sp := newManagedAgentsSpawnerWithSessions(st, newFakeAgentAPI(), newFakeEnvAPI(), &fakeSessionAPI{}, events)
	pending, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:    AgentHandle{ID: "agent_1", Version: 1},
		Env:      EnvHandle{ID: "env_1"},
		TeamName: "alpha",
		Store:    st,
		SummaryWriter: func(string, string) error {
			return errors.New("disk full")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, ch, err := pending.Stream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)
	if session.Err() == nil || !strings.Contains(session.Err().Error(), "summary_write") {
		t.Fatalf("session.Err()=%v, want summary_write", session.Err())
	}
	state, err := st.LoadRunState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alpha := state.Teams["alpha"]
	if alpha.Status != "failed" || !strings.Contains(alpha.LastError, "summary_write") {
		t.Fatalf("unexpected state: %+v", alpha)
	}
}

func TestManagedAgentsTranslator_StateWriteFailureExits(t *testing.T) {
	ctx := context.Background()
	base := seededSessionStore(t)
	st := &failingUpdateStore{Store: base, panicValue: "boom"}
	events := &fakeSessionEventsAPI{
		stream: &fakeStream{events: []managedEvent{
			rawEvent(`{"id":"e1","type":"session.status_running","processed_at":"2026-04-19T12:00:00Z"}`),
		}},
	}
	sp := newManagedAgentsSpawnerWithSessions(st, newFakeAgentAPI(), newFakeEnvAPI(), &fakeSessionAPI{}, events)
	pending, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:    AgentHandle{ID: "agent_1", Version: 1},
		Env:      EnvHandle{ID: "env_1"},
		TeamName: "alpha",
		Store:    st,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, ch, err := pending.Stream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)
	if session.Err() == nil || !strings.Contains(session.Err().Error(), "state_write: panic: boom") {
		t.Fatalf("session.Err()=%v, want state_write panic", session.Err())
	}
	if st.calls < 2 {
		t.Fatalf("UpdateTeamState calls=%d, want primary plus fallback", st.calls)
	}
}

func TestTranslateMAEvent_CoversKnownDesignRows(t *testing.T) {
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		raw  string
		want EventType
	}{
		{"agent_message", `{"id":"e1","type":"agent.message","content":[{"type":"text","text":"hi"}]}`, EventTypeAgentMessage},
		{"agent_thinking", `{"id":"e1","type":"agent.thinking"}`, EventTypeAgentThinking},
		{"agent_tool_use", `{"id":"e1","type":"agent.tool_use","name":"bash","input":{"command":"true"}}`, EventTypeAgentToolUse},
		{"agent_tool_result", `{"id":"e1","type":"agent.tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}]}`, EventTypeAgentToolResult},
		{"agent_mcp_tool_use", `{"id":"e1","type":"agent.mcp_tool_use","mcp_server_name":"docs","name":"search"}`, EventTypeAgentMCPToolUse},
		{"agent_mcp_tool_result", `{"id":"e1","type":"agent.mcp_tool_result","mcp_tool_use_id":"t1","content":[{"type":"text","text":"ok"}]}`, EventTypeAgentMCPToolResult},
		{"agent_custom_tool_use", `{"id":"e1","type":"agent.custom_tool_use","name":"x"}`, EventTypeAgentCustomToolUse},
		{"agent_context_compacted", `{"id":"e1","type":"agent.thread_context_compacted","content":[{"type":"text","text":"short"}]}`, EventTypeAgentThreadContextCompacted},
		{"status_running", `{"id":"e1","type":"session.status_running"}`, EventTypeSessionStatusRunning},
		{"status_idle", `{"id":"e1","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`, EventTypeSessionStatusIdle},
		{"status_rescheduled", `{"id":"e1","type":"session.status_rescheduled"}`, EventTypeSessionStatusRescheduled},
		{"status_terminated", `{"id":"e1","type":"session.status_terminated"}`, EventTypeSessionStatusTerminated},
		{"session_error", `{"id":"e1","type":"session.error","error":{"message":"boom","code":"x"}}`, EventTypeSessionError},
		{"span_start", `{"id":"e1","type":"span.model_request_start"}`, EventTypeSpanModelRequestStart},
		{"span_end", `{"id":"e1","type":"span.model_request_end","model_usage":{"input_tokens":1}}`, EventTypeSpanModelRequestEnd},
		{"unknown", `{"id":"e1","type":"session.deleted"}`, EventTypeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := translateMAEvent([]byte(tc.raw), now)
			if err != nil {
				t.Fatalf("translateMAEvent: %v", err)
			}
			if got.EventType() != tc.want {
				t.Fatalf("EventType=%s, want %s", got.EventType(), tc.want)
			}
		})
	}
}

func seededSessionStore(t *testing.T) *memstore.MemStore {
	t.Helper()
	st := memstore.New()
	if err := st.SaveRunState(context.Background(), &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Teams: map[string]store.TeamState{
			"alpha": {Status: "pending"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func rawEvent(s string) managedEvent {
	return managedEvent{raw: []byte(s)}
}

func logEventIDs(t *testing.T, log string) []string {
	t.Helper()
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var ev struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		ids = append(ids, ev.ID)
	}
	return ids
}

func drain(ch <-chan Event) {
	for {
		if _, ok := <-ch; !ok {
			return
		}
	}
}

type callSeq struct {
	mu    sync.Mutex
	calls []string
}

func (s *callSeq) Add(call string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

func (s *callSeq) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

type fakeSessionAPI struct {
	seq *callSeq
}

func (f *fakeSessionAPI) New(context.Context, anthropic.BetaSessionNewParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSession, error) {
	f.seq.Add("new")
	return &anthropic.BetaManagedAgentsSession{ID: "sess_1"}, nil
}

func (f *fakeSessionAPI) Get(context.Context, string, anthropic.BetaSessionGetParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSession, error) {
	return &anthropic.BetaManagedAgentsSession{ID: "sess_1", Status: anthropic.BetaManagedAgentsSessionStatusIdle}, nil
}

type fakeSessionEventsAPI struct {
	seq     *callSeq
	stream  eventStream
	streams []eventStream
	pager   eventPager
	sent    []anthropic.BetaSessionEventSendParams
}

func (f *fakeSessionEventsAPI) Send(_ context.Context, _ string, params anthropic.BetaSessionEventSendParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsSendSessionEvents, error) {
	f.seq.Add("send")
	f.sent = append(f.sent, params)
	return &anthropic.BetaManagedAgentsSendSessionEvents{}, nil
}

func (f *fakeSessionEventsAPI) StreamEvents(context.Context, string, anthropic.BetaSessionEventStreamParams, ...option.RequestOption) eventStream {
	f.seq.Add("stream")
	if len(f.streams) > 0 {
		next := f.streams[0]
		f.streams = f.streams[1:]
		return next
	}
	if f.stream == nil {
		return &fakeStream{}
	}
	return f.stream
}

func (f *fakeSessionEventsAPI) ListAutoPaging(context.Context, string, anthropic.BetaSessionEventListParams, ...option.RequestOption) eventPager {
	if f.pager != nil {
		return f.pager
	}
	return &fakePager{}
}

type fakeStream struct {
	events []managedEvent
	idx    int
	err    error
}

func (s *fakeStream) Next() bool {
	if s.idx >= len(s.events) {
		return false
	}
	s.idx++
	return true
}

func (s *fakeStream) Current() managedEvent {
	return s.events[s.idx-1]
}

func (s *fakeStream) Err() error {
	return s.err
}

func (s *fakeStream) Close() error {
	return nil
}

type fakePager struct {
	events []managedEvent
	idx    int
	err    error
}

func (p *fakePager) Next() bool {
	if p.idx >= len(p.events) {
		return false
	}
	p.idx++
	return true
}

func (p *fakePager) Current() managedEvent {
	return p.events[p.idx-1]
}

func (p *fakePager) Err() error {
	return p.err
}

type failingUpdateStore struct {
	store.Store
	calls      int
	panicValue any
	err        error
}

func (s *failingUpdateStore) UpdateTeamState(context.Context, string, func(*store.TeamState)) error {
	s.calls++
	if s.panicValue != nil {
		panic(s.panicValue)
	}
	return s.err
}
