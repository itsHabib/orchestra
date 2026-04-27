package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidate_ValidConfig(t *testing.T) {
	cfg := &Config{
		Name: "test-project",
		Teams: []Team{
			{
				Name: "backend",
				Lead: Lead{Role: "Backend Lead"},
				Tasks: []Task{
					{Summary: "Build API", Details: "REST API", Verify: "go build"},
					{Summary: "Build DB", Details: "Postgres", Verify: "go build"},
				},
			},
		},
	}
	res := cfg.Validate()
	if !res.Valid() {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	if res.Config != cfg {
		t.Fatalf("Config not populated on valid result")
	}
}

func TestValidate_EmptyName(t *testing.T) {
	cfg := &Config{
		Teams: []Team{{Name: "a", Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}}}},
	}
	res := cfg.Validate()
	if res.Valid() {
		t.Fatal("expected validation to fail for empty project name")
	}
	if !errorsContain(res.Errors, "project name is required") {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
}

func TestValidate_EmptyTeams(t *testing.T) {
	cfg := &Config{Name: "p"}
	res := cfg.Validate()
	if res.Valid() {
		t.Fatal("expected validation to fail for empty teams")
	}
}

func TestValidate_DuplicateTeamNames(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Teams: []Team{
			{Name: "a", Tasks: []Task{{Summary: "x"}}},
			{Name: "a", Tasks: []Task{{Summary: "y"}}},
		},
	}
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "duplicate team name") {
		t.Fatalf("expected duplicate team name error, got: %v", res.Errors)
	}
}

func TestValidate_SelfReference(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Teams: []Team{
			{Name: "a", Tasks: []Task{{Summary: "x"}}, DependsOn: []string{"a"}},
		},
	}
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "cannot depend on itself") {
		t.Fatalf("expected self-reference error, got: %v", res.Errors)
	}
}

func TestValidate_UnknownDependency(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Teams: []Team{
			{Name: "a", Tasks: []Task{{Summary: "x"}}, DependsOn: []string{"b"}},
		},
	}
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "unknown team") {
		t.Fatalf("expected unknown team error, got: %v", res.Errors)
	}
}

func TestValidate_Cycle(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Teams: []Team{
			{Name: "a", Tasks: []Task{{Summary: "x"}}, DependsOn: []string{"b"}},
			{Name: "b", Tasks: []Task{{Summary: "y"}}, DependsOn: []string{"a"}},
		},
	}
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "cycle") {
		t.Fatalf("expected cycle error, got: %v", res.Errors)
	}
}

func TestValidate_EmptyTaskSummary(t *testing.T) {
	cfg := &Config{
		Name:  "p",
		Teams: []Team{{Name: "a", Tasks: []Task{{Summary: ""}}}},
	}
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "empty summary") {
		t.Fatalf("expected empty summary error, got: %v", res.Errors)
	}
}

func TestValidate_NoTasks(t *testing.T) {
	cfg := &Config{
		Name:  "p",
		Teams: []Team{{Name: "a"}},
	}
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "at least one task") {
		t.Fatalf("expected no tasks error, got: %v", res.Errors)
	}
}

func TestValidate_TeamSizeWarning(t *testing.T) {
	members := make([]Member, 6)
	for i := range members {
		members[i] = Member{Role: "dev"}
	}
	cfg := &Config{
		Name: "p",
		Teams: []Team{{
			Name:    "a",
			Members: members,
			Tasks: []Task{
				{Summary: "t1", Details: "d", Verify: "v"},
				{Summary: "t2", Details: "d", Verify: "v"},
				{Summary: "t3", Details: "d", Verify: "v"},
				{Summary: "t4", Details: "d", Verify: "v"},
				{Summary: "t5", Details: "d", Verify: "v"},
				{Summary: "t6", Details: "d", Verify: "v"},
				{Summary: "t7", Details: "d", Verify: "v"},
				{Summary: "t8", Details: "d", Verify: "v"},
				{Summary: "t9", Details: "d", Verify: "v"},
				{Summary: "t10", Details: "d", Verify: "v"},
				{Summary: "t11", Details: "d", Verify: "v"},
				{Summary: "t12", Details: "d", Verify: "v"},
			},
		}},
	}
	res := cfg.Validate()
	if !res.Valid() {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "members") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected team size warning")
	}
}

