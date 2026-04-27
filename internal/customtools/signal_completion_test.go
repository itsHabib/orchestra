package customtools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/notify"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/memstore"
)

const testTeam = "ship-feat"

func newSignalRunContext(t *testing.T, st store.Store) *RunContext {
	t.Helper()
	return &RunContext{
		Store: st,
		Now:   func() time.Time { return time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC) },
		RunID: "run_test",
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func loadTeamState(t *testing.T, st store.Store) store.TeamState {
	t.Helper()
	state, err := st.LoadRunState(context.Background())
	if err != nil {
		t.Fatalf("load run state: %v", err)
	}
	if state == nil {
		t.Fatalf("run state is nil")
	}
	ts, ok := state.Teams[testTeam]
	if !ok {
		t.Fatalf("team %q missing from state", testTeam)
	}
	return ts
}

// signalDoneFixture seeds a fresh memstore with one team, attaches a notify
// recorder, and runs Handle with a status=done payload. Returns the decoded
// result, the team state after Handle, and the captured notifications.
func signalDoneFixture(t *testing.T) (signalCompletionResult, store.TeamState, []notify.Notification, *RunContext) {
	t.Helper()
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{Teams: map[string]store.TeamState{testTeam: {}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	captured := make([]notify.Notification, 0, 1)
	rc := newSignalRunContext(t, st)
	rc.Notifier = notify.NotifierFunc(func(_ context.Context, n *notify.Notification) error {
		if n != nil {
			captured = append(captured, *n)
		}
		return nil
	})

	res, err := NewSignalCompletion().Handle(ctx, rc, testTeam, mustJSON(t, map[string]string{
		"status":  "done",
		"summary": "Shipped feature",
		"pr_url":  "https://github.com/o/r/pull/1",
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	var decoded signalCompletionResult
	if err := json.Unmarshal(res, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return decoded, loadTeamState(t, st), captured, rc
}

func TestSignalCompletionDoneReturnsOK(t *testing.T) {
	t.Parallel()
	decoded, _, _, _ := signalDoneFixture(t)
	if !decoded.OK {
		t.Fatalf("ok=false on success: %+v", decoded)
	}
	if decoded.Duplicate {
		t.Fatalf("duplicate flag set on first call: %+v", decoded)
	}
	if decoded.Status != "done" {
		t.Fatalf("status echo: %+v", decoded)
	}
}

func TestSignalCompletionDoneWritesState(t *testing.T) {
	t.Parallel()
	_, ts, _, rc := signalDoneFixture(t)
	if ts.SignalStatus != "done" {
		t.Fatalf("signal_status: %s", ts.SignalStatus)
	}
	if ts.SignalSummary != "Shipped feature" {
		t.Fatalf("signal_summary: %s", ts.SignalSummary)
	}
	if ts.SignalPRURL == "" {
		t.Fatalf("signal_pr_url empty")
	}
	if !ts.SignalAt.Equal(rc.Now()) {
		t.Fatalf("signal_at: want %v got %v", rc.Now(), ts.SignalAt)
	}
}

func TestSignalCompletionDoneFiresNotification(t *testing.T) {
	t.Parallel()
	_, _, captured, _ := signalDoneFixture(t)
	if len(captured) != 1 {
		t.Fatalf("notify count: want 1 got %d", len(captured))
	}
	got := captured[0]
	if got.Status != "done" {
		t.Fatalf("notify status: %s", got.Status)
	}
	if got.Team != testTeam {
		t.Fatalf("notify team: %s", got.Team)
	}
	if got.RunID != "run_test" {
		t.Fatalf("notify run_id: %s", got.RunID)
	}
	if got.PRURL == "" {
		t.Fatalf("notify pr_url empty")
	}
}

func TestSignalCompletionBlockedRequiresReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{Teams: map[string]store.TeamState{testTeam: {}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := NewSignalCompletion()
	rc := newSignalRunContext(t, st)

	_, err := h.Handle(ctx, rc, testTeam, mustJSON(t, map[string]string{
		"status":  "blocked",
		"summary": "stuck",
	}))
	if err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Fatalf("expected reason-required error, got %v", err)
	}

	// Confirm state was NOT mutated by a rejected call.
	ts := loadTeamState(t, st)
	if ts.SignalStatus != "" {
		t.Fatalf("rejected call should not write state: %+v", ts)
	}
}

func TestSignalCompletionBlockedWithReasonWritesState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{Teams: map[string]store.TeamState{testTeam: {}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := NewSignalCompletion()
	rc := newSignalRunContext(t, st)

	if _, err := h.Handle(ctx, rc, testTeam, mustJSON(t, map[string]string{
		"status":  "blocked",
		"summary": "ambiguous spec",
		"reason":  "spec doesn't say which flag should be the default",
	})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	ts := loadTeamState(t, st)
	if ts.SignalStatus != "blocked" || ts.SignalReason == "" {
		t.Fatalf("blocked state not written: %+v", ts)
	}
}

func TestSignalCompletionIdempotentOnDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{Teams: map[string]store.TeamState{testTeam: {}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	captured := make([]notify.Notification, 0, 2)
	rc := newSignalRunContext(t, st)
	rc.Notifier = notify.NotifierFunc(func(_ context.Context, n *notify.Notification) error {
		if n != nil {
			captured = append(captured, *n)
		}
		return nil
	})

	h := NewSignalCompletion()
	first := mustJSON(t, map[string]string{"status": "done", "summary": "first", "pr_url": "url1"})
	if _, err := h.Handle(ctx, rc, testTeam, first); err != nil {
		t.Fatalf("first handle: %v", err)
	}

	// Second call with different summary — must be a no-op + duplicate=true.
	second := mustJSON(t, map[string]string{"status": "blocked", "summary": "second", "reason": "wat"})
	res, err := h.Handle(ctx, rc, testTeam, second)
	if err != nil {
		t.Fatalf("second handle: %v", err)
	}
	var decoded signalCompletionResult
	if err := json.Unmarshal(res, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.Duplicate {
		t.Fatalf("second call should report duplicate=true, got %+v", decoded)
	}

	ts := loadTeamState(t, st)
	if ts.SignalStatus != "done" || ts.SignalSummary != "first" || ts.SignalPRURL != "url1" {
		t.Fatalf("first signal should be preserved, got %+v", ts)
	}

	if len(captured) != 1 {
		t.Fatalf("notify should fire once across duplicates; got %d calls", len(captured))
	}
}

func TestSignalCompletionRejectsBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload any
		want    string
	}{
		{
			name:    "unknown status",
			payload: map[string]string{"status": "weird", "summary": "x"},
			want:    "status must be",
		},
		{
			name:    "missing summary",
			payload: map[string]string{"status": "done"},
			want:    "summary is required",
		},
		{
			name:    "empty input",
			payload: nil,
			want:    "empty input",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := memstore.New()
			if err := st.SaveRunState(ctx, &store.RunState{Teams: map[string]store.TeamState{testTeam: {}}}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			rc := newSignalRunContext(t, st)
			h := NewSignalCompletion()
			var raw json.RawMessage
			if tc.payload != nil {
				raw = mustJSON(t, tc.payload)
			}
			_, err := h.Handle(ctx, rc, testTeam, raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSignalCompletionToleratesNotifierFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{Teams: map[string]store.TeamState{testTeam: {}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rc := newSignalRunContext(t, st)
	rc.Notifier = notify.NotifierFunc(func(context.Context, *notify.Notification) error {
		return errors.New("sink down")
	})

	h := NewSignalCompletion()
	_, err := h.Handle(ctx, rc, testTeam, mustJSON(t, map[string]string{
		"status": "done", "summary": "x", "pr_url": "y",
	}))
	// A direct (non-fan-out) notifier surface error IS surfaced — see the
	// signal_completion comment. We assert the state was still written so a
	// flaky notifier doesn't lose the signal.
	if err == nil {
		t.Fatalf("expected error from notifier failure")
	}
	ts := loadTeamState(t, st)
	if ts.SignalStatus != "done" {
		t.Fatalf("state should still be written on notifier failure, got %+v", ts)
	}
}

func TestSignalCompletionRequiresStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rc := &RunContext{Now: time.Now}
	h := NewSignalCompletion()
	_, err := h.Handle(ctx, rc, testTeam, mustJSON(t, map[string]string{
		"status": "done", "summary": "x",
	}))
	if err == nil || !strings.Contains(err.Error(), "nil store") {
		t.Fatalf("want nil-store error, got %v", err)
	}
}

func TestSignalCompletionToolDefinitionStable(t *testing.T) {
	t.Parallel()
	def := NewSignalCompletion().Tool()
	if def.Name != SignalCompletionTool {
		t.Fatalf("name: want %s got %s", SignalCompletionTool, def.Name)
	}
	required, ok := def.InputSchema["required"].([]string)
	if !ok {
		t.Fatalf("required field missing or wrong type")
	}
	wantRequired := map[string]bool{"status": false, "summary": false}
	for _, r := range required {
		if _, in := wantRequired[r]; in {
			wantRequired[r] = true
		}
	}
	for k, found := range wantRequired {
		if !found {
			t.Fatalf("required schema missing %q", k)
		}
	}
}
