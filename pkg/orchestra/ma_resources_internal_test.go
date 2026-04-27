package orchestra

import (
	"testing"

	"github.com/itsHabib/orchestra/internal/customtools"
	"github.com/itsHabib/orchestra/internal/skills"
	"github.com/itsHabib/orchestra/internal/spawner"
)

func TestResolveSkillsForTeam_HappyPath(t *testing.T) {
	t.Parallel()
	team := &Team{
		Name: "ship",
		Skills: []SkillRef{
			{Name: "ship-feature"},
		},
	}
	entries := map[string]skills.Entry{
		"ship-feature": {SkillID: "skill_01", LatestVersion: "v1"},
	}
	got, err := resolveSkillsForTeam(team, entries)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d skills want 1", len(got))
	}
	if got[0].Name != "skill_01" {
		t.Fatalf("Skill.Name should be skill_id from cache: %s", got[0].Name)
	}
	if got[0].Version != "v1" {
		t.Fatalf("Skill.Version: want v1 got %s", got[0].Version)
	}
	if got[0].Metadata["type"] != "custom" {
		t.Fatalf("Skill.Metadata[type]: want custom got %s", got[0].Metadata["type"])
	}
}

func TestResolveSkillsForTeam_VersionOverride(t *testing.T) {
	t.Parallel()
	team := &Team{
		Name: "ship",
		Skills: []SkillRef{
			{Name: "ship-feature", Version: "vpinned"},
		},
	}
	entries := map[string]skills.Entry{
		"ship-feature": {SkillID: "skill_01", LatestVersion: "vlatest"},
	}
	got, _ := resolveSkillsForTeam(team, entries)
	if got[0].Version != "vpinned" {
		t.Fatalf("explicit Version should override LatestVersion: got %s", got[0].Version)
	}
}

func TestResolveSkillsForTeam_TypeOverride(t *testing.T) {
	t.Parallel()
	team := &Team{
		Name: "ship",
		Skills: []SkillRef{
			{Name: "anth-skill", Type: "anthropic"},
		},
	}
	entries := map[string]skills.Entry{
		"anth-skill": {SkillID: "skill_a"},
	}
	got, _ := resolveSkillsForTeam(team, entries)
	if got[0].Metadata["type"] != "anthropic" {
		t.Fatalf("explicit Type should propagate to Metadata: %s", got[0].Metadata["type"])
	}
}

func TestResolveSkillsForTeam_UnknownReturnsError(t *testing.T) {
	t.Parallel()
	team := &Team{
		Name: "ship",
		Skills: []SkillRef{
			{Name: "missing"},
		},
	}
	if _, err := resolveSkillsForTeam(team, map[string]skills.Entry{}); err == nil {
		t.Fatal("expected error for unregistered skill")
	}
}

func TestResolveCustomToolsForTeam_HappyPath(t *testing.T) {
	t.Parallel()
	customtools.Reset()
	t.Cleanup(customtools.Reset)
	customtools.MustRegister(customtools.NewSignalCompletion())

	team := &Team{
		Name:        "ship",
		CustomTools: []CustomToolRef{{Name: "signal_completion"}},
	}
	got, err := resolveCustomToolsForTeam(team)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tools want 1", len(got))
	}
	if got[0].Name != "signal_completion" {
		t.Fatalf("Tool.Name: %s", got[0].Name)
	}
	if got[0].Type != "custom" {
		t.Fatalf("Tool.Type: want custom got %s", got[0].Type)
	}
	if got[0].Description == "" {
		t.Fatal("Tool.Description should be populated from handler.Tool().Description")
	}
	if got[0].InputSchema == nil {
		t.Fatal("Tool.InputSchema should propagate from handler")
	}
}

func TestResolveCustomToolsForTeam_UnknownReturnsError(t *testing.T) {
	t.Parallel()
	customtools.Reset()
	t.Cleanup(customtools.Reset)

	team := &Team{
		Name:        "ship",
		CustomTools: []CustomToolRef{{Name: "no-such-tool"}},
	}
	if _, err := resolveCustomToolsForTeam(team); err == nil {
		t.Fatal("expected error for unregistered custom tool")
	}
}

func TestAgentsSkillCopyDecouplesMetadata(t *testing.T) {
	t.Parallel()
	src := []spawner.Skill{{Name: "s", Metadata: map[string]string{"type": "custom"}}}
	dst := agentsSkillCopy(src)
	src[0].Metadata["type"] = "mutated"
	if dst[0].Metadata["type"] != "custom" {
		t.Fatal("copy should not share Metadata map with source")
	}
}

func TestManagedAgentSpecAttachesResolvedSkillsAndTools(t *testing.T) {
	t.Parallel()
	team := &Team{
		Name: "ship",
		Lead: Lead{Role: "Implementer"},
	}
	r := &orchestrationRun{
		cfg: &Config{
			Name:     "p",
			Defaults: Defaults{Model: "opus"},
		},
		teamSkills: map[string][]spawner.Skill{
			"ship": {{Name: "skill_01", Version: "v1", Metadata: map[string]string{"type": "custom"}}},
		},
		teamCustomTools: map[string][]spawner.Tool{
			"ship": {{Name: "signal_completion", Type: "custom", Description: "desc", InputSchema: map[string]any{"type": "object"}}},
		},
	}

	spec := r.managedAgentSpec(team)
	if len(spec.Skills) != 1 || spec.Skills[0].Name != "skill_01" {
		t.Fatalf("skills not propagated: %+v", spec.Skills)
	}
	// Built-in tools (bash/read/write/edit/grep/glob) plus the resolved
	// custom tool should both appear.
	var foundCustom bool
	for i := range spec.Tools {
		if spec.Tools[i].Name == "signal_completion" && spec.Tools[i].Type == "custom" {
			foundCustom = true
			break
		}
	}
	if !foundCustom {
		t.Fatalf("custom tool not appended to spec.Tools: %+v", spec.Tools)
	}
}