func TestValidate_TaskQualityWarning(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Teams: []Team{{
			Name:  "a",
			Tasks: []Task{{Summary: "do stuff"}},
		}},
	}
	res := cfg.Validate()
	if !res.Valid() {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected task quality warnings for empty details/verify")
	}
}

func TestBackendYAMLForms(t *testing.T) {
	tests := []struct {
		name string
		yml  string
		want string
	}{
		{name: "scalar", yml: "backend: managed_agents\n", want: "managed_agents"},
		{name: "mapping", yml: "backend:\n  kind: managed_agents\n", want: "managed_agents"},
		{name: "missing", yml: "", want: "local"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.yml), &cfg); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			cfg.ResolveDefaults()
			if cfg.Backend.Kind != tc.want {
				t.Fatalf("Backend.Kind=%q, want %q", cfg.Backend.Kind, tc.want)
			}
		})
	}
}

func TestValidate_BackendKind(t *testing.T) {
	cfg := &Config{
		Name:    "p",
		Backend: Backend{Kind: "bogus"},
		Teams:   []Team{{Name: "a", Tasks: []Task{{Summary: "x"}}}},
	}
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "backend.kind") {
		t.Fatalf("expected backend.kind validation error, got %v", res.Errors)
	}
}

func TestValidate_ManagedAgentsWarnings(t *testing.T) {
	cfg := &Config{
		Name:        "p",
		Backend:     Backend{Kind: "managed_agents"},
		Coordinator: Coordinator{Enabled: true},
		Teams: []Team{{
			Name:    "a",
			Members: []Member{{Role: "dev"}},
			Tasks:   []Task{{Summary: "x", Details: "d", Verify: "v"}},
		}},
	}
	res := cfg.Validate()
	if !res.Valid() {
		t.Fatalf("Validate: %v", res.Errors)
	}
	var coordinator, members bool
	for _, w := range res.Warnings {
		coordinator = coordinator || strings.Contains(w.Message, "coordinator is not supported under backend.kind=managed_agents")
		members = members || strings.Contains(w.Message, "members are not supported under backend.kind=managed_agents")
	}
	if !coordinator || !members {
		t.Fatalf("warnings=%v, want coordinator and members warnings", res.Warnings)
	}
}

func TestResolveDefaults(t *testing.T) {
	cfg := &Config{
		Teams: []Team{{Name: "a", Lead: Lead{Role: "dev"}}},
	}
	cfg.ResolveDefaults()
	if cfg.Backend.Kind != "local" {
		t.Fatalf("expected local backend, got %s", cfg.Backend.Kind)
	}
	if cfg.Defaults.Model != "sonnet" {
		t.Fatalf("expected sonnet, got %s", cfg.Defaults.Model)
	}
	if cfg.Defaults.MaxTurns != 200 {
		t.Fatalf("expected 200, got %d", cfg.Defaults.MaxTurns)
	}
	if cfg.Defaults.MAConcurrentSessions != DefaultMAConcurrentSessions {
		t.Fatalf("MAConcurrentSessions=%d, want %d", cfg.Defaults.MAConcurrentSessions, DefaultMAConcurrentSessions)
	}
	if cfg.Teams[0].Lead.Model != "sonnet" {
		t.Fatalf("expected team model sonnet, got %s", cfg.Teams[0].Lead.Model)
	}
}

func TestResolveDefaults_PreservesMAConcurrentSessionsOverride(t *testing.T) {
	cfg := &Config{
		Defaults: Defaults{MAConcurrentSessions: 5},
		Teams:    []Team{{Name: "a", Lead: Lead{Role: "dev"}}},
	}
	cfg.ResolveDefaults()
	if cfg.Defaults.MAConcurrentSessions != 5 {
		t.Fatalf("MAConcurrentSessions=%d, want override 5", cfg.Defaults.MAConcurrentSessions)
	}
}

func TestValidate_NegativeMAConcurrentSessions(t *testing.T) {
	cfg := &Config{
		Name:     "p",
		Defaults: Defaults{MAConcurrentSessions: -1},
		Teams:    []Team{{Name: "a", Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}}}},
	}
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "ma_concurrent_sessions") {
		t.Fatalf("expected ma_concurrent_sessions validation error, got %v", res.Errors)
	}
}

