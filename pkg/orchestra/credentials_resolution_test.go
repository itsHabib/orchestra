package orchestra

import (
	"context"
	"strings"
	"testing"

	"github.com/itsHabib/orchestra/internal/config"
)

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
