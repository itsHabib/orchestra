package dag

import (
	"strings"
	"testing"

	"github.com/itsHabib/orchestra/internal/config"
)

func TestBuildTiers_LinearChain(t *testing.T) {
	teams := []config.Team{
		{Name: "a"},
		{Name: "b", DependsOn: []string{"a"}},
		{Name: "c", DependsOn: []string{"b"}},
	}
	tiers, err := BuildTiers(teams)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tiers) != 3 {
		t.Fatalf("expected 3 tiers, got %d: %v", len(tiers), tiers)
	}
	if tiers[0][0] != "a" || tiers[1][0] != "b" || tiers[2][0] != "c" {
		t.Fatalf("unexpected order: %v", tiers)
	}
}

func TestBuildTiers_Diamond(t *testing.T) {
	teams := []config.Team{
		{Name: "a"},
		{Name: "b", DependsOn: []string{"a"}},
		{Name: "c", DependsOn: []string{"a"}},
		{Name: "d", DependsOn: []string{"b", "c"}},
	}
	tiers, err := BuildTiers(teams)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tiers) != 3 {
		t.Fatalf("expected 3 tiers, got %d: %v", len(tiers), tiers)
	}
	if len(tiers[0]) != 1 || tiers[0][0] != "a" {
		t.Fatalf("tier 0 should be [a], got %v", tiers[0])
	}
	if len(tiers[1]) != 2 {
		t.Fatalf("tier 1 should have 2 teams, got %v", tiers[1])
	}
	if len(tiers[2]) != 1 || tiers[2][0] != "d" {
		t.Fatalf("tier 2 should be [d], got %v", tiers[2])
	}
}

func TestBuildTiers_AllParallel(t *testing.T) {
	teams := []config.Team{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}
	tiers, err := BuildTiers(teams)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tiers) != 1 {
		t.Fatalf("expected 1 tier, got %d: %v", len(tiers), tiers)
	}
	if len(tiers[0]) != 3 {
		t.Fatalf("expected 3 teams in tier, got %d", len(tiers[0]))
	}
}

func TestBuildTiers_Cycle(t *testing.T) {
	teams := []config.Team{
		{Name: "a", DependsOn: []string{"b"}},
		{Name: "b", DependsOn: []string{"a"}},
	}
	_, err := BuildTiers(teams)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got: %v", err)
	}
}

func TestBuildTiers_Empty(t *testing.T) {
	tiers, err := BuildTiers(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tiers != nil {
		t.Fatalf("expected nil, got %v", tiers)
	}
}

func TestBuildTiers_Single(t *testing.T) {
	teams := []config.Team{{Name: "solo"}}
	tiers, err := BuildTiers(teams)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tiers) != 1 || len(tiers[0]) != 1 || tiers[0][0] != "solo" {
		t.Fatalf("expected [[solo]], got %v", tiers)
	}
}
