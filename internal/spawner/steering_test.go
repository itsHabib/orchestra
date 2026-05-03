package spawner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/itsHabib/orchestra/internal/store"
)

type recordingEventsClient struct {
	calls       int
	failUntil   int
	failErr     error
	lastParams  anthropic.BetaSessionEventSendParams
	lastSession string
}

func (c *recordingEventsClient) Send(_ context.Context, sessionID string, params anthropic.BetaSessionEventSendParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsSendSessionEvents, error) {
	c.calls++
	c.lastSession = sessionID
	c.lastParams = params
	if c.calls <= c.failUntil {
		return nil, c.failErr
	}
	return &anthropic.BetaManagedAgentsSendSessionEvents{}, nil
}

// recordingEventsClient implements only the narrow SessionEventSender
// interface — steering helpers don't need StreamEvents / ListAutoPaging.
var _ SessionEventSender = (*recordingEventsClient)(nil)

func TestSendUserMessage_DeliversTextWithMessageType(t *testing.T) {
	client := &recordingEventsClient{}
	if err := SendUserMessage(context.Background(), client, "sess_42", "use the existing JSON store", 0); err != nil {
		t.Fatalf("SendUserMessage: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("calls=%d, want 1", client.calls)
	}
	if client.lastSession != "sess_42" {
		t.Fatalf("session=%q, want sess_42", client.lastSession)
	}
	if got := len(client.lastParams.Events); got != 1 {
		t.Fatalf("events=%d, want 1", got)
	}
	ev := client.lastParams.Events[0]
	if ev.OfUserMessage == nil {
		t.Fatalf("expected OfUserMessage to be set, got %+v", ev)
	}
	if ev.OfUserMessage.Type != anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage {
		t.Fatalf("Type=%q, want user_message (verifies the SDK Type-bug workaround is still applied)", ev.OfUserMessage.Type)
	}
	if got := len(ev.OfUserMessage.Content); got != 1 {
		t.Fatalf("content blocks=%d, want 1", got)
	}
	if ev.OfUserMessage.Content[0].OfText == nil || ev.OfUserMessage.Content[0].OfText.Text != "use the existing JSON store" {
		t.Fatalf("text=%+v, want 'use the existing JSON store'", ev.OfUserMessage.Content[0].OfText)
	}
}

func TestSendUserMessage_RetriesOn5xxThenSucceeds(t *testing.T) {
	client := &recordingEventsClient{
		failUntil: 2,
		failErr:   newAPIError(http.StatusInternalServerError),
	}
	if err := SendUserMessage(context.Background(), client, "sess_42", "hi", 3); err != nil {
		t.Fatalf("SendUserMessage: %v", err)
	}
	if client.calls != 3 {
		t.Fatalf("calls=%d, want 3 (two failures + one success)", client.calls)
	}
}

func TestSendUserMessage_NoRetryReturnsFirstError(t *testing.T) {
	client := &recordingEventsClient{
		failUntil: 1,
		failErr:   newAPIError(http.StatusServiceUnavailable),
	}
	err := SendUserMessage(context.Background(), client, "sess_42", "hi", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if client.calls != 1 {
		t.Fatalf("calls=%d, want exactly 1 under retryAttempts=0", client.calls)
	}
}

func TestSendUserMessage_RejectsEmptySession(t *testing.T) {
	err := SendUserMessage(context.Background(), &recordingEventsClient{}, "", "hi", 0)
	if !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("err=%v, want ErrInvalidArgument", err)
	}
}

func TestSendUserMessage_RejectsNilClient(t *testing.T) {
	err := SendUserMessage(context.Background(), nil, "sess_42", "hi", 0)
	if !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("err=%v, want ErrInvalidArgument", err)
	}
}

