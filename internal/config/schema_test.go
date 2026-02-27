package config

import (
	"strings"
	"testing"
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
	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
}

func TestValidate_EmptyName(t *testing.T) {
	cfg := &Config{
		Teams: []Team{{Name: "a", Tasks: []Task{{Summary: "x", Details: "d", Verify: "v"}}}},
	}
	_, err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty project name")
	}
	if !strings.Contains(err.Error(), "project name is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_EmptyTeams(t *testing.T) {
	cfg := &Config{Name: "p"}
	_, err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty teams")
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
	_, err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate team name") {
		t.Fatalf("expected duplicate team name error, got: %v", err)
	}
}

func TestValidate_SelfReference(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Teams: []Team{
			{Name: "a", Tasks: []Task{{Summary: "x"}}, DependsOn: []string{"a"}},
		},
	}
	_, err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "cannot depend on itself") {
		t.Fatalf("expected self-reference error, got: %v", err)
	}
}

func TestValidate_UnknownDependency(t *testing.T) {
	cfg := &Config{
		Name: "p",
		Teams: []Team{
			{Name: "a", Tasks: []Task{{Summary: "x"}}, DependsOn: []string{"b"}},
		},
	}
	_, err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown team") {
		t.Fatalf("expected unknown team error, got: %v", err)
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
	_, err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got: %v", err)
	}
}

func TestValidate_EmptyTaskSummary(t *testing.T) {
	cfg := &Config{
		Name:  "p",
		Teams: []Team{{Name: "a", Tasks: []Task{{Summary: ""}}}},
	}
	_, err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "empty summary") {
		t.Fatalf("expected empty summary error, got: %v", err)
	}
}

func TestValidate_NoTasks(t *testing.T) {
	cfg := &Config{
		Name:  "p",
		Teams: []Team{{Name: "a"}},
	}
	_, err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "at least one task") {
		t.Fatalf("expected no tasks error, got: %v", err)
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
	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, w := range warnings {
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
	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected task quality warnings for empty details/verify")
	}
}

func TestResolveDefaults(t *testing.T) {
	cfg := &Config{
		Teams: []Team{{Name: "a", Lead: Lead{Role: "dev"}}},
	}
	cfg.ResolveDefaults()
	if cfg.Defaults.Model != "sonnet" {
		t.Fatalf("expected sonnet, got %s", cfg.Defaults.Model)
	}
	if cfg.Defaults.MaxTurns != 200 {
		t.Fatalf("expected 200, got %d", cfg.Defaults.MaxTurns)
	}
	if cfg.Teams[0].Lead.Model != "sonnet" {
		t.Fatalf("expected team model sonnet, got %s", cfg.Teams[0].Lead.Model)
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
