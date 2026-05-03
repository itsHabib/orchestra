// Package shipfeaturesmoke contains the live-MA smoke for the P2 wiring:
// skill-registration cache → managed agent spec resolution →
// signal_completion dispatch → state + notifications.
//
// The test is opt-in: it runs only when ORCHESTRA_MA_INTEGRATION=1 and an
// Anthropic key is reachable (ANTHROPIC_API_KEY env var or
// <user-config-dir>/orchestra/config.json:api_key). It does NOT run the
// full /ship-feature workflow (PR + reviews + CI) — that is P4. The smoke
// builds a text-only MA team with the ship-feature skill and the
// signal_completion custom tool attached, sends a prompt that instructs
// the agent to call signal_completion immediately, and asserts that
// state.json reflects the signal and notifications.ndjson got an append.
//
// Why the recipe isn't invoked verbatim here: ShipDesignDocs requires a
// repository, which would force the smoke to manage a real GitHub fixture
// and would push the runtime well past the 5-minute target. The recipe is
// covered by unit tests in internal/recipes; this file covers the engine
// half.
package shipfeaturesmoke

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/skills"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/pkg/orchestra"
)

const (
	smokeTeamName  = "ship-smoke"
	smokeSkillName = "ship-feature"
	smokeToolName  = "signal_completion"
	smokeTimeout   = 5 * time.Minute
)

func TestSignalCompletionWiring_LiveMA(t *testing.T) {
	if os.Getenv("ORCHESTRA_MA_INTEGRATION") != "1" {
		t.Skip("set ORCHESTRA_MA_INTEGRATION=1 to enable")
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		if _, err := os.Stat(orchestraConfigPath(t)); err != nil {
			t.Skip("set ANTHROPIC_API_KEY (or populate <user-config-dir>/orchestra/config.json) to enable")
		}
	}

	requireSkillRegistered(t, smokeSkillName)

	wsRoot := t.TempDir()
	if v := os.Getenv("ORCHESTRA_SMOKE_KEEP_WORKSPACE"); v != "" {
		// Persistent dir for debugging when something fails — without
		// this t.TempDir cleanup eats state.json before the operator
		// can look at it.
		wsRoot = v
		_ = os.MkdirAll(wsRoot, 0o755)
	}
	t.Logf("smoke workspace: %s", wsRoot)
	cfg := buildSmokeConfig()
	if res := cfg.Validate(); !res.Valid() {
		t.Fatalf("smoke config invalid: %v", res.Errors)
	}

	ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
	defer cancel()

	// pkg/orchestra.Run takes a workspace dir; the engine writes
	// state.json + notifications.ndjson + per-team logs under it.
	//
	// We do NOT fail on a non-nil Run error. Once the agent calls
	// signal_completion the test's success criterion is satisfied (state
	// + notifications updated). The agent may keep emitting tool calls
	// afterwards which can fail the run for unrelated reasons (e.g. a
	// tool that requires confirmation under MA's v1 surface). The smoke
	// only cares about the P2 signal-completion path; downstream session
	// behavior is out of scope here.
	if _, err := orchestra.Run(ctx, cfg, orchestra.WithWorkspaceDir(wsRoot)); err != nil {
		t.Logf("orchestra.Run returned error (tolerated — assertions check Signal* state): %v", err)
	}

	runDir := wsRoot
	assertSignalState(t, runDir)
	assertNotificationsNDJSON(t, runDir)
}

func buildSmokeConfig() *config.Config {
	cfg := &config.Config{
		Name: "ship-feature-smoke",
		Backend: config.Backend{
			Kind:          "managed_agents",
			ManagedAgents: &config.ManagedAgentsBackend{},
		},
		Defaults: config.Defaults{
			Model:                "opus",
			MaxTurns:             20,
			TimeoutMinutes:       5,
			PermissionMode:       "acceptEdits",
			MAConcurrentSessions: 1,
		},
		Agents: []config.Agent{
			{
				Name: smokeTeamName,
				Lead: config.Lead{Role: "Smoke Probe"},
				Tasks: []config.Task{
					{Summary: "P2 wiring smoke"},
				},
				Context: smokePromptContext(),
				Skills: []config.SkillRef{
					{Name: smokeSkillName, Type: "custom"},
				},
				CustomTools: []config.CustomToolRef{
					{Name: smokeToolName},
				},
			},
		},
	}
	cfg.ResolveDefaults()
	return cfg
}

// smokePromptContext is a deliberately-short directive: the agent should
// confirm it has the skill attached and immediately fire the sentinel.
// We don't drive the full /ship-feature workflow here — see the package
// doc for why.
func smokePromptContext() string {
	return strings.Join([]string{
		"This is a wiring smoke test, not a real workflow run.",
		"You have the ship-feature skill attached to this session and the",
		"signal_completion custom tool available.",
		"",
		"Do not implement anything. Call signal_completion exactly once",
		`with status="done", summary="P2 smoke verified", pr_url="https://example.com/smoke",`,
		"then stop.",
	}, "\n")
}

func requireSkillRegistered(t *testing.T, name string) {
	t.Helper()
	cache := skills.NewFileCache(skills.DefaultCachePath())
	entry, ok, err := cache.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("read skills cache: %v", err)
	}
	if !ok || entry.SkillID == "" {
		t.Fatalf("skill %q is not registered — run `orchestra skills upload %s` first", name, name)
	}
}

func assertSignalState(t *testing.T, runDir string) {
	t.Helper()
	statePath := filepath.Join(runDir, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	var state store.RunState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse state.json: %v", err)
	}
	team := state.Agents[smokeTeamName]
	if team.SignalStatus != "done" {
		t.Fatalf("SignalStatus: want done got %q (full team state: %+v)", team.SignalStatus, team)
	}
	if team.SignalSummary == "" {
		t.Fatal("SignalSummary should be populated")
	}
	if team.SignalPRURL == "" {
		t.Fatal("SignalPRURL should be populated for status=done")
	}
}

func assertNotificationsNDJSON(t *testing.T, runDir string) {
	t.Helper()
	logPath := filepath.Join(runDir, "notifications.ndjson")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read notifications.ndjson: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("notifications.ndjson is empty — handler should have appended an entry")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		t.Fatal("no NDJSON records")
	}
	var found bool
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("parse ndjson line %q: %v", line, err)
		}
		if rec["status"] == "done" && rec["team"] == smokeTeamName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no done-status notification for team %q in:\n%s", smokeTeamName, string(data))
	}
}

func orchestraConfigPath(t *testing.T) string {
	t.Helper()
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "orchestra", "config.json")
}
