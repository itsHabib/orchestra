package recipes

import (
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
)

func TestShipDesignDocsBuildsOneTeamPerDoc(t *testing.T) {
	t.Parallel()
	cfg, err := ShipDesignDocs(&ShipDesignDocsParams{
		DocPaths: []string{
			"docs/feat-flag-quiet.md",
			"docs/feat-flag-version.md",
		},
		RepoURL: "https://github.com/itsHabib/orchestra-fixture",
	})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}

	if cfg.Backend.Kind != "managed_agents" {
		t.Fatalf("backend.kind: want managed_agents got %s", cfg.Backend.Kind)
	}
	if cfg.Backend.ManagedAgents == nil {
		t.Fatal("backend.managed_agents missing")
	}
	if !cfg.Backend.ManagedAgents.OpenPullRequests {
		t.Fatal("recipe should default OpenPullRequests=true")
	}
	if cfg.Backend.ManagedAgents.Repository.URL != "https://github.com/itsHabib/orchestra-fixture" {
		t.Fatalf("repo url not propagated: %+v", cfg.Backend.ManagedAgents.Repository)
	}
	if cfg.Backend.ManagedAgents.Repository.DefaultBranch != "main" {
		t.Fatalf("default branch: want main got %s", cfg.Backend.ManagedAgents.Repository.DefaultBranch)
	}
	if cfg.Backend.ManagedAgents.Repository.MountPath != "/workspace/repo" {
		t.Fatalf("mount path: want /workspace/repo got %s", cfg.Backend.ManagedAgents.Repository.MountPath)
	}

	if len(cfg.Teams) != 2 {
		t.Fatalf("teams: want 2 got %d", len(cfg.Teams))
	}
	wantNames := []string{"ship-feat-flag-quiet", "ship-feat-flag-version"}
	for i, want := range wantNames {
		assertShipTeamShape(t, &cfg.Teams[i], want)
	}
}

func assertShipTeamShape(t *testing.T, team *config.Team, wantName string) {
	t.Helper()
	if team.Name != wantName {
		t.Fatalf("team name: want %s got %s", wantName, team.Name)
	}
	if len(team.DependsOn) != 0 {
		t.Fatalf("team %s should be tier-0 (no depends_on), got %v", team.Name, team.DependsOn)
	}
	if len(team.Skills) != 1 || team.Skills[0].Name != "ship-feature" || team.Skills[0].Type != "custom" {
		t.Fatalf("team %s skills: want [{ship-feature, custom}] got %+v", team.Name, team.Skills)
	}
	if len(team.CustomTools) != 1 || team.CustomTools[0].Name != "signal_completion" {
		t.Fatalf("team %s custom_tools: want [{signal_completion}] got %+v", team.Name, team.CustomTools)
	}
	if len(team.Tasks) == 0 {
		t.Fatalf("team %s has no tasks", team.Name)
	}
}

func TestShipDesignDocsAppliesDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := ShipDesignDocs(&ShipDesignDocsParams{
		DocPaths: []string{"docs/x.md"},
		RepoURL:  "https://github.com/x/y",
	})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	if cfg.Defaults.Model != "opus" {
		t.Fatalf("default model: want opus got %s", cfg.Defaults.Model)
	}
	if cfg.Defaults.MAConcurrentSessions != 4 {
		t.Fatalf("default concurrency: want 4 got %d", cfg.Defaults.MAConcurrentSessions)
	}
	if cfg.Defaults.TimeoutMinutes != 90 {
		t.Fatalf("default timeout: want 90m got %dm", cfg.Defaults.TimeoutMinutes)
	}
	if cfg.Name != "ship-design-docs" {
		t.Fatalf("default run name: want ship-design-docs got %s", cfg.Name)
	}
}

func TestShipDesignDocsHonorsOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := ShipDesignDocs(&ShipDesignDocsParams{
		DocPaths:            []string{"docs/x.md"},
		RepoURL:             "https://github.com/x/y",
		DefaultBranch:       "trunk",
		Model:               "sonnet",
		Concurrency:         8,
		Timeout:             45 * time.Minute,
		DisablePullRequests: true,
		RunName:             "custom-run",
	})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	if cfg.Backend.ManagedAgents.Repository.DefaultBranch != "trunk" {
		t.Fatalf("default branch override lost: %s", cfg.Backend.ManagedAgents.Repository.DefaultBranch)
	}
	if cfg.Defaults.Model != "sonnet" {
		t.Fatalf("model override lost: %s", cfg.Defaults.Model)
	}
	if cfg.Defaults.MAConcurrentSessions != 8 {
		t.Fatalf("concurrency override lost: %d", cfg.Defaults.MAConcurrentSessions)
	}
	if cfg.Defaults.TimeoutMinutes != 45 {
		t.Fatalf("timeout override lost: %d", cfg.Defaults.TimeoutMinutes)
	}
	if cfg.Backend.ManagedAgents.OpenPullRequests {
		t.Fatal("DisablePullRequests=true should set OpenPullRequests=false")
	}
	if cfg.Name != "custom-run" {
		t.Fatalf("run name override lost: %s", cfg.Name)
	}
}

