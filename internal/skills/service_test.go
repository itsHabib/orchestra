package skills

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// fakeUploadClient records uploads/deletes and returns synthetic file_ids so
// the service can be exercised without a real Anthropic API. The mutex guards
// the recording slices so concurrent service calls (future tests) don't race;
// today's tests are sequential, but the cost of the lock is negligible.
type fakeUploadClient struct {
	mu          sync.Mutex
	seq         atomic.Int64
	uploads     []recordedUpload
	deletes     []string
	uploadErr   error
	deleteErr   error
	clobberName string // if set, returned FileMetadata.Filename uses this
}

type recordedUpload struct {
	FileID   string
	Filename string
	Body     []byte
}

func (f *fakeUploadClient) Upload(_ context.Context, params anthropic.BetaFileUploadParams, _ ...option.RequestOption) (*anthropic.FileMetadata, error) {
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	body, err := io.ReadAll(params.File)
	if err != nil {
		return nil, err
	}
	filename := "anonymous_file"
	if named, ok := params.File.(interface{ Filename() string }); ok {
		filename = named.Filename()
	}
	if f.clobberName != "" {
		filename = f.clobberName
	}
	id := "file_" + nextID(&f.seq)
	f.mu.Lock()
	f.uploads = append(f.uploads, recordedUpload{FileID: id, Filename: filename, Body: body})
	f.mu.Unlock()
	return &anthropic.FileMetadata{ID: id, Filename: filename, SizeBytes: int64(len(body))}, nil
}

func (f *fakeUploadClient) Delete(_ context.Context, fileID string, _ anthropic.BetaFileDeleteParams, _ ...option.RequestOption) (*anthropic.DeletedFile, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.mu.Lock()
	f.deletes = append(f.deletes, fileID)
	f.mu.Unlock()
	return &anthropic.DeletedFile{ID: fileID}, nil
}

func nextID(seq *atomic.Int64) string {
	n := seq.Add(1)
	out := []byte{}
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	if len(out) == 0 {
		out = []byte{'0'}
	}
	return string(out)
}

func newServiceFixture(t *testing.T) (*Service, *fakeUploadClient, *FileCache, string) {
	t.Helper()
	dir := t.TempDir()
	cache := NewFileCache(filepath.Join(dir, "skills.json"))
	client := &fakeUploadClient{}
	clock := func() time.Time { return time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC) }
	svc := New(cache, client, WithClock(clock))
	return svc, client, cache, dir
}

