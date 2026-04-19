package filestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsHabib/orchestra/pkg/store"
	"github.com/itsHabib/orchestra/pkg/store/filestore"
	"github.com/itsHabib/orchestra/pkg/store/storetest"
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

func TestArchiveRunMovesActiveFiles(t *testing.T) {
	ctx := context.Background()
	workspaceDir := filepath.Join(t.TempDir(), ".orchestra")
	s := filestore.New(workspaceDir, filestore.WithConfigDir(filepath.Join(t.TempDir(), "config")))

	if err := s.SaveRunState(ctx, &store.RunState{
		Project: "archive-test",
		RunID:   "run-42",
		Teams:   map[string]store.TeamState{"alpha": {Status: "done"}},
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