func TestSendUserInterrupt_DeliversInterruptType(t *testing.T) {
	client := &recordingEventsClient{}
	if err := SendUserInterrupt(context.Background(), client, "sess_42", 0); err != nil {
		t.Fatalf("SendUserInterrupt: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("calls=%d, want 1", client.calls)
	}
	if got := len(client.lastParams.Events); got != 1 {
		t.Fatalf("events=%d, want 1", got)
	}
	if client.lastParams.Events[0].OfUserInterrupt == nil {
		t.Fatalf("expected OfUserInterrupt to be set, got %+v", client.lastParams.Events[0])
	}
}

func TestSendUserInterrupt_NoRetryByDefault(t *testing.T) {
	client := &recordingEventsClient{
		failUntil: 5,
		failErr:   newAPIError(http.StatusServiceUnavailable),
	}
	err := SendUserInterrupt(context.Background(), client, "sess_42", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if client.calls != 1 {
		t.Fatalf("calls=%d, want exactly 1 — interrupts must not retry without explicit opt-in", client.calls)
	}
}

func TestListTeamSessions_FlagsSteerableAndSortsByName(t *testing.T) {
	state := &store.RunState{
		Agents: map[string]store.AgentState{
			"charlie": {Status: "running", SessionID: "sess_c", AgentID: "agent_c", LastEventID: "e3", LastEventAt: time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)},
			"alpha":   {Status: "pending"},
			"bravo":   {Status: "running", SessionID: ""},
			"delta":   {Status: "done", SessionID: "sess_d"},
		},
	}
	rows := ListTeamSessions(state)
	wantOrder := []string{"alpha", "bravo", "charlie", "delta"}
	if len(rows) != len(wantOrder) {
		t.Fatalf("len(rows)=%d, want %d", len(rows), len(wantOrder))
	}
	for i, want := range wantOrder {
		if rows[i].Team != want {
			t.Fatalf("rows[%d].Team=%q, want %q", i, rows[i].Team, want)
		}
	}

	cases := map[string]bool{
		"alpha":   false, // pending
		"bravo":   false, // running but no session id
		"charlie": true,  // running with session id
		"delta":   false, // done
	}
	for _, row := range rows {
		if got := row.Steerable; got != cases[row.Team] {
			t.Fatalf("%s.Steerable=%v, want %v", row.Team, got, cases[row.Team])
		}
	}
}

func TestListTeamSessions_HandlesNilState(t *testing.T) {
	if got := ListTeamSessions(nil); got != nil {
		t.Fatalf("rows=%v, want nil", got)
	}
}

func TestSteerableSessionID_Sentinels(t *testing.T) {
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
			want: ErrNoActiveRun,
		},
		{
			name: "local backend",
			st:   &store.RunState{Backend: "local", Agents: map[string]store.AgentState{}},
			team: "x",
			want: ErrLocalBackend,
		},
		{
			name: "team missing",
			st:   &store.RunState{Backend: "managed_agents", Agents: map[string]store.AgentState{}},
			team: "x",
			want: ErrTeamNotFound,
		},
		{
			name: "team not running",
			st: &store.RunState{Backend: "managed_agents", Agents: map[string]store.AgentState{
				"x": {Status: "done"},
			}},
			team: "x",
			want: ErrTeamNotRunning,
		},
		{
			name: "no session",
			st: &store.RunState{Backend: "managed_agents", Agents: map[string]store.AgentState{
				"x": {Status: "running"},
			}},
			team: "x",
			want: ErrNoSessionRecorded,
		},
	}
	for _, tc := range cases {
		_, err := SteerableSessionID(tc.st, tc.team)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: err=%v, want sentinel=%v", tc.name, err, tc.want)
		}
	}
}

func TestSteerableSessionID_HappyPath(t *testing.T) {
	state := &store.RunState{
		Backend: "managed_agents",
		Agents: map[string]store.AgentState{
			"alpha": {Status: "running", SessionID: "sess_xyz"},
		},
	}
	got, err := SteerableSessionID(state, "alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "sess_xyz" {
		t.Fatalf("session id: got %q, want sess_xyz", got)
	}
}

func TestIsSteeringSentinel_RecognizesBareAndWrappedSentinels(t *testing.T) {
	for _, sentinel := range []error{ErrNoActiveRun, ErrTeamNotFound, ErrTeamNotRunning, ErrNoSessionRecorded, ErrLocalBackend} {
		if !IsSteeringSentinel(sentinel) {
			t.Fatalf("bare sentinel %v not recognized", sentinel)
		}
		// %w-wrapping must keep IsSteeringSentinel happy — the CLI
		// commands wrap with extra context (workspace path, team name)
		// before returning, and callers errors.Is against the bare
		// sentinel; this test pins that contract.
		wrapped := fmt.Errorf("with extra context: %w", sentinel)
		if !IsSteeringSentinel(wrapped) {
			t.Fatalf("wrapped sentinel %v not recognized", wrapped)
		}
	}
	if IsSteeringSentinel(errors.New("random non-sentinel")) {
		t.Fatal("non-sentinel reported as sentinel")
	}
	if IsSteeringSentinel(nil) {
		t.Fatal("nil reported as sentinel")
	}
}

func newAPIError(status int) *anthropic.Error {
	apiErr := &anthropic.Error{}
	apiErr.StatusCode = status
	apiErr.Response = &http.Response{StatusCode: status}
	return apiErr
}

// TestTranslator_RoundTripsUserEchoEvents verifies the new translator branch
// emits typed echo events with the original ID and processed_at preserved.
func TestTranslator_RoundTripsUserEchoEvents(t *testing.T) {
	now := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)

	msgRaw := []byte(`{"id":"evt_msg","type":"user.message","processed_at":"2026-04-25T10:00:00Z","content":[{"type":"text","text":"use the JSON store"}]}`)
	got, _, err := translateMAEvent(msgRaw, now)
	if err != nil {
		t.Fatalf("translateMAEvent(user.message): %v", err)
	}
	echo, ok := got.(UserMessageEchoEvent)
	if !ok {
		t.Fatalf("type=%T, want UserMessageEchoEvent", got)
	}
	if echo.ID != "evt_msg" || echo.Type != EventTypeUserMessage {
		t.Fatalf("base=%+v, want id=evt_msg type=user.message", echo.BaseEvent)
	}
	if !strings.Contains(echo.Text, "use the JSON store") {
		t.Fatalf("Text=%q, want to contain 'use the JSON store'", echo.Text)
	}

	intRaw := []byte(`{"id":"evt_int","type":"user.interrupt","processed_at":"2026-04-25T10:00:01Z"}`)
	got, _, err = translateMAEvent(intRaw, now)
	if err != nil {
		t.Fatalf("translateMAEvent(user.interrupt): %v", err)
	}
	intEcho, ok := got.(UserInterruptEchoEvent)
	if !ok {
		t.Fatalf("type=%T, want UserInterruptEchoEvent", got)
	}
	if intEcho.ID != "evt_int" || intEcho.Type != EventTypeUserInterrupt {
		t.Fatalf("base=%+v, want id=evt_int type=user.interrupt", intEcho.BaseEvent)
	}
}
