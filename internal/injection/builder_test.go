package injection

import (
	"strings"
	"testing"

	"github.com/michaelhabib/orchestra/internal/config"
	"github.com/michaelhabib/orchestra/internal/workspace"
)

func TestBuildPrompt_SoloNoDeps(t *testing.T) {
	team := config.Team{
		Name: "backend",
		Lead: config.Lead{Role: "Backend Lead"},
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
	prompt := BuildPrompt(team, "my-project", nil, &config.Config{})

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
	prompt := BuildPrompt(team, "proj", state, cfg)

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
	prompt := BuildPrompt(team, "proj", nil, &config.Config{})

	if !strings.Contains(prompt, "You have 2 teammates") {
		t.Error("missing team count")
	}
	if !strings.Contains(prompt, "API Engineer: REST endpoints") {
		t.Error("missing member listing")
	}
	if !strings.Contains(prompt, "TeamCreate") {
		t.Error("missing TeamCreate instruction")
	}
	if !strings.Contains(prompt, "Spawn teammates in parallel") {
		t.Error("missing spawn instruction")
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
	prompt := BuildPrompt(team, "proj", state, cfg)

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
	prompt := BuildPrompt(team, "p", nil, &config.Config{})
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
	prompt := BuildPrompt(team, "p", nil, &config.Config{})
	for _, s := range []string{"Build the thing", "Detailed requirements here", "src/foo.go, src/bar.go", "`go test -v ./...`"} {
		if !strings.Contains(prompt, s) {
			t.Errorf("missing %q in prompt", s)
		}
	}
}
