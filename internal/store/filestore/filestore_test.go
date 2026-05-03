package filestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
	"github.com/itsHabib/orchestra/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T) store.Store {
		t.Helper()
		return filestore.New(
			filepath.Join(t.TempDir(), ".orchestra"),
			filestore.WithConfigDir(filepath.Join(t.TempDir(), "config")),
		)
	})
}

// TestLoadRunState_LegacyTeamsKeyMigrates pins the v2→v3 read-side
// migration: a state.json with only the legacy `teams:` key loads cleanly
// into RunState.Agents and the next save writes `agents:` going forward.
func TestLoadRunState_LegacyTeamsKeyMigrates(t *testing.T) {
	ctx := context.Background()
	workspaceDir := filepath.Join(t.TempDir(), ".orchestra")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"project":"legacy","run_id":"r1","teams":{"alpha":{"status":"running"}}}`)
	if err := os.WriteFile(filepath.Join(workspaceDir, "state.json"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}

	s := filestore.New(workspaceDir, filestore.WithConfigDir(filepath.Join(t.TempDir(), "config")))
	got, err := s.LoadRunState(ctx)
	if err != nil {
		t.Fatalf("LoadRunState: %v", err)
	}
	if len(got.Agents) != 1 || got.Agents["alpha"].Status != "running" {
		t.Fatalf("Agents = %+v, want one alpha=running", got.Agents)
	}

	// Saving back should write the canonical `agents:` key.
	if err := s.SaveRunState(ctx, got); err != nil {
		t.Fatalf("SaveRunState: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspaceDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), `"agents"`) {
		t.Fatalf("expected `\"agents\"` key in saved state.json, got: %s", data)
	}
	if contains(string(data), `"teams"`) {
		t.Fatalf("legacy `\"teams\"` key should not be re-emitted, got: %s", data)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestArchiveRunMovesActiveFiles(t *testing.T) {
	ctx := context.Background()
	workspaceDir := filepath.Join(t.TempDir(), ".orchestra")
	s := filestore.New(workspaceDir, filestore.WithConfigDir(filepath.Join(t.TempDir(), "config")))

	if err := s.SaveRunState(ctx, &store.RunState{
		Project: "archive-test",
		RunID:   "run-42",
		Agents:  map[string]store.AgentState{"alpha": {Status: "done"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceDir, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "results", "alpha.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.ArchiveRun(ctx, ""); err != nil {
		t.Fatalf("ArchiveRun: %v", err)
	}
	if _, err := s.LoadRunState(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LoadRunState after archive err=%v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "archive", "run-42", "state.json")); err != nil {
		t.Fatalf("archived state missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "archive", "run-42", "results", "alpha.json")); err != nil {
		t.Fatalf("archived result missing: %v", err)
	}
}
