package orchestra

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

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/files"
)

// stubUploader is the same shape internal/files uses in its tests but lives
// here to keep the cross-package wiring honest — orchestrationRun constructs
// a real [files.Service] with a fake Uploader, so the test exercises the
// production path end-to-end.
type stubUploader struct {
	mu    sync.Mutex
	calls int
	ids   []string
}

func (s *stubUploader) Upload(_ context.Context, params anthropic.BetaFileUploadParams, _ ...option.RequestOption) (*anthropic.FileMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := io.ReadAll(params.File); err != nil {
		return nil, err
	}
	idx := s.calls
	s.calls++
	if idx >= len(s.ids) {
		return nil, errors.New("stub: ran out of pre-seeded ids")
	}
	return &anthropic.FileMetadata{ID: s.ids[idx]}, nil
}

func TestBuildFileResources_HappyPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	pathA := writeTempFile(t, tmp, "spec.md", "# spec")
	pathB := writeTempFile(t, tmp, "data.csv", "a,b,c")

	up := &stubUploader{ids: []string{"file_spec", "file_data"}}
	cache := files.NewCache(filepath.Join(tmp, "cache.json"))
	svc := files.New(up, files.WithCache(cache))

	r := &orchestrationRun{fileService: svc}
	team := &Team{
		Name: "designer",
		Files: []config.FileMount{
			{Path: pathA, MountPath: "/workspace/spec.md"},
			{Path: pathB},
		},
	}

	got, err := r.buildFileResources(context.Background(), team)
	if err != nil {
		t.Fatalf("buildFileResources: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Type != "file" || got[0].FileID != "file_spec" || got[0].MountPath != "/workspace/spec.md" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Type != "file" || got[1].FileID != "file_data" || got[1].MountPath != "/workspace/data.csv" {
		t.Errorf("got[1] default mount = %+v", got[1])
	}
}

func TestBuildFileResources_NoFilesNoService(t *testing.T) {
	t.Parallel()
	r := &orchestrationRun{} // fileService nil — should not error when team has no files
	got, err := r.buildFileResources(context.Background(), &Team{Name: "alpha"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

func TestBuildFileResources_MissingService(t *testing.T) {
	t.Parallel()
	r := &orchestrationRun{} // declares files but no service wired
	team := &Team{
		Name:  "alpha",
		Files: []config.FileMount{{Path: "/tmp/x"}},
	}
	_, err := r.buildFileResources(context.Background(), team)
	if err == nil {
		t.Fatalf("expected error when files declared but service nil")
	}
}

func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}
