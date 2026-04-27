package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRegistry_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "mcp-runs.json"))
	ctx := context.Background()

	want := Entry{
		RunID:        "20260427T120000.000000000Z",
		WorkspaceDir: filepath.Join(dir, "runs", "alpha"),
		YAMLPath:     filepath.Join(dir, "runs", "alpha", "orchestra.yaml"),
		LogPath:      filepath.Join(dir, "runs", "alpha", "orchestra.log"),
		RepoURL:      "https://github.com/itsHabib/orchestra",
		DocPaths:     []string{"docs/foo.md", "docs/bar.md"},
		PID:          12345,
		StartedAt:    time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
	}
	if err := r.Put(ctx, &want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := r.Get(ctx, want.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("Get: ok=false for %s", want.RunID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}

	all, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("List: len=%d, want 1", len(all))
	}
}

func TestRegistry_ListEmpty(t *testing.T) {
	t.Parallel()

	r := NewRegistry(filepath.Join(t.TempDir(), "mcp-runs.json"))
	got, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List on missing file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List on missing file: want empty, got %v", got)
	}
}

func TestRegistry_Delete(t *testing.T) {
	t.Parallel()

	r := NewRegistry(filepath.Join(t.TempDir(), "mcp-runs.json"))
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if err := r.Put(ctx, &Entry{RunID: id, WorkspaceDir: "/tmp/" + id}); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	if err := r.Delete(ctx, "b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, ok, err := r.Get(ctx, "b")
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if ok {
		t.Fatalf("Get after delete: still present")
	}

	all, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List: len=%d, want 2", len(all))
	}
	// Listing should be sorted by RunID — a, c after b is removed.
	if all[0].RunID != "a" || all[1].RunID != "c" {
		t.Fatalf("List ordering: got %v", []string{all[0].RunID, all[1].RunID})
	}
}

func TestRegistry_Delete_MissingIsNoop(t *testing.T) {
	t.Parallel()

	r := NewRegistry(filepath.Join(t.TempDir(), "mcp-runs.json"))
	if err := r.Delete(context.Background(), "ghost"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestRegistry_Validations(t *testing.T) {
	t.Parallel()

	r := NewRegistry(filepath.Join(t.TempDir(), "mcp-runs.json"))
	ctx := context.Background()

	if err := r.Put(ctx, nil); err == nil {
		t.Fatalf("Put nil entry: want error, got nil")
	}
	if err := r.Put(ctx, &Entry{}); err == nil {
		t.Fatalf("Put empty run id: want error, got nil")
	}
	if _, _, err := r.Get(ctx, ""); err == nil {
		t.Fatalf("Get empty run id: want error, got nil")
	}
	if err := r.Delete(ctx, ""); err == nil {
		t.Fatalf("Delete empty run id: want error, got nil")
	}
}

func TestRegistry_AtomicWrite_NoTorn(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "mcp-runs.json")
	r := NewRegistry(path)
	ctx := context.Background()

	for i := 0; i < 16; i++ {
		id := makeRunID(i)
		if err := r.Put(ctx, &Entry{RunID: id, WorkspaceDir: "/tmp/" + id}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// After every write, the file must be parseable JSON. A torn
		// write would either fail to parse or contain a partial map.
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var f registryFile
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("file unparseable after Put #%d: %v\n%s", i, err, raw)
		}
	}
}

func TestNewRunID_Format(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 27, 12, 34, 56, 123456789, time.UTC)
	got := NewRunID(now)
	const want = "20260427T123456.123456789Z"
	if got != want {
		t.Fatalf("NewRunID:\n got=%q\nwant=%q", got, want)
	}
	// Lex-sort over consecutive ids must be chronological — required for
	// stable list_jobs ordering.
	earlier := NewRunID(now.Add(-time.Second))
	if earlier >= got {
		t.Fatalf("NewRunID not lex-sortable: %q !< %q", earlier, got)
	}
}

func TestUserDataDir_NonEmpty(t *testing.T) {
	t.Parallel()

	if got := userDataDir(); got == "" {
		t.Fatalf("userDataDir returned empty string")
	}
}

func TestDefaultRegistryPath_UnderUserDataDir(t *testing.T) {
	t.Parallel()

	got := DefaultRegistryPath()
	want := filepath.Join(userDataDir(), "orchestra", "mcp-runs.json")
	if got != want {
		t.Fatalf("DefaultRegistryPath:\n got=%q\nwant=%q", got, want)
	}
}

func TestPrepareRun_WritesYAMLAndPopulates(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "mcp-runs")
	cfg := map[string]any{
		"name": "demo",
		"backend": map[string]any{
			"kind": "managed_agents",
		},
	}
	docs := []string{"docs/a.md", "docs/b.md"}

	entry, err := PrepareRun(root, "20260427T120000.000000000Z", cfg, "https://github.com/x/y", docs)
	if err != nil {
		t.Fatalf("PrepareRun: %v", err)
	}
	if entry.RunID != "20260427T120000.000000000Z" {
		t.Fatalf("RunID: got %q", entry.RunID)
	}
	if entry.WorkspaceDir == "" || !strings.HasPrefix(entry.WorkspaceDir, root) {
		t.Fatalf("WorkspaceDir not under root: %q", entry.WorkspaceDir)
	}
	if !reflect.DeepEqual(entry.DocPaths, docs) {
		t.Fatalf("DocPaths copied wrong: got %v", entry.DocPaths)
	}

	yamlBytes, err := os.ReadFile(entry.YAMLPath)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	if !strings.Contains(string(yamlBytes), "name: demo") {
		t.Fatalf("yaml content missing fields:\n%s", yamlBytes)
	}
}

func TestPrepareRun_ValidatesArgs(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "mcp-runs")
	cfg := map[string]any{"name": "demo"}

	cases := []struct {
		name      string
		root      string
		runID     string
		cfg       any
		wantSubst string
	}{
		{"empty root", "", "id", cfg, "workspace root"},
		{"empty run id", root, "", cfg, "run id"},
		{"nil config", root, "id", nil, "config"},
	}
	for _, tc := range cases {
		_, err := PrepareRun(tc.root, tc.runID, tc.cfg, "url", nil)
		if err == nil {
			t.Fatalf("%s: want error, got nil", tc.name)
		}
		if !strings.Contains(err.Error(), tc.wantSubst) {
			t.Fatalf("%s: error %q does not contain %q", tc.name, err, tc.wantSubst)
		}
	}
}

func makeRunID(seq int) string {
	return NewRunID(time.Date(2026, 4, 27, 12, 0, seq, 0, time.UTC))
}