func TestShipDesignDocsRequiresDocsAndRepo(t *testing.T) {
	t.Parallel()
	if _, err := ShipDesignDocs(&ShipDesignDocsParams{RepoURL: "https://github.com/x/y"}); err == nil {
		t.Fatal("expected error on empty doc paths")
	}
	if _, err := ShipDesignDocs(&ShipDesignDocsParams{DocPaths: []string{"docs/x.md"}}); err == nil {
		t.Fatal("expected error on empty repo URL")
	}
	if _, err := ShipDesignDocs(&ShipDesignDocsParams{
		DocPaths: []string{"   "},
		RepoURL:  "https://github.com/x/y",
	}); err == nil {
		t.Fatal("expected error on whitespace-only doc path")
	}
}

func TestShipDesignDocsDisambiguatesDuplicateBasenames(t *testing.T) {
	t.Parallel()
	cfg, err := ShipDesignDocs(&ShipDesignDocsParams{
		DocPaths: []string{"docs/foo.md", "other/foo.md", "third/foo.md"},
		RepoURL:  "https://github.com/x/y",
	})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	want := []string{"ship-foo", "ship-foo-2", "ship-foo-3"}
	for i, w := range want {
		if cfg.Teams[i].Name != w {
			t.Fatalf("team %d: want %s got %s", i, w, cfg.Teams[i].Name)
		}
	}
}

func TestShipDesignDocsConfigPassesValidate(t *testing.T) {
	t.Parallel()
	cfg, err := ShipDesignDocs(&ShipDesignDocsParams{
		DocPaths: []string{"docs/feat.md"},
		RepoURL:  "https://github.com/x/y",
	})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	res := cfg.Validate()
	if !res.Valid() {
		t.Fatalf("recipe-generated config failed Validate: %v", res.Errors)
	}
}

func TestShipDesignDocsConfigPassesResourceValidationWhenRegistered(t *testing.T) {
	t.Parallel()
	cfg, err := ShipDesignDocs(&ShipDesignDocsParams{
		DocPaths: []string{"docs/feat.md"},
		RepoURL:  "https://github.com/x/y",
	})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	res := cfg.ValidateResourceReferences(
		map[string]bool{"ship-feature": true},
		map[string]bool{"signal_completion": true},
	)
	if !res.Valid() {
		t.Fatalf("recipe config should validate when resources are registered: %v", res.Errors)
	}
}

func TestShipDesignDocsConfigFailsResourceValidationWhenSkillMissing(t *testing.T) {
	t.Parallel()
	cfg, err := ShipDesignDocs(&ShipDesignDocsParams{
		DocPaths: []string{"docs/feat.md"},
		RepoURL:  "https://github.com/x/y",
	})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	res := cfg.ValidateResourceReferences(
		map[string]bool{},
		map[string]bool{"signal_completion": true},
	)
	if res.Valid() {
		t.Fatal("expected error when ship-feature skill not registered")
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected at least one error about missing skill")
	}
}

func TestTeamNameForDocSlugifies(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"docs/feat-flag-quiet.md", "ship-feat-flag-quiet"},
		{"FOO BAR.md", "ship-foo-bar"},
		{"docs/Already_Snake_Case.md", "ship-already-snake-case"},
		{"weird!!chars*.md", "ship-weird-chars"},
	}
	for _, c := range cases {
		got := teamNameForDoc(c.in)
		if got != c.want {
			t.Fatalf("teamNameForDoc(%q): want %s got %s", c.in, c.want, got)
		}
	}
}

func TestRecipeReturnsBackendCompatibleConfig(t *testing.T) {
	t.Parallel()
	cfg, err := ShipDesignDocs(&ShipDesignDocsParams{
		DocPaths: []string{"docs/x.md"},
		RepoURL:  "https://github.com/x/y",
	})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	// Re-validate after ResolveDefaults to confirm the recipe's defaults
	// don't drift from what the engine would compute itself.
	cfg.ResolveDefaults()
	res := cfg.Validate()
	if !res.Valid() {
		t.Fatalf("post-resolve validation: %v", res.Errors)
	}
	if cfg.Defaults.PermissionMode == "" {
		t.Fatal("permission mode should be set")
	}
	if cfg.Defaults.MaxTurns == 0 {
		t.Fatal("max turns should be set")
	}
}

// Compile-time assertion: keep the recipe params reachable from the public
// API of the package. If a refactor renames the fields, this catches it
// before downstream code breaks.
var _ = ShipDesignDocsParams{
	DocPaths:            nil,
	RepoURL:             "",
	DefaultBranch:       "",
	Model:               "",
	Concurrency:         0,
	Timeout:             0,
	DisablePullRequests: false,
	RunName:             "",
}

var _ config.Config = config.Config{}
