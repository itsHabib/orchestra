package skills

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFileCacheRoundTrip(t *testing.T) {
	t.Parallel()
	cache := NewFileCache(filepath.Join(t.TempDir(), "skills.json"))
	ctx := context.Background()

	entry := Entry{
		FileID:      "file_01ABC",
		ContentHash: "sha256:deadbeef",
		SourcePath:  "/tmp/SKILL.md",
		Filename:    "SKILL.md",
		UploadedAt:  time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC),
	}
	if err := cache.Put(ctx, "ship-feature", &entry); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok, err := cache.Get(ctx, "ship-feature")
	switch {
	case err != nil:
		t.Fatalf("get: %v", err)
	case !ok:
		t.Fatalf("get: not found after put")
	case got != entry:
		t.Fatalf("get: round-trip mismatch\nwant %+v\ngot  %+v", entry, got)
	}
}

func TestFileCachePersistsAcrossInstances(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "skills.json")
	ctx := context.Background()
	entry := Entry{FileID: "f1", ContentHash: "h", SourcePath: "/x"}

	if err := NewFileCache(path).Put(ctx, "alpha", &entry); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := NewFileCache(path).Get(ctx, "alpha")
	switch {
	case err != nil:
		t.Fatalf("get: %v", err)
	case !ok:
		t.Fatalf("not found in fresh instance")
	case got.FileID != "f1":
		t.Fatalf("file_id: want f1 got %s", got.FileID)
	}
}

func TestFileCacheGetMissing(t *testing.T) {
	t.Parallel()
	cache := NewFileCache(filepath.Join(t.TempDir(), "skills.json"))
	_, ok, err := cache.Get(context.Background(), "nope")
	if err != nil {
		t.Fatalf("get on missing file should not error: %v", err)
	}
	if ok {
		t.Fatalf("get on missing entry should report ok=false")
	}
}

func TestFileCacheDelete(t *testing.T) {
	t.Parallel()
	cache := NewFileCache(filepath.Join(t.TempDir(), "skills.json"))
	ctx := context.Background()
	if err := cache.Put(ctx, "x", &Entry{FileID: "f"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := cache.Delete(ctx, "x"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, ok, err := cache.Get(ctx, "x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Fatalf("entry should be gone after delete")
	}
	// Deleting a missing entry is a no-op.
	if err := cache.Delete(ctx, "x"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestFileCacheList(t *testing.T) {
	t.Parallel()
	cache := NewFileCache(filepath.Join(t.TempDir(), "skills.json"))
	ctx := context.Background()
	if err := cache.Put(ctx, "a", &Entry{FileID: "fa"}); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if err := cache.Put(ctx, "b", &Entry{FileID: "fb"}); err != nil {
		t.Fatalf("put b: %v", err)
	}

	got, err := cache.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got["a"].FileID != "fa" || got["b"].FileID != "fb" {
		t.Fatalf("list: %+v", got)
	}
	names := SortedNames(got)
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("sorted names: %v", names)
	}
}

func TestFileCacheConcurrentPuts(t *testing.T) {
	t.Parallel()
	cache := NewFileCache(filepath.Join(t.TempDir(), "skills.json"))
	ctx := context.Background()

	var wg sync.WaitGroup
	const writers = 8
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := cache.Put(ctx, name(i), &Entry{FileID: name(i) + "-id"})
			if err != nil {
				t.Errorf("put %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, err := cache.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != writers {
		t.Fatalf("want %d entries, got %d", writers, len(got))
	}
}

func TestFileCacheEmptyName(t *testing.T) {
	t.Parallel()
	cache := NewFileCache(filepath.Join(t.TempDir(), "skills.json"))
	ctx := context.Background()
	if err := cache.Put(ctx, "", &Entry{}); err == nil {
		t.Fatalf("put: expected error on empty name")
	}
	if _, _, err := cache.Get(ctx, ""); err == nil {
		t.Fatalf("get: expected error on empty name")
	}
	if err := cache.Delete(ctx, ""); err == nil {
		t.Fatalf("delete: expected error on empty name")
	}
}

func name(i int) string {
	return string(rune('a' + i))
}
