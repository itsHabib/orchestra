package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const validRepoURL = "https://github.com/itsHabib/orchestra"

func mustValidTeams() []Team {
	return []Team{
		{
			Name:  "alpha",
			Lead:  Lead{Role: "Lead"},
			Tasks: []Task{{Summary: "do x", Details: "d", Verify: "go test"}},
		},
	}
}

func TestRepository_DefaultsAppliedOnResolve(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Backend: Backend{
			Kind: "managed_agents",
			ManagedAgents: &ManagedAgentsBackend{
				Repository: &RepositorySpec{URL: validRepoURL},
			},
		},
		Teams: mustValidTeams(),
	}
	cfg.ResolveDefaults()

	got := cfg.Backend.ManagedAgents.Repository
	if got.MountPath != DefaultRepoMountPath {
		t.Fatalf("MountPath = %q, want %q", got.MountPath, DefaultRepoMountPath)
	}
	if got.DefaultBranch != DefaultRepoDefaultBranch {
		t.Fatalf("DefaultBranch = %q, want %q", got.DefaultBranch, DefaultRepoDefaultBranch)
	}
}

func TestRepository_ValidateURLFailure(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Backend: Backend{
			Kind: "managed_agents",
			ManagedAgents: &ManagedAgentsBackend{
				Repository: &RepositorySpec{URL: "git@github.com:foo/bar.git"},
			},
		},
		Teams: mustValidTeams(),
	}
	cfg.ResolveDefaults()
	res := cfg.Validate()
	if res.Valid() {
		t.Fatal("expected error for ssh url")
	}
	if !errorsContain(res.Errors, "backend.managed_agents.repository.url") {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
}

func TestRepository_OpenPRRequiresRepository(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Backend: Backend{
			Kind: "managed_agents",
			ManagedAgents: &ManagedAgentsBackend{
				OpenPullRequests: true,
			},
		},
		Teams: mustValidTeams(),
	}
	cfg.ResolveDefaults()
	res := cfg.Validate()
	if res.Valid() {
		t.Fatal("expected error: open_pull_requests without repository")
	}
	if !errorsContain(res.Errors, "open_pull_requests requires a repository") {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
}

func TestRepository_OverrideShadowsProject(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Backend: Backend{
			Kind: "managed_agents",
			ManagedAgents: &ManagedAgentsBackend{
				Repository: &RepositorySpec{URL: validRepoURL},
			},
		},
		Teams: []Team{
			{
				Name:  "alpha",
				Lead:  Lead{Role: "Lead"},
				Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}},
				EnvironmentOverride: EnvironmentOverride{
					Repository: &RepositorySpec{URL: "https://github.com/other/repo"},
				},
			},
		},
	}
	cfg.ResolveDefaults()
	res := cfg.Validate()
	if !res.Valid() {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	got := cfg.Teams[0].EffectiveRepository(cfg)
	if got == nil || got.URL != "https://github.com/other/repo" {
		t.Fatalf("override not effective, got %+v", got)
	}
}

func TestRepository_CrossRepoDependsOnWarns(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Backend: Backend{
			Kind: "managed_agents",
			ManagedAgents: &ManagedAgentsBackend{
				Repository: &RepositorySpec{URL: validRepoURL},
			},
		},
		Teams: []Team{
			{Name: "alpha", Lead: Lead{Role: "L"}, Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}}},
			{
				Name:      "beta",
				Lead:      Lead{Role: "L"},
				Tasks:     []Task{{Summary: "x", Details: "d", Verify: "v"}},
				DependsOn: []string{"alpha"},
				EnvironmentOverride: EnvironmentOverride{
					Repository: &RepositorySpec{URL: "https://github.com/other/repo"},
				},
			},
		},
	}
	cfg.ResolveDefaults()
	res := cfg.Validate()
	if !res.Valid() {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected at least one warning")
	}
	found := false
	for _, w := range res.Warnings {
		if w.Team == "beta" && strings.Contains(w.Message, "different repository") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected cross-repo warning, got: %v", res.Warnings)
	}
}

func TestRepository_YAMLParsesNestedBackend(t *testing.T) {
	src := `
name: p
backend:
  kind: managed_agents
  managed_agents:
    repository:
      url: https://github.com/itsHabib/orchestra
      mount_path: /workspace/main
      default_branch: master
    open_pull_requests: true
teams:
  - name: alpha
    lead:
      role: Lead
    tasks:
      - summary: do x
        details: d
        verify: v
    environment_override:
      repository:
        url: https://github.com/itsHabib/other
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	cfg.ResolveDefaults()

	if cfg.Backend.Kind != "managed_agents" {
		t.Fatalf("Kind = %q", cfg.Backend.Kind)
	}
	if cfg.Backend.ManagedAgents == nil || cfg.Backend.ManagedAgents.Repository == nil {
		t.Fatal("ManagedAgents.Repository nil")
	}
	repo := cfg.Backend.ManagedAgents.Repository
	if repo.URL != "https://github.com/itsHabib/orchestra" || repo.MountPath != "/workspace/main" || repo.DefaultBranch != "master" {
		t.Fatalf("unexpected repository: %+v", repo)
	}
	if !cfg.Backend.ManagedAgents.OpenPullRequests {
		t.Fatal("OpenPullRequests should be true")
	}
	override := cfg.Teams[0].EnvironmentOverride.Repository
	if override == nil || override.URL != "https://github.com/itsHabib/other" {
		t.Fatalf("override = %+v", override)
	}
}
