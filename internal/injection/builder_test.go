package injection

import (
	"strings"
	"testing"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/workspace"
)

func TestBuildPrompt_SoloNoDeps(t *testing.T) {
	team := config.Team{
		Name:    "backend",
		Lead:    config.Lead{Role: "Backend Lead"},
		Context: "Go 1.22, Chi router\n",
		Tasks: []config.Task{
			{
				Summary:      "Build API",
				Details:      "REST endpoints with validation",
				Deliverables: []string{"src/api/"},
				Verify:       "go build ./...",
			},
		},
	}
	prompt := BuildPrompt(&team, "my-project", nil, &config.Config{}, nil, "", "", Capabilities{})

	checks := []string{
		"You are: Backend Lead",
		"Project: my-project",
		"Go 1.22, Chi router",
		"### Task: Build API",
		"REST endpoints with validation",
		"src/api/",
		"`go build ./...`",
		"Work through your tasks in order",
	}
	for _, c := range checks {
		if !strings.Contains(prompt, c) {
			t.Errorf("prompt missing %q", c)
		}
	}
	// Should NOT contain team instructions
	if strings.Contains(prompt, "TeamCreate") {
		t.Error("solo agent prompt should not contain TeamCreate instructions")
	}
}

func TestBuildPrompt_SoloWithDeps(t *testing.T) {
	team := config.Team{
		Name:      "frontend",
		Lead:      config.Lead{Role: "Frontend Lead"},
		DependsOn: []string{"backend"},
		Tasks:     []config.Task{{Summary: "Build UI", Details: "React", Verify: "npm build"}},
	}
	state := &workspace.State{
		Teams: map[string]workspace.TeamState{
			"backend": {
				Status:        "done",
				ResultSummary: "Built 12 REST endpoints",
				Artifacts:     []string{"src/api/", "src/db/"},
			},
		},
	}
	cfg := &config.Config{
		Teams: []config.Team{
			{Name: "backend", Lead: config.Lead{Role: "Backend Lead"}},
		},
	}
	prompt := BuildPrompt(&team, "proj", state, cfg, nil, "", "", Capabilities{})

	if !strings.Contains(prompt, "Context from Previous Teams") {
		t.Error("missing previous teams section")
	}
	if !strings.Contains(prompt, "backend (Backend Lead)") {
		t.Error("missing backend reference with role")
	}
	if !strings.Contains(prompt, "Built 12 REST endpoints") {
		t.Error("missing result summary")
	}
}

func TestBuildPrompt_TeamLeadWithMembers(t *testing.T) {
	team := config.Team{
		Name: "backend",
		Lead: config.Lead{Role: "Backend Lead"},
		Members: []config.Member{
			{Role: "API Engineer", Focus: "REST endpoints"},
			{Role: "DB Engineer", Focus: "Postgres schema"},
		},
		Tasks: []config.Task{
			{Summary: "Build API", Details: "REST", Verify: "go build"},
			{Summary: "Build DB", Details: "Postgres", Verify: "go test"},
		},
	}
	prompt := BuildPrompt(&team, "proj", nil, &config.Config{}, nil, ".orchestra/messages/backend", ".orchestra/messages", Capabilities{})

	if !strings.Contains(prompt, "You have 2 teammates") {
		t.Error("missing team count")
	}
	if !strings.Contains(prompt, "API Engineer: REST endpoints") {
		t.Error("missing member listing")
	}
	if !strings.Contains(prompt, "TeamCreate") {
		t.Error("missing TeamCreate instruction")
	}
	if !strings.Contains(prompt, "SendMessage") {
		t.Error("missing SendMessage instruction")
	}
}

