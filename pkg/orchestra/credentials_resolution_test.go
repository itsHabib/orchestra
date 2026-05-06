package orchestra

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/event"
)

// recordingEmitter captures every emitted event for assertion. Safe for
// concurrent use (the engine emits from multiple team goroutines) so tests
// can share one instance across the run if needed.
type recordingEmitter struct {
	mu     sync.Mutex
	events []event.Event
}

//nolint:gocritic // Event-by-value matches the event.Emitter interface contract.
func (r *recordingEmitter) Emit(ev event.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingEmitter) snapshot() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.events...)
}

// TestEmitMACredentialWarning_PointsAtTrackingIssue locks in the
// dogfood §B2 follow-up: when an MA-backed run resolves
// requires_credentials, the warning must name the SDK gap, the working
// substitute (github_repository ResourceRef), the tracking issue, and the
// dogfood section that surfaced the scope. This message is the chat-side
// LLM's only signal that secrets are not reaching the sandbox; if the
// pointers regress, the next dogfood operator has to re-derive the gap
// from source.
func TestEmitMACredentialWarning_PointsAtTrackingIssue(t *testing.T) {
	t.Parallel()
	rec := &recordingEmitter{}
	emitMACredentialWarning(rec, map[string]map[string]string{
		"alpha": {"GITHUB_TOKEN": "ghp_x"},
		"beta":  {"POLYGON_API_KEY": "pk_y"},
	})

	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("emitted %d events, want 1: %+v", len(events), events)
	}
	got := events[0]
	if got.Kind != event.KindWarn {
		t.Errorf("Kind = %v, want KindWarn", got.Kind)
	}
	for _, want := range []string{
		"GITHUB_TOKEN",
		"POLYGON_API_KEY",
		"github_repository ResourceRef",
		"github.com/itsHabib/orchestra/issues/42",
		"docs/feedback-phase-a-dogfood.md",
	} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("warning text missing %q\nfull text: %s", want, got.Message)
		}
	}
}

// TestEmitMACredentialWarning_NoOpOnEmptyEnv pins the no-warn path:
// runs without requires_credentials must not emit a misleading warning
// just because the function was called.
func TestEmitMACredentialWarning_NoOpOnEmptyEnv(t *testing.T) {
	t.Parallel()
	rec := &recordingEmitter{}
	emitMACredentialWarning(rec, nil)
	emitMACredentialWarning(rec, map[string]map[string]string{"alpha": {}})
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("expected no events, got %+v", got)
	}
}

// TestResolveAgentCredentials_RejectsEnvNameCollision pins the dual-key
// guard reviewers (Codex P2 + Copilot) flagged: two credential names
// like `foo-bar` and `foo_bar` both normalize to `FOO_BAR`. Picking
// either one silently means the injected secret depends on map
// iteration order. Fail fast at run start with a message naming both
// colliding credential names.
//
// Resolution looks up env vars first, so seeding the canonical
// upper-snake env names is enough to satisfy the resolver — no need to
// touch the user-scoped credentials.json file.
func TestResolveAgentCredentials_RejectsEnvNameCollision(t *testing.T) {
	t.Setenv("FOO_BAR", "ghp_secret")
	cfg := &Config{
		Name: "p",
		Agents: []config.Agent{
			{
				Name:                "alpha",
				RequiresCredentials: []string{"foo-bar", "foo_bar"},
				Lead:                config.Lead{Role: "L"},
				Tasks:               []config.Task{{Summary: "x"}},
			},
		},
	}

	_, err := resolveAgentCredentials(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected collision error")
	}
	msg := err.Error()
	for _, want := range []string{"foo-bar", "foo_bar", "FOO_BAR"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}