func TestResolveDefaults_PreservesOverrides(t *testing.T) {
	cfg := &Config{
		Defaults: Defaults{Model: "haiku"},
		Teams:    []Team{{Name: "a", Lead: Lead{Role: "dev", Model: "opus"}}},
	}
	cfg.ResolveDefaults()
	if cfg.Teams[0].Lead.Model != "opus" {
		t.Fatalf("expected opus override preserved, got %s", cfg.Teams[0].Lead.Model)
	}
}

func TestTeamByName(t *testing.T) {
	cfg := &Config{
		Teams: []Team{{Name: "a"}, {Name: "b"}},
	}
	if cfg.TeamByName("a") == nil {
		t.Fatal("expected to find team a")
	}
	if cfg.TeamByName("c") != nil {
		t.Fatal("expected nil for unknown team")
	}
}

func TestHasMembers(t *testing.T) {
	team := &Team{Name: "a"}
	if team.HasMembers() {
		t.Fatal("expected no members")
	}
	team.Members = []Member{{Role: "dev"}}
	if !team.HasMembers() {
		t.Fatal("expected members")
	}
}

// errorsContain reports whether any ConfigError in errs has a Message
// containing substr. Used by tests that previously asserted on the
// joined err.Error() string.
func errorsContain(errs []ConfigError, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

// warningsContain reports whether any Warning in warnings has a Message
// containing substr.
func warningsContain(warnings []Warning, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w.Message, substr) {
			return true
		}
	}
	return false
}