func TestBuildPrompt_TeamLeadWithDeps(t *testing.T) {
	team := config.Team{
		Name:      "frontend",
		Lead:      config.Lead{Role: "Frontend Lead"},
		DependsOn: []string{"backend"},
		Members:   []config.Member{{Role: "UI Dev", Focus: "components"}},
		Tasks:     []config.Task{{Summary: "Build UI", Details: "React", Verify: "npm build"}},
	}
	state := &workspace.State{
		Teams: map[string]workspace.TeamState{
			"backend": {Status: "done", ResultSummary: "API done"},
		},
	}
	cfg := &config.Config{
		Teams: []config.Team{{Name: "backend", Lead: config.Lead{Role: "Backend Lead"}}},
	}
	prompt := BuildPrompt(&team, "proj", state, cfg, nil, "", "", Capabilities{})

	if !strings.Contains(prompt, "TeamCreate") {
		t.Error("missing TeamCreate instruction for team lead")
	}
	if !strings.Contains(prompt, "Context from Previous Teams") {
		t.Error("missing deps section")
	}
}

func TestBuildPrompt_ContextInjectedVerbatim(t *testing.T) {
	ctx := "Tech stack: Go 1.22, Chi router, sqlc for query generation.\nAuth: JWT."
	team := config.Team{
		Name:    "backend",
		Lead:    config.Lead{Role: "Lead"},
		Context: ctx,
		Tasks:   []config.Task{{Summary: "t", Details: "d", Verify: "v"}},
	}
	prompt := BuildPrompt(&team, "p", nil, &config.Config{}, nil, "", "", Capabilities{})
	if !strings.Contains(prompt, ctx) {
		t.Error("context not injected verbatim")
	}
}

func TestBuildPrompt_TaskFieldsPresent(t *testing.T) {
	team := config.Team{
		Name: "t",
		Lead: config.Lead{Role: "Lead"},
		Tasks: []config.Task{{
			Summary:      "Build the thing",
			Details:      "Detailed requirements here",
			Deliverables: []string{"src/foo.go", "src/bar.go"},
			Verify:       "go test -v ./...",
		}},
	}
	prompt := BuildPrompt(&team, "p", nil, &config.Config{}, nil, "", "", Capabilities{})
	for _, s := range []string{"Build the thing", "Detailed requirements here", "src/foo.go, src/bar.go", "`go test -v ./...`"} {
		if !strings.Contains(prompt, s) {
			t.Errorf("missing %q in prompt", s)
		}
	}
}

func TestBuildPrompt_WithTierPeers(t *testing.T) {
	cfg := &config.Config{
		Teams: []config.Team{
			{
				Name: "frontend",
				Lead: config.Lead{Role: "Frontend Lead"},
				Tasks: []config.Task{
					{Summary: "Build dashboard UI"},
					{Summary: "Implement auth flow"},
				},
			},
			{
				Name: "backend",
				Lead: config.Lead{Role: "Backend Lead"},
				Tasks: []config.Task{
					{Summary: "Build API"},
				},
			},
			{
				Name: "devops",
				Lead: config.Lead{Role: "DevOps Lead"},
				Tasks: []config.Task{
					{Summary: "Set up Docker"},
					{Summary: "Configure GitHub Actions"},
				},
			},
		},
	}

	tierPeers := []string{"frontend", "backend", "devops"}

	// Test from backend's perspective
	team := *cfg.TeamByName("backend")
	prompt := BuildPrompt(&team, "proj", nil, cfg, tierPeers, "", "", Capabilities{})

	// Should contain the section header
	if !strings.Contains(prompt, "## Parallel Teams (Your Tier)") {
		t.Error("missing parallel teams section header")
	}
	// Should list frontend and devops but not backend itself
	if !strings.Contains(prompt, "frontend (Frontend Lead): Build dashboard UI, Implement auth flow") {
		t.Error("missing frontend peer entry")
	}
	if !strings.Contains(prompt, "devops (DevOps Lead): Set up Docker, Configure GitHub Actions") {
		t.Error("missing devops peer entry")
	}
	if strings.Contains(prompt, "- backend (Backend Lead)") {
		t.Error("should not list self as peer")
	}
}

func TestBuildPrompt_NilTierPeers(t *testing.T) {
	team := config.Team{
		Name:  "solo",
		Lead:  config.Lead{Role: "Solo Lead"},
		Tasks: []config.Task{{Summary: "Do stuff", Verify: "true"}},
	}
	prompt := BuildPrompt(&team, "proj", nil, &config.Config{}, nil, "", "", Capabilities{})

	if strings.Contains(prompt, "Parallel Teams") {
		t.Error("nil tierPeers should not produce parallel teams section")
	}
}