func writeSkill(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestServiceUploadFirstTime(t *testing.T) {
	t.Parallel()
	svc, client, cache, dir := newServiceFixture(t)
	src := writeSkill(t, dir, "ship-feature/SKILL.md", "# ship-feature\nbody\n")

	res, err := svc.Upload(context.Background(), "ship-feature", src)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Action != SyncReuploaded {
		t.Fatalf("action: want %s got %s", SyncReuploaded, res.Action)
	}
	if res.PreviousFile != "" {
		t.Fatalf("first upload should have no PreviousFile, got %s", res.PreviousFile)
	}
	if len(client.uploads) != 1 {
		t.Fatalf("upload count: want 1 got %d", len(client.uploads))
	}
	if client.uploads[0].Filename != "SKILL.md" {
		t.Fatalf("filename: want SKILL.md got %s", client.uploads[0].Filename)
	}
	stored, ok, err := cache.Get(context.Background(), "ship-feature")
	switch {
	case err != nil:
		t.Fatalf("cache get: %v", err)
	case !ok:
		t.Fatalf("cache empty after upload")
	case stored.FileID != res.Entry.FileID:
		t.Fatalf("cache file_id mismatch: want %s got %s", res.Entry.FileID, stored.FileID)
	case stored.SourcePath != src:
		t.Fatalf("cache source: want %s got %s", src, stored.SourcePath)
	case stored.ContentHash != ContentHash([]byte("# ship-feature\nbody\n")):
		t.Fatalf("cache hash mismatch")
	}
}

func TestServiceUploadIdempotentOnNoChange(t *testing.T) {
	t.Parallel()
	svc, client, _, dir := newServiceFixture(t)
	src := writeSkill(t, dir, "ship-feature/SKILL.md", "# body\n")

	first, err := svc.Upload(context.Background(), "ship-feature", src)
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	second, err := svc.Upload(context.Background(), "ship-feature", src)
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if second.Action != SyncUpToDate {
		t.Fatalf("second action: want %s got %s", SyncUpToDate, second.Action)
	}
	if second.Entry.FileID != first.Entry.FileID {
		t.Fatalf("file_id changed on identical content: %s -> %s", first.Entry.FileID, second.Entry.FileID)
	}
	if len(client.uploads) != 1 {
		t.Fatalf("expected 1 upload total, got %d", len(client.uploads))
	}
}

func TestServiceUploadReuploadsOnDrift(t *testing.T) {
	t.Parallel()
	svc, client, cache, dir := newServiceFixture(t)
	src := writeSkill(t, dir, "ship-feature/SKILL.md", "v1\n")

	first, err := svc.Upload(context.Background(), "ship-feature", src)
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if err := os.WriteFile(src, []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	second, err := svc.Upload(context.Background(), "ship-feature", src)
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if second.Action != SyncReuploaded {
		t.Fatalf("action: want %s got %s", SyncReuploaded, second.Action)
	}
	if second.PreviousFile != first.Entry.FileID {
		t.Fatalf("previous_file: want %s got %s", first.Entry.FileID, second.PreviousFile)
	}
	if second.Entry.FileID == first.Entry.FileID {
		t.Fatalf("file_id should change on drift")
	}
	if len(client.uploads) != 2 {
		t.Fatalf("expected 2 uploads, got %d", len(client.uploads))
	}
	// Cache reflects the latest entry.
	stored, _, err := cache.Get(context.Background(), "ship-feature")
	if err != nil {
		t.Fatalf("cache get: %v", err)
	}
	if stored.FileID != second.Entry.FileID {
		t.Fatalf("cache stale: want %s got %s", second.Entry.FileID, stored.FileID)
	}
}

func TestServiceUploadMissingSource(t *testing.T) {
	t.Parallel()
	svc, _, _, dir := newServiceFixture(t)
	missing := filepath.Join(dir, "does-not-exist.md")
	res, err := svc.Upload(context.Background(), "ship-feature", missing)
	if err == nil {
		t.Fatalf("expected error on missing source")
	}
	if !res.SourceMissing {
		t.Fatalf("SourceMissing not set on missing file: %+v", res)
	}
}

func TestServiceUploadAPIError(t *testing.T) {
	t.Parallel()
	svc, client, cache, dir := newServiceFixture(t)
	src := writeSkill(t, dir, "x/SKILL.md", "body\n")
	client.uploadErr = errors.New("boom")

	if _, err := svc.Upload(context.Background(), "x", src); err == nil {
		t.Fatalf("expected upload error to propagate")
	}
	if _, ok, _ := cache.Get(context.Background(), "x"); ok {
		t.Fatalf("cache should not contain entry on failed upload")
	}
}

func TestServiceLookupDriftAndMissing(t *testing.T) {
	t.Parallel()
	svc, _, cache, dir := newServiceFixture(t)
	src := writeSkill(t, dir, "x/SKILL.md", "v1\n")
	if _, err := svc.Upload(context.Background(), "x", src); err != nil {
		t.Fatalf("upload: %v", err)
	}

	// Identical content => not drifted.
	look, err := svc.Lookup(context.Background(), "x")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if look.Drifted {
		t.Fatalf("unexpected drift on identical content")
	}

	// Modify source => drifted.
	if err := os.WriteFile(src, []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	look, err = svc.Lookup(context.Background(), "x")
	if err != nil {
		t.Fatalf("lookup post-drift: %v", err)
	}
	if !look.Drifted {
		t.Fatalf("expected drift after source mutation")
	}

	// Remove source => SourceMissing, no drift signal.
	if err := os.Remove(src); err != nil {
		t.Fatalf("remove: %v", err)
	}
	look, err = svc.Lookup(context.Background(), "x")
	if err != nil {
		t.Fatalf("lookup with missing source: %v", err)
	}
	if !look.SourceMissing {
		t.Fatalf("expected SourceMissing when source absent")
	}
	if look.Drifted {
		t.Fatalf("Drifted should not fire when source is missing")
	}

	// Lookup of unknown name => Found=false.
	look, err = svc.Lookup(context.Background(), "no-such-skill")
	if err != nil {
		t.Fatalf("lookup unknown: %v", err)
	}
	if look.Found {
		t.Fatalf("Found should be false for unknown name")
	}

	_ = cache
}

func TestServiceSync(t *testing.T) {
	t.Parallel()
	svc, client, _, dir := newServiceFixture(t)

	srcA := writeSkill(t, dir, "a/SKILL.md", "alpha v1\n")
	srcB := writeSkill(t, dir, "b/SKILL.md", "beta v1\n")
	srcC := writeSkill(t, dir, "c/SKILL.md", "gamma v1\n")
	for _, pair := range []struct{ name, src string }{
		{"a", srcA}, {"b", srcB}, {"c", srcC},
	} {
		if _, err := svc.Upload(context.Background(), pair.name, pair.src); err != nil {
			t.Fatalf("upload %s: %v", pair.name, err)
		}
	}
	uploadsAfterFirst := len(client.uploads)

	// Drift one source, remove another, leave the third intact.
	if err := os.WriteFile(srcA, []byte("alpha v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite a: %v", err)
	}
	if err := os.Remove(srcB); err != nil {
		t.Fatalf("remove b: %v", err)
	}

	results, err := svc.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	got := map[string]SyncAction{}
	for _, r := range results {
		got[r.Name] = r.Action
	}
	if got["a"] != SyncReuploaded {
		t.Fatalf("a: want %s got %s", SyncReuploaded, got["a"])
	}
	if got["b"] != SyncSkipped {
		t.Fatalf("b: want %s got %s", SyncSkipped, got["b"])
	}
	if got["c"] != SyncUpToDate {
		t.Fatalf("c: want %s got %s", SyncUpToDate, got["c"])
	}
	if len(client.uploads) != uploadsAfterFirst+1 {
		t.Fatalf("expected exactly one re-upload, got total %d (was %d)",
			len(client.uploads), uploadsAfterFirst)
	}
}

func TestServiceSyncUploadFailureSurfacesError(t *testing.T) {
	t.Parallel()
	svc, client, _, dir := newServiceFixture(t)
	src := writeSkill(t, dir, "x/SKILL.md", "v1\n")
	if _, err := svc.Upload(context.Background(), "x", src); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	// Drift the source so Sync attempts a re-upload, then make the API fail.
	if err := os.WriteFile(src, []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	client.uploadErr = errors.New("transient API failure")

	results, err := svc.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Action != SyncSkipped {
		t.Fatalf("action: want %s got %q (zero value would silently swallow the error)",
			SyncSkipped, r.Action)
	}
	if r.Err == nil {
		t.Fatalf("Err should be set on upload failure")
	}
}

func TestServiceSyncEmpty(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newServiceFixture(t)
	results, err := svc.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync empty: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results from empty cache, got %d", len(results))
	}
}

func TestServiceSortedLookups(t *testing.T) {
	t.Parallel()
	svc, _, _, dir := newServiceFixture(t)
	for _, n := range []string{"zeta", "alpha", "mu"} {
		src := writeSkill(t, dir, n+"/SKILL.md", n+" body\n")
		if _, err := svc.Upload(context.Background(), n, src); err != nil {
			t.Fatalf("upload %s: %v", n, err)
		}
	}
	looks, err := svc.SortedLookups(context.Background())
	if err != nil {
		t.Fatalf("sorted lookups: %v", err)
	}
	if len(looks) != 3 {
		t.Fatalf("want 3 lookups got %d", len(looks))
	}
	want := []string{"alpha", "mu", "zeta"}
	for i, w := range want {
		if looks[i].Name != w {
			t.Fatalf("position %d: want %s got %s", i, w, looks[i].Name)
		}
	}
}

func TestResolveSourceDefault(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("home dir unavailable: %v", err)
	}
	got, err := ResolveSource("ship-feature", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := filepath.Join(home, DefaultSourceDir, "ship-feature", "SKILL.md")
	if got != want {
		t.Fatalf("default path: want %s got %s", want, got)
	}
}

func TestResolveSourceOverride(t *testing.T) {
	t.Parallel()
	got, err := ResolveSource("ship-feature", "./relative/SKILL.md")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("override should resolve to absolute, got %s", got)
	}
}

func TestResolveSourceEmptyName(t *testing.T) {
	t.Parallel()
	if _, err := ResolveSource("", ""); err == nil {
		t.Fatalf("expected error when both name and override are empty")
	}
}