func TestValidate_TeamSkillsRoundTrip(t *testing.T) {
	t.Parallel()
	yamlSrc := `
name: p
backend:
  kind: managed_agents
  managed_agents:
    repository:
      url: https://github.com/x/y
defaults:
  model: opus
  max_turns: 200
  permission_mode: acceptEdits
  timeout_minutes: 90
  inbox_poll_interval: 5m
teams:
  - name: ship
    lead:
      role: Implementer
    tasks:
      - summary: ship the doc
        details: do it
        verify: 'true'
    skills:
      - name: ship-feature
        type: custom
      - name: built-in
        type: anthropic
        version: v1
    custom_tools:
      - name: signal_completion
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlSrc), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.Teams) != 1 {
		t.Fatalf("teams: %+v", cfg.Teams)
	}
	team := cfg.Teams[0]
	if len(team.Skills) != 2 {
		t.Fatalf("skills: %+v", team.Skills)
	}
	if team.Skills[0].Name != "ship-feature" || team.Skills[0].Type != "custom" {
		t.Fatalf("skill 0: %+v", team.Skills[0])
	}
	if team.Skills[1].Type != "anthropic" || team.Skills[1].Version != "v1" {
		t.Fatalf("skill 1: %+v", team.Skills[1])
	}
	if len(team.CustomTools) != 1 || team.CustomTools[0].Name != "signal_completion" {
		t.Fatalf("custom_tools: %+v", team.CustomTools)
	}
}

func TestValidate_SkillShape_EmptyName(t *testing.T) {
	t.Parallel()
	cfg := managedConfigWithTeam(&Team{
		Name:   "ship",
		Lead:   Lead{Role: "Lead"},
		Tasks:  []Task{{Summary: "ship"}},
		Skills: []SkillRef{{Name: "", Type: "custom"}},
	})
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "empty name") {
		t.Fatalf("expected empty-skill-name error: %v", res.Errors)
	}
}

func TestValidate_SkillShape_InvalidType(t *testing.T) {
	t.Parallel()
	cfg := managedConfigWithTeam(&Team{
		Name:   "ship",
		Lead:   Lead{Role: "Lead"},
		Tasks:  []Task{{Summary: "ship"}},
		Skills: []SkillRef{{Name: "ship-feature", Type: "bogus"}},
	})
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "type must be one of") {
		t.Fatalf("expected invalid-type error: %v", res.Errors)
	}
}

func TestValidate_CustomToolShape_EmptyName(t *testing.T) {
	t.Parallel()
	cfg := managedConfigWithTeam(&Team{
		Name:        "ship",
		Lead:        Lead{Role: "Lead"},
		Tasks:       []Task{{Summary: "ship"}},
		CustomTools: []CustomToolRef{{Name: ""}},
	})
	res := cfg.Validate()
	if res.Valid() || !errorsContain(res.Errors, "empty name") {
		t.Fatalf("expected empty-tool-name error: %v", res.Errors)
	}
}

func TestValidate_LocalBackendWarnsOnSkills(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Name:    "p",
		Backend: Backend{Kind: "local"},
		Teams: []Team{{
			Name:        "team",
			Lead:        Lead{Role: "Lead"},
			Tasks:       []Task{{Summary: "task"}},
			Skills:      []SkillRef{{Name: "ship-feature"}},
			CustomTools: []CustomToolRef{{Name: "signal_completion"}},
		}},
	}
	res := cfg.Validate()
	if !res.Valid() {
		t.Fatalf("local backend with skills should be valid (warnings only): %v", res.Errors)
	}
	if !warningsContain(res.Warnings, "skills are not supported under backend.kind=local") {
		t.Fatalf("expected skills-on-local warning: %v", res.Warnings)
	}
	if !warningsContain(res.Warnings, "custom_tools are not supported under backend.kind=local") {
		t.Fatalf("expected custom_tools-on-local warning: %v", res.Warnings)
	}
}

func TestValidateResourceReferences_MAUnknownSkillIsError(t *testing.T) {
	t.Parallel()
	cfg := managedConfigWithTeam(&Team{
		Name:   "ship",
		Lead:   Lead{Role: "Lead"},
		Tasks:  []Task{{Summary: "ship"}},
		Skills: []SkillRef{{Name: "ship-feature"}},
	})
	res := cfg.ValidateResourceReferences(map[string]bool{}, map[string]bool{})
	if res.Valid() {
		t.Fatal("expected error for unknown skill under MA")
	}
	if !errorsContain(res.Errors, "ship-feature") {
		t.Fatalf("error should name the skill: %v", res.Errors)
	}
}

func TestValidateResourceReferences_MAUnknownToolIsError(t *testing.T) {
	t.Parallel()
	cfg := managedConfigWithTeam(&Team{
		Name:        "ship",
		Lead:        Lead{Role: "Lead"},
		Tasks:       []Task{{Summary: "ship"}},
		CustomTools: []CustomToolRef{{Name: "signal_completion"}},
	})
	res := cfg.ValidateResourceReferences(map[string]bool{}, map[string]bool{})
	if res.Valid() {
		t.Fatal("expected error for unknown custom_tool under MA")
	}
	if !errorsContain(res.Errors, "signal_completion") {
		t.Fatalf("error should name the tool: %v", res.Errors)
	}
}

func TestValidateResourceReferences_LocalUnknownDowngradesToWarning(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Name:    "p",
		Backend: Backend{Kind: "local"},
		Teams: []Team{{
			Name:        "ship",
			Lead:        Lead{Role: "Lead"},
			Tasks:       []Task{{Summary: "ship"}},
			Skills:      []SkillRef{{Name: "ship-feature"}},
			CustomTools: []CustomToolRef{{Name: "signal_completion"}},
		}},
	}
	res := cfg.ValidateResourceReferences(map[string]bool{}, map[string]bool{})
	if !res.Valid() {
		t.Fatalf("local backend should downgrade unknown-name to warning: %v", res.Errors)
	}
	if !warningsContain(res.Warnings, "ship-feature") {
		t.Fatalf("expected warning for unknown skill: %v", res.Warnings)
	}
	if !warningsContain(res.Warnings, "signal_completion") {
		t.Fatalf("expected warning for unknown tool: %v", res.Warnings)
	}
}

func TestValidateResourceReferences_AllKnownIsClean(t *testing.T) {
	t.Parallel()
	cfg := managedConfigWithTeam(&Team{
		Name:        "ship",
		Lead:        Lead{Role: "Lead"},
		Tasks:       []Task{{Summary: "ship"}},
		Skills:      []SkillRef{{Name: "ship-feature"}},
		CustomTools: []CustomToolRef{{Name: "signal_completion"}},
	})
	res := cfg.ValidateResourceReferences(
		map[string]bool{"ship-feature": true},
		map[string]bool{"signal_completion": true},
	)
	if !res.Valid() {
		t.Fatalf("clean refs should pass: %v", res.Errors)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("clean refs should not warn: %v", res.Warnings)
	}
}

func managedConfigWithTeam(team *Team) *Config {
	return &Config{
		Name: "p",
		Backend: Backend{
			Kind: "managed_agents",
			ManagedAgents: &ManagedAgentsBackend{
				Repository: &RepositorySpec{URL: "https://github.com/x/y"},
			},
		},
		Defaults: Defaults{Model: "opus"},
		Teams:    []Team{*team},
	}
}