func TestBuildPrompt_SingleTierPeer(t *testing.T) {
	// When a team is alone in its tier, tierPeers has only itself — section should be skipped
	cfg := &config.Config{
		Teams: []config.Team{
			{Name: "only", Lead: config.Lead{Role: "Lead"}, Tasks: []config.Task{{Summary: "Work"}}},
		},
	}
	prompt := BuildPrompt(&cfg.Teams[0], "proj", nil, cfg, []string{"only"}, "", "", Capabilities{})

	if strings.Contains(prompt, "Parallel Teams") {
		t.Error("single-team tier should not produce parallel teams section")
	}
}

func TestBuildPrompt_LocalBackendByteIdenticalWhenNoArtifactPublish(t *testing.T) {
	team := config.Team{
		Name:  "backend",
		Lead:  config.Lead{Role: "Backend Lead"},
		Tasks: []config.Task{{Summary: "Build", Details: "d", Verify: "v"}},
	}
	zero := BuildPrompt(&team, "p", nil, &config.Config{}, nil, "", "", Capabilities{})
	nilSpec := BuildPrompt(&team, "p", nil, &config.Config{}, nil, "", "", Capabilities{ArtifactPublish: nil})
	if zero != nilSpec {
		t.Fatal("zero-value Capabilities and explicit nil ArtifactPublish must produce identical output")
	}
	if strings.Contains(zero, "Artifact delivery") {
		t.Fatal("local prompt must not contain artifact-delivery section")
	}
}

func TestBuildPrompt_ArtifactPublishSection(t *testing.T) {
	team := config.Team{
		Name:      "team-b",
		Lead:      config.Lead{Role: "B Lead"},
		Tasks:     []config.Task{{Summary: "Edit README", Details: "d", Verify: "v"}},
		DependsOn: []string{"team-a"},
	}
	state := &workspace.State{
		Teams: map[string]workspace.TeamState{
			"team-a": {
				Status:        "done",
				ResultSummary: "edited README",
				RepositoryArtifacts: []workspace.RepositoryArtifact{
					{Branch: "orchestra/team-a-run-123", CommitSHA: "deadbeef"},
				},
			},
		},
	}
	cfg := &config.Config{
		Teams: []config.Team{{Name: "team-a", Lead: config.Lead{Role: "A Lead"}}},
	}
	caps := Capabilities{ArtifactPublish: &ArtifactPublishSpec{
		MountPath:  "/workspace/repo",
		BranchName: "orchestra/team-b-run-123",
		UpstreamMounts: []UpstreamMount{
			{TeamName: "team-a", MountPath: "/workspace/upstream/team-a", Branch: "orchestra/team-a-run-123"},
		},
	}}
	prompt := BuildPrompt(&team, "p", state, cfg, nil, "", "", caps)

	checks := []string{
		"## Artifact delivery",
		"`/workspace/repo`",
		"`orchestra/team-b-run-123`",
		"Do NOT open a pull request",
		"team-a: `/workspace/upstream/team-a` (branch `orchestra/team-a-run-123`)",
	}
	for _, c := range checks {
		if !strings.Contains(prompt, c) {
			t.Errorf("artifact prompt missing %q", c)
		}
	}
}

func TestBuildPrompt_ArtifactPublishSectionTier0NoUpstreams(t *testing.T) {
	team := config.Team{
		Name:  "team-a",
		Lead:  config.Lead{Role: "A Lead"},
		Tasks: []config.Task{{Summary: "Edit README", Details: "d", Verify: "v"}},
	}
	caps := Capabilities{ArtifactPublish: &ArtifactPublishSpec{
		MountPath:  "/workspace/repo",
		BranchName: "orchestra/team-a-run-123",
	}}
	prompt := BuildPrompt(&team, "p", nil, &config.Config{}, nil, "", "", caps)

	if !strings.Contains(prompt, "## Artifact delivery") {
		t.Fatal("artifact section missing")
	}
	if strings.Contains(prompt, "mounted read-only") {
		t.Fatal("tier-0 team must not list upstream mounts")
	}
}
