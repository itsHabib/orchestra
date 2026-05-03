package customtools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/artifacts"
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

func loadTeamState(t *testing.T, st store.Store) store.AgentState {
	t.Helper()
	state, err := st.LoadRunState(context.Background())
	if err != nil {
		t.Fatalf("load run state: %v", err)
	}
	if state == nil {
		t.Fatalf("run state is nil")
	}
	ts, ok := state.Agents[testTeam]
	if !ok {
		t.Fatalf("team %q missing from state", testTeam)
	}
	return ts
}

// signalDoneFixture seeds a fresh memstore with one team, attaches a notify
// recorder, and runs Handle with a status=done payload. Returns the decoded
// result, the team state after Handle, and the captured notifications.
func signalDoneFixture(t *testing.T) (signalCompletionResult, store.AgentState, []notify.Notification, *RunContext) {
	t.Helper()
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{Agents: map[string]store.AgentState{testTeam: {}}}); err != nil {
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
	if err := st.SaveRunState(ctx, &store.RunState{Agents: map[string]store.AgentState{testTeam: {}}}); err != nil {
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
	if err := st.SaveRunState(ctx, &store.RunState{Agents: map[string]store.AgentState{testTeam: {}}}); err != nil {
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
	if err := st.SaveRunState(ctx, &store.RunState{Agents: map[string]store.AgentState{testTeam: {}}}); err != nil {
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
	// The echo must reflect what was recorded, not the rejected duplicate's
	// status — otherwise a confused agent calling signal_completion(blocked)
	// after a successful signal_completion(done) would believe its block was
	// accepted when in fact "done" is still on file.
	if decoded.Status != "done" {
		t.Fatalf("duplicate echo should carry the recorded status %q, got %q", "done", decoded.Status)
	}

	ts := loadTeamState(t, st)
	if ts.SignalStatus != "done" || ts.SignalSummary != "first" || ts.SignalPRURL != "url1" {
		t.Fatalf("first signal should be preserved, got %+v", ts)
	}

	if len(captured) != 1 {
		t.Fatalf("notify should fire once across duplicates; got %d calls", len(captured))
	}
}

// TestSignalCompletionBlockedToDoneTransition covers the §7.2 recovery
// flow: a team that signaled blocked and then got unblocked via steering
// must be able to land its eventual signal_completion(done) — otherwise
// §11.2's run-status derivation never sees the run reach done. The
// transition overwrites every Signal* field (including SignalReason,
// which is no longer relevant once the team is done) and fires a fresh
// notification.
func TestSignalCompletionBlockedToDoneTransition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, captured := blockedToDoneFixture(t)
	h := NewSignalCompletion()
	rc := newSignalRunContext(t, st)
	rc.Notifier = notify.NotifierFunc(func(_ context.Context, n *notify.Notification) error {
		if n != nil {
			*captured = append(*captured, *n)
		}
		return nil
	})

	first := mustJSON(t, map[string]string{"status": "blocked", "summary": "ambiguous spec", "reason": "the doc has no concrete spec"})
	expectAccepted(ctx, t, h, rc, first, "blocked")

	// blocked → done is the allowed recovery transition. The handler
	// must overwrite all Signal* fields with the new outcome.
	second := mustJSON(t, map[string]string{"status": "done", "summary": "shipped", "pr_url": "https://github.com/x/y/pull/42"})
	expectAccepted(ctx, t, h, rc, second, "done")

	ts := loadTeamState(t, st)
	wantPR := "https://github.com/x/y/pull/42"
	if ts.SignalStatus != "done" || ts.SignalSummary != "shipped" || ts.SignalPRURL != wantPR || ts.SignalReason != "" {
		t.Fatalf("blocked → done did not overwrite all Signal* fields: %+v", ts)
	}

	// Both signals must fire notifications — the human cares about both
	// the block (so they can act) and the completion (so they know the
	// PR is ready).
	if len(*captured) != 2 || (*captured)[0].Status != "blocked" || (*captured)[1].Status != "done" {
		t.Fatalf("notify order/count: %+v", *captured)
	}
}

func blockedToDoneFixture(t *testing.T) (*memstore.MemStore, *[]notify.Notification) {
	t.Helper()
	st := memstore.New()
	if err := st.SaveRunState(context.Background(), &store.RunState{Agents: map[string]store.AgentState{testTeam: {}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	captured := make([]notify.Notification, 0, 2)
	return st, &captured
}

// expectAccepted runs Handle and asserts the call was NOT a duplicate and
// the recorded status matches `want`. Used by the transition test so each
// call site stays one line.
func expectAccepted(ctx context.Context, t *testing.T, h SignalCompletionHandler, rc *RunContext, payload []byte, want string) {
	t.Helper()
	raw, err := h.Handle(ctx, rc, testTeam, payload)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var decoded signalCompletionResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Duplicate {
		t.Fatalf("call must not be a duplicate: %+v", decoded)
	}
	if decoded.Status != want {
		t.Fatalf("recorded status: got %q, want %q", decoded.Status, want)
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
			name:    "done without pr_url",
			payload: map[string]string{"status": "done", "summary": "shipped"},
			want:    "pr_url is required when status=done",
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
			if err := st.SaveRunState(ctx, &store.RunState{Agents: map[string]store.AgentState{testTeam: {}}}); err != nil {
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
	if err := st.SaveRunState(ctx, &store.RunState{Agents: map[string]store.AgentState{testTeam: {}}}); err != nil {
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
		"status": "done", "summary": "x", "pr_url": "https://example.com/pr/1",
	}))
	if err == nil || !strings.Contains(err.Error(), "nil store") {
		t.Fatalf("want nil-store error, got %v", err)
	}
}

// newSignalContextWithArtifacts seeds a memstore + a FileStore-backed
// artifacts root under t.TempDir(). Returns the context, the store, and the
// artifacts root path so tests can inspect on-disk files directly.
func newSignalContextWithArtifacts(t *testing.T, phase string) (*RunContext, *memstore.MemStore, string) {
	t.Helper()
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{
		Agents: map[string]store.AgentState{testTeam: {}},
		Phase:  phase,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	root := filepath.Join(t.TempDir(), "artifacts")
	rc := newSignalRunContext(t, st)
	rc.Artifacts = artifacts.NewFileStore(root)
	rc.Phase = phase
	return rc, st, root
}

func TestSignalCompletionPersistsArtifacts(t *testing.T) {
	t.Parallel()
	rc, st, root := newSignalContextWithArtifacts(t, "design")
	ctx := context.Background()

	if _, err := NewSignalCompletion().Handle(ctx, rc, testTeam, mustJSON(t, map[string]any{
		"status":  "done",
		"summary": "shipped",
		"pr_url":  "https://github.com/o/r/pull/1",
		"artifacts": map[string]any{
			"design_doc": map[string]any{"type": "text", "content": "draft body"},
			"verdict":    map[string]any{"type": "json", "content": map[string]string{"decision": "proceed"}},
		},
	})); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	ts := loadTeamState(t, st)
	want := []string{"design_doc", "verdict"}
	if !reflect.DeepEqual(ts.Artifacts, want) {
		t.Fatalf("AgentState.Artifacts = %v, want %v", ts.Artifacts, want)
	}

	gotText, metaText, err := rc.Artifacts.Get(ctx, rc.RunID, testTeam, "design_doc")
	if err != nil {
		t.Fatalf("Get text artifact: %v", err)
	}
	if metaText.Type != artifacts.TypeText {
		t.Errorf("text type = %q, want %q", metaText.Type, artifacts.TypeText)
	}
	if metaText.Phase != "design" {
		t.Errorf("text phase = %q, want design", metaText.Phase)
	}
	if string(gotText.Content) != `"draft body"` {
		t.Errorf("text content = %s, want %q", gotText.Content, `"draft body"`)
	}

	gotJSON, _, err := rc.Artifacts.Get(ctx, rc.RunID, testTeam, "verdict")
	if err != nil {
		t.Fatalf("Get json artifact: %v", err)
	}
	if !json.Valid(gotJSON.Content) {
		t.Errorf("json content not valid JSON: %s", gotJSON.Content)
	}

	// On-disk path lives under <root>/<agent>/<key>.json
	wantPath := filepath.Join(root, testTeam, "design_doc.json")
	if _, statErr := os.Stat(wantPath); statErr != nil {
		t.Errorf("expected file at %s: %v", wantPath, statErr)
	}
}

func TestSignalCompletionRejectsBadArtifacts(t *testing.T) {
	t.Parallel()

	bigText := strings.Repeat("x", 300*1024) // > 256KB cap
	bigJSON := json.RawMessage(`{"data":"` + strings.Repeat("y", 300*1024) + `"}`)

	cases := []struct {
		name      string
		artifacts map[string]any
		want      string
	}{
		{
			name:      "bad type",
			artifacts: map[string]any{"k": map[string]any{"type": "xml", "content": "v"}},
			want:      `type must be "text" or "json"`,
		},
		{
			name:      "text content not a string",
			artifacts: map[string]any{"k": map[string]any{"type": "text", "content": 42}},
			want:      "type=text requires a JSON string content",
		},
		{
			name:      "per-key cap exceeded — text",
			artifacts: map[string]any{"big": map[string]any{"type": "text", "content": bigText}},
			want:      "size",
		},
		{
			name:      "per-key cap exceeded — json",
			artifacts: rawArtifacts(map[string]artifactInput{"big": {Type: "json", Content: bigJSON}}),
			want:      "size",
		},
		{
			name:      "empty content",
			artifacts: map[string]any{"k": map[string]any{"type": "text"}},
			want:      "content is empty",
		},
		{
			name:      "empty key",
			artifacts: map[string]any{"": map[string]any{"type": "text", "content": "v"}},
			want:      "artifact key is empty",
		},
		{
			name:      "key path traversal",
			artifacts: map[string]any{"../etc/passwd": map[string]any{"type": "text", "content": "v"}},
			want:      "invalid characters",
		},
		{
			name:      "key reserved",
			artifacts: map[string]any{".": map[string]any{"type": "text", "content": "v"}},
			want:      "reserved",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc, st, _ := newSignalContextWithArtifacts(t, "")
			payload := map[string]any{
				"status":    "done",
				"summary":   "x",
				"pr_url":    "https://example.com/pr/1",
				"artifacts": tc.artifacts,
			}
			_, err := NewSignalCompletion().Handle(context.Background(), rc, testTeam, mustJSON(t, payload))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
			// Rejected calls must NOT mutate state.
			ts := loadTeamState(t, st)
			if ts.SignalStatus != "" {
				t.Fatalf("rejected call wrote state: %+v", ts)
			}
			if len(ts.Artifacts) != 0 {
				t.Fatalf("rejected call appended artifacts: %v", ts.Artifacts)
			}
		})
	}
}

func TestSignalCompletionRejectsAggregateCap(t *testing.T) {
	t.Parallel()

	// Three artifacts at 200KB each = 600KB; aggregate is well under the
	// 4MB cap. Build something that pushes total over while keeping each
	// well under the per-key 256KB cap. Eight artifacts of ~600KB exceeds
	// the per-key cap, so we use 24 artifacts of ~200KB to total ~4.8MB.
	const perKey = 200 * 1024
	const count = 24
	arts := make(map[string]any, count)
	for i := 0; i < count; i++ {
		arts[strings.Repeat("k", i+1)] = map[string]any{ // unique keys "k", "kk", "kkk"...
			"type":    "text",
			"content": strings.Repeat("p", perKey-2), // accounting for the JSON quotes
		}
	}

	rc, _, _ := newSignalContextWithArtifacts(t, "")
	_, err := NewSignalCompletion().Handle(context.Background(), rc, testTeam, mustJSON(t, map[string]any{
		"status":    "done",
		"summary":   "x",
		"pr_url":    "https://example.com/pr/1",
		"artifacts": arts,
	}))
	if err == nil {
		t.Fatalf("expected aggregate-cap error")
	}
	if !strings.Contains(err.Error(), "total size") {
		t.Fatalf("expected aggregate cap error, got %v", err)
	}
}

func TestSignalCompletionDuplicateSignalDropsArtifacts(t *testing.T) {
	t.Parallel()
	rc, st, root := newSignalContextWithArtifacts(t, "")
	ctx := context.Background()

	first := mustJSON(t, map[string]any{
		"status":  "done",
		"summary": "first",
		"pr_url":  "https://example.com/pr/1",
		"artifacts": map[string]any{
			"first_only": map[string]any{"type": "text", "content": "from first"},
		},
	})
	if _, err := NewSignalCompletion().Handle(ctx, rc, testTeam, first); err != nil {
		t.Fatalf("first Handle: %v", err)
	}

	// Second signal carries different artifact set. Duplicate detection
	// short-circuits before persistence; second call's artifacts are dropped.
	second := mustJSON(t, map[string]any{
		"status":  "done",
		"summary": "second",
		"pr_url":  "https://example.com/pr/2",
		"artifacts": map[string]any{
			"second_only": map[string]any{"type": "text", "content": "from second"},
		},
	})
	if _, err := NewSignalCompletion().Handle(ctx, rc, testTeam, second); err != nil {
		t.Fatalf("second Handle: %v", err)
	}

	ts := loadTeamState(t, st)
	if !reflect.DeepEqual(ts.Artifacts, []string{"first_only"}) {
		t.Fatalf("AgentState.Artifacts = %v, want [first_only]", ts.Artifacts)
	}
	if _, statErr := os.Stat(filepath.Join(root, testTeam, "second_only.json")); statErr == nil {
		t.Errorf("second-call artifact should NOT be persisted")
	}
}

// TestSignalCompletionBlockedToDoneArtifacts confirms the §7.2 recovery
// transition still persists the recovery signal's artifacts. Without this,
// the recipe runtime would see the agent reach status=done with no artifacts
// even though the agent attached them.
func TestSignalCompletionBlockedToDoneArtifacts(t *testing.T) {
	t.Parallel()
	rc, st, _ := newSignalContextWithArtifacts(t, "")
	ctx := context.Background()
	h := NewSignalCompletion()

	first := mustJSON(t, map[string]string{
		"status": "blocked", "summary": "stuck", "reason": "needs human",
	})
	if _, err := h.Handle(ctx, rc, testTeam, first); err != nil {
		t.Fatalf("blocked Handle: %v", err)
	}

	second := mustJSON(t, map[string]any{
		"status":  "done",
		"summary": "shipped after steer",
		"pr_url":  "https://example.com/pr/9",
		"artifacts": map[string]any{
			"final_doc": map[string]any{"type": "text", "content": "the doc"},
		},
	})
	if _, err := h.Handle(ctx, rc, testTeam, second); err != nil {
		t.Fatalf("recovery Handle: %v", err)
	}

	ts := loadTeamState(t, st)
	if ts.SignalStatus != "done" {
		t.Fatalf("status = %q, want done", ts.SignalStatus)
	}
	if !reflect.DeepEqual(ts.Artifacts, []string{"final_doc"}) {
		t.Fatalf("AgentState.Artifacts = %v, want [final_doc]", ts.Artifacts)
	}
	if _, _, err := rc.Artifacts.Get(ctx, rc.RunID, testTeam, "final_doc"); err != nil {
		t.Fatalf("recovery artifact should be on disk: %v", err)
	}
}

// TestSignalCompletionNilArtifactsStoreSafely covers the local-backend / unit-
// test code path: when run.Artifacts is nil, the handler must drop artifacts
// silently rather than panic. The signal status itself still lands.
func TestSignalCompletionNilArtifactsStoreSafely(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{Agents: map[string]store.AgentState{testTeam: {}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rc := newSignalRunContext(t, st) // no Artifacts wired

	if _, err := NewSignalCompletion().Handle(ctx, rc, testTeam, mustJSON(t, map[string]any{
		"status":  "done",
		"summary": "x",
		"pr_url":  "https://example.com/pr/1",
		"artifacts": map[string]any{
			"foo": map[string]any{"type": "text", "content": "v"},
		},
	})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ts := loadTeamState(t, st)
	if ts.SignalStatus != "done" {
		t.Fatalf("signal not recorded: %+v", ts)
	}
	if len(ts.Artifacts) != 0 {
		t.Fatalf("Artifacts should be empty when no store wired: %v", ts.Artifacts)
	}
}

// rawArtifacts builds a payload-shaped map[string]any from typed inputs so the
// per-key cap test can submit a raw JSON RawMessage directly without
// re-encoding it through json.Marshal (which would re-escape the inner JSON).
func rawArtifacts(in map[string]artifactInput) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = map[string]any{
			"type":    v.Type,
			"content": v.Content,
		}
	}
	return out
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
