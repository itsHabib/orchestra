package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// stubUploader records each Upload call and returns a fixed ID per
// invocation, indexed by call count. Tests pass a slice of IDs; running off
// the end is a hard fail (caught via t.Fatalf in the assertion path).
type stubUploader struct {
	mu    sync.Mutex
	calls []stubUpload
	ids   []string
	err   error
}

type stubUpload struct {
	contentBytes []byte
}

func (s *stubUploader) Upload(_ context.Context, params anthropic.BetaFileUploadParams, _ ...option.RequestOption) (*anthropic.FileMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	body, err := io.ReadAll(params.File)
	if err != nil {
		return nil, err
	}
	s.calls = append(s.calls, stubUpload{contentBytes: body})
	idx := len(s.calls) - 1
	if idx >= len(s.ids) {
		return nil, errors.New("stub: ran out of pre-seeded ids")
	}
	return &anthropic.FileMetadata{ID: s.ids[idx]}, nil
}

func (s *stubUploader) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func newTestService(t *testing.T, ids ...string) (*Service, *stubUploader, *Cache) {
	t.Helper()
	up := &stubUploader{ids: ids}
	cache := NewCache(filepath.Join(t.TempDir(), CacheFileName))
	svc := New(up, WithCache(cache))
	return svc, up, cache
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestServiceResolveUploadsAndCaches(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	svc, up, cache := newTestService(t, "file_abc", "file_def")
	pathA := writeFile(t, tmp, "a.md", "content A")
	pathB := writeFile(t, tmp, "b.json", `{"k":"v"}`)

	resolved, err := svc.Resolve(context.Background(), []FileResource{
		{Path: pathA, MountPath: "/workspace/spec.md"},
		{Path: pathB},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("len = %d, want 2", len(resolved))
	}
	if resolved[0].FileID != "file_abc" {
		t.Errorf("FileID[0] = %q, want file_abc", resolved[0].FileID)
	}
	if resolved[0].MountPath != "/workspace/spec.md" {
		t.Errorf("MountPath[0] = %q", resolved[0].MountPath)
	}
	if resolved[1].FileID != "file_def" {
		t.Errorf("FileID[1] = %q", resolved[1].FileID)
	}
	if resolved[1].MountPath != "/workspace/b.json" {
		t.Errorf("MountPath[1] default = %q, want /workspace/b.json", resolved[1].MountPath)
	}
	if up.callCount() != 2 {
		t.Errorf("upload called %d times, want 2", up.callCount())
	}

	// Re-resolving the same files should hit the cache and not re-upload.
	if _, err := svc.Resolve(context.Background(), []FileResource{
		{Path: pathA},
		{Path: pathB},
	}); err != nil {
		t.Fatalf("re-Resolve: %v", err)
	}
	if up.callCount() != 2 {
		t.Errorf("cache miss on second resolve: upload called %d times, want still 2", up.callCount())
	}

	// Cache file persists on disk.
	if _, err := os.Stat(cache.Path()); err != nil {
		t.Errorf("cache file should exist: %v", err)
	}
}

func TestServiceResolveDifferentContentReUploads(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	svc, up, _ := newTestService(t, "file_v1", "file_v2")
	path := writeFile(t, tmp, "a.md", "version 1")

	if _, err := svc.Resolve(context.Background(), []FileResource{{Path: path}}); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	// Same path, different content → fresh hash → cache miss → re-upload.
	if err := os.WriteFile(path, []byte("version 2"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	resolved, err := svc.Resolve(context.Background(), []FileResource{{Path: path}})
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if up.callCount() != 2 {
		t.Errorf("upload count = %d, want 2 (content changed)", up.callCount())
	}
	if resolved[0].FileID != "file_v2" {
		t.Errorf("FileID = %q, want file_v2", resolved[0].FileID)
	}
}

func TestServiceResolveErrors(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	t.Run("empty path", func(t *testing.T) {
		_, err := svc.Resolve(context.Background(), []FileResource{{Path: ""}})
		if err == nil || !contains(err.Error(), "path is required") {
			t.Fatalf("got %v, want path-required error", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := svc.Resolve(context.Background(), []FileResource{{Path: filepath.Join(t.TempDir(), "ghost")}})
		if err == nil {
			t.Fatalf("expected error for missing file")
		}
	})

	t.Run("upload failure surfaces", func(t *testing.T) {
		tmp := t.TempDir()
		path := writeFile(t, tmp, "a.md", "x")
		up := &stubUploader{err: errors.New("upstream 500")}
		bad := New(up, WithCache(NewCache(filepath.Join(t.TempDir(), CacheFileName))))
		_, err := bad.Resolve(context.Background(), []FileResource{{Path: path}})
		if err == nil || !contains(err.Error(), "upstream 500") {
			t.Fatalf("expected upload error, got %v", err)
		}
	})
}

func TestServiceResolveEmptyInput(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	out, err := svc.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve(nil): %v", err)
	}
	if out != nil {
		t.Errorf("nil input should return nil, got %v", out)
	}
}

func TestCachePutGetDelete(t *testing.T) {
	t.Parallel()
	cache := NewCache(filepath.Join(t.TempDir(), CacheFileName))

	if _, ok := cache.Get("h1"); ok {
		t.Fatalf("empty cache should miss")
	}
	if err := cache.Put("h1", "file_1"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := cache.Get("h1")
	if !ok || got != "file_1" {
		t.Fatalf("Get = %q,%v; want file_1,true", got, ok)
	}
	if err := cache.Delete("h1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := cache.Get("h1"); ok {
		t.Fatalf("Get after Delete should miss")
	}

	if err := cache.Put("", "file_x"); err == nil {
		t.Fatalf("Put with empty hash should error")
	}
	if err := cache.Put("h", ""); err == nil {
		t.Fatalf("Put with empty file id should error")
	}
}

// contains is strings.Contains without the import (the file doesn't
// otherwise need the package).
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
