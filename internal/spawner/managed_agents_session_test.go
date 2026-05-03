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
	alpha := state.Agents["alpha"]
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

// TestManagedAgentsTranslator_RequiresActionKeepsSessionAlive covers the
// custom-tool flow: when the agent emits a tool_use, MA pauses the session
// at status_idle/requires_action waiting for the host-side result. The
// pkg/orchestra dispatcher delivers the result asynchronously, so the
// spawner must not fail the team on requires_action — it just needs to
// keep the session alive until the result lands and the agent resumes.
//
// Pre-fix this case was hard-failed with "tool confirmation requested;
// not supported in v1", which was correct only for MCP tools whose
// permission_policy is always_ask. Custom tools never need confirmation.
func TestManagedAgentsTranslator_RequiresActionKeepsSessionAlive(t *testing.T) {
	ctx := context.Background()
	st := seededSessionStore(t)
	events := &fakeSessionEventsAPI{
		stream: &fakeStream{events: []managedEvent{
			rawEvent(`{"id":"e1","type":"session.status_idle","processed_at":"2026-04-19T12:00:00Z","stop_reason":{"type":"requires_action"}}`),
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
	alpha := state.Agents["alpha"]
	if alpha.Status == "failed" {
		t.Fatalf("requires_action must not fail the team (custom-tool result is on its way); got %+v", alpha)
	}
	if alpha.LastError != "" {
		t.Fatalf("LastError should be empty on requires_action: got %q", alpha.LastError)
	}
	if alpha.LastEventID != "e1" {
		t.Fatalf("LastEventID should advance: got %q want e1", alpha.LastEventID)
	}
}

// TestManagedAgentsTranslator_EndTurnAfterBlockedSignalKeepsSessionAlive
// covers DESIGN-ship-feature-workflow §12.3: a team that has signaled
// blocked must NOT be transitioned to status=done when the agent stops
// emitting on end_turn. The session has to stay reachable for the MCP
// unblock tool / orchestra msg to land a user.message.
//
// The test seeds SignalStatus="blocked" before the session runs (the
// customtools handler would write this in the real flow). The fakeStream
// then emits the end_turn idle event the way MA would after the agent's
// final tool_use. The expected outcome: the team's Status stays "running",
// the summary writer is NOT called, and the session-end is logged via
// LastEventID/LastEventAt only. The session terminates here only because
// fakeStream has no further events; in production the stream would block
// on the next read until a steering event arrives.
func TestManagedAgentsTranslator_EndTurnAfterBlockedSignalKeepsSessionAlive(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Agents: map[string]store.AgentState{
			// Pre-block state: customtools handler already wrote
			// SignalStatus="blocked" + reason in response to the agent's
			// signal_completion(blocked) call.
			"alpha": {
				Status:        "running",
				SignalStatus:  "blocked",
				SignalReason:  "the doc has no concrete spec",
				SignalSummary: "ambiguous",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var summaryCalls int
	events := &fakeSessionEventsAPI{
		stream: &fakeStream{events: []managedEvent{
			rawEvent(`{"id":"e1","type":"session.status_idle","processed_at":"2026-04-19T12:00:00Z","stop_reason":{"type":"end_turn"}}`),
		}},
	}
	sp := newManagedAgentsSpawnerWithSessions(st, newFakeAgentAPI(), newFakeEnvAPI(), &fakeSessionAPI{}, events)
	pending, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:    AgentHandle{ID: "agent_1", Version: 1},
		Env:      EnvHandle{ID: "env_1"},
		TeamName: "alpha",
		Store:    st,
		SummaryWriter: func(string, string) error {
			summaryCalls++
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
	alpha := state.Agents["alpha"]
	if alpha.Status != "running" {
		t.Fatalf("Status: got %q, want %q (blocked teams must stay reachable)", alpha.Status, "running")
	}
	if alpha.SignalStatus != "blocked" {
		t.Fatalf("SignalStatus must be preserved: got %q", alpha.SignalStatus)
	}
	if alpha.SignalReason == "" || alpha.SignalSummary == "" {
		t.Fatalf("blocked-side fields lost: %+v", alpha)
	}
	if alpha.LastEventID != "e1" {
		t.Fatalf("LastEventID: got %q, want e1", alpha.LastEventID)
	}
	if !alpha.EndedAt.IsZero() {
		t.Fatalf("EndedAt must remain zero on blocked: got %v", alpha.EndedAt)
	}
	if summaryCalls != 0 {
		t.Fatalf("summary writer must not run for blocked teams: calls=%d", summaryCalls)
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
	alpha := state.Agents["alpha"]
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
	alpha := state.Agents["alpha"]
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
	alpha := state.Agents["alpha"]
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
		t.Fatalf("UpdateAgentState calls=%d, want primary plus fallback", st.calls)
	}
}

type customToolResultProbe struct {
	Type            string `json:"type"`
	CustomToolUseID string `json:"custom_tool_use_id"`
	IsError         bool   `json:"is_error"`
	Content         []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func marshalCustomToolResultProbe(t *testing.T, event *UserEvent) customToolResultProbe {
	t.Helper()
	params, err := toSessionEventSendParams(event)
	if err != nil {
		t.Fatalf("toSessionEventSendParams: %v", err)
	}
	if len(params.Events) != 1 {
		t.Fatalf("Events len=%d, want 1", len(params.Events))
	}
	raw, err := json.Marshal(params.Events[0])
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var probe customToolResultProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if probe.Type != "user.custom_tool_result" {
		t.Fatalf("event type=%q, want user.custom_tool_result; raw=%s", probe.Type, raw)
	}
	return probe
}

func TestToSessionEventSendParams_CustomToolResultShape(t *testing.T) {
	cases := []struct {
		name      string
		event     *UserEvent
		wantText  string
		wantError bool
	}{
		{
			name: "result is json bytes",
			event: &UserEvent{
				Type: UserEventTypeCustomToolResult,
				CustomToolResult: &CustomToolResult{
					ToolUseID: "evt_x",
					Result:    json.RawMessage(`{"ok":true}`),
				},
			},
			wantText: `{"ok":true}`,
		},
		{
			name: "result is structured value",
			event: &UserEvent{
				Type: UserEventTypeCustomToolResult,
				CustomToolResult: &CustomToolResult{
					ToolUseID: "evt_x",
					Result:    map[string]any{"ok": true},
				},
			},
			wantText: `{"ok":true}`,
		},
		{
			name: "error overrides result",
			event: &UserEvent{
				Type: UserEventTypeCustomToolResult,
				CustomToolResult: &CustomToolResult{
					ToolUseID: "evt_x",
					Result:    "ignored",
					Error:     "oops",
				},
			},
			wantText:  "oops",
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := marshalCustomToolResultProbe(t, tc.event)
			if probe.CustomToolUseID != "evt_x" {
				t.Fatalf("tool_use_id=%q, want evt_x", probe.CustomToolUseID)
			}
			if probe.IsError != tc.wantError {
				t.Fatalf("is_error=%v, want %v", probe.IsError, tc.wantError)
			}
			if len(probe.Content) != 1 || probe.Content[0].Type != "text" {
				t.Fatalf("unexpected content: %+v", probe.Content)
			}
			if probe.Content[0].Text != tc.wantText {
				t.Fatalf("content text=%q, want %q", probe.Content[0].Text, tc.wantText)
			}
		})
	}
}

func TestToSessionEventSendParams_CustomToolResultRequiresFields(t *testing.T) {
	if _, err := toSessionEventSendParams(&UserEvent{Type: UserEventTypeCustomToolResult}); err == nil {
		t.Fatalf("expected error on nil CustomToolResult")
	}
	if _, err := toSessionEventSendParams(&UserEvent{
		Type:             UserEventTypeCustomToolResult,
		CustomToolResult: &CustomToolResult{},
	}); err == nil {
		t.Fatalf("expected error on empty tool use id")
	}
}

func TestToSessionEventSendParams_UserMessageIncludesType(t *testing.T) {
	// Regression guard for an SDK helper bug: anthropic-sdk-go v1.37.0's
	// BetaManagedAgentsEventParamsOfUserMessage does not set the required
	// Type field, and MA rejects the resulting request with HTTP 400
	// "events[0].type: Field required". We work around it by constructing
	// the union directly; this test pins the marshaled shape so a future
	// "simplification" back to the helper trips in CI rather than at
	// runtime against live MA.
	params, err := toSessionEventSendParams(&UserEvent{
		Type:    UserEventTypeMessage,
		Message: "hello",
	})
	if err != nil {
		t.Fatalf("toSessionEventSendParams: %v", err)
	}
	if len(params.Events) != 1 {
		t.Fatalf("Events len=%d, want 1", len(params.Events))
	}
	raw, err := json.Marshal(params.Events[0])
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if probe.Type != "user.message" {
		t.Fatalf("event type=%q, want user.message; raw=%s", probe.Type, raw)
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
		{"user_message_echo", `{"id":"e1","type":"user.message","content":[{"type":"text","text":"steered"}]}`, EventTypeUserMessage},
		{"user_interrupt_echo", `{"id":"e1","type":"user.interrupt"}`, EventTypeUserInterrupt},
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

func TestManagedAgentsTranslator_BootstrapEchoTaggedAsOrchestrator(t *testing.T) {
	ctx := context.Background()
	st := seededSessionStore(t)
	// Send returns a Data union populated with an event id; the session
	// must remember that id so the corresponding echo through the stream
	// comes back with FromOrchestrator=true.
	events := &fakeSessionEventsAPI{
		seq: &callSeq{},
		stream: &fakeStream{events: []managedEvent{
			rawEvent(`{"id":"sent_1","type":"user.message","processed_at":"2026-04-25T10:00:00Z","content":[{"type":"text","text":"bootstrap"}]}`),
			rawEvent(`{"id":"steered_1","type":"user.message","processed_at":"2026-04-25T10:00:01Z","content":[{"type":"text","text":"out of band"}]}`),
		}},
		sendResponses: []*anthropic.BetaManagedAgentsSendSessionEvents{
			{Data: []anthropic.BetaManagedAgentsSendSessionEventsDataUnion{{ID: "sent_1", Type: "user.message"}}},
		},
	}
	sp := newManagedAgentsSpawnerWithSessions(st, newFakeAgentAPI(), newFakeEnvAPI(), &fakeSessionAPI{seq: events.seq}, events)
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
	if err := session.Send(ctx, &UserEvent{Type: UserEventTypeMessage, Message: "bootstrap"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var sawBootstrap, sawSteered bool
	for ev := range ch {
		echo, ok := ev.(UserMessageEchoEvent)
		if !ok {
			continue
		}
		switch string(echo.ID) {
		case "sent_1":
			sawBootstrap = true
			if !echo.FromOrchestrator {
				t.Fatalf("bootstrap echo should be FromOrchestrator=true, got %+v", echo)
			}
		case "steered_1":
			sawSteered = true
			if echo.FromOrchestrator {
				t.Fatalf("out-of-process steered echo should be FromOrchestrator=false, got %+v", echo)
			}
		}
	}
	if !sawBootstrap || !sawSteered {
		t.Fatalf("missing echo events: bootstrap=%v steered=%v", sawBootstrap, sawSteered)
	}
}

func TestManagedAgentsTranslator_UserEchoAdvancesLastEventOnly(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Agents: map[string]store.AgentState{
			"alpha": {
				Status:       "running",
				SessionID:    "sess_1",
				InputTokens:  10,
				OutputTokens: 5,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	events := &fakeSessionEventsAPI{
		stream: &fakeStream{events: []managedEvent{
			rawEvent(`{"id":"e1","type":"user.message","processed_at":"2026-04-25T10:00:00Z","content":[{"type":"text","text":"use the JSON store"}]}`),
			rawEvent(`{"id":"e2","type":"user.interrupt","processed_at":"2026-04-25T10:00:01Z"}`),
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
	alpha := state.Agents["alpha"]
	if alpha.Status != "running" {
		t.Fatalf("Status=%q, want unchanged 'running' (echoes must not transition state)", alpha.Status)
	}
	if alpha.InputTokens != 10 || alpha.OutputTokens != 5 {
		t.Fatalf("counters mutated: %+v", alpha)
	}
	if alpha.LastEventID != "e2" {
		t.Fatalf("LastEventID=%q, want e2 (latest echo)", alpha.LastEventID)
	}
	if alpha.LastEventAt.IsZero() {
		t.Fatalf("LastEventAt was not advanced")
	}
}

func seededSessionStore(t *testing.T) *memstore.MemStore {
	t.Helper()
	st := memstore.New()
	if err := st.SaveRunState(context.Background(), &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Agents: map[string]store.AgentState{
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
	seq           *callSeq
	stream        eventStream
	streams       []eventStream
	pager         eventPager
	sent          []anthropic.BetaSessionEventSendParams
	sendResponses []*anthropic.BetaManagedAgentsSendSessionEvents
}

func (f *fakeSessionEventsAPI) Send(_ context.Context, _ string, params anthropic.BetaSessionEventSendParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsSendSessionEvents, error) {
	f.seq.Add("send")
	f.sent = append(f.sent, params)
	if len(f.sendResponses) > 0 {
		next := f.sendResponses[0]
		f.sendResponses = f.sendResponses[1:]
		return next, nil
	}
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

func (s *failingUpdateStore) UpdateAgentState(context.Context, string, func(*store.AgentState)) error {
	s.calls++
	if s.panicValue != nil {
		panic(s.panicValue)
	}
	return s.err
}
