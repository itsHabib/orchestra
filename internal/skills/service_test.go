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

// fakeRegistrationClient records the multipart calls and returns synthetic
// skill_ids/version ids so the service can be exercised without a real
// Anthropic API. The mutex guards the recording slices so concurrent service
// calls don't race; today's tests are sequential, but the cost of the lock
// is negligible.
type fakeRegistrationClient struct {
	mu          sync.Mutex
	skillSeq    atomic.Int64
	versionSeq  atomic.Int64
	registers   []recordedUpload
	versions    []recordedVersionUpload
	registerErr error
	versionErr  error
}

type recordedUpload struct {
	SkillID      string
	DisplayTitle string
	Filenames    []string
	Bodies       map[string][]byte
}

type recordedVersionUpload struct {
	SkillID   string
	Version   string
	Filenames []string
	Bodies    map[string][]byte
}

// The SDK-imposed signature takes BetaSkillNewParams by value, which trips
// gocritic's hugeParam lint. The signature must match RegistrationClient,
// so the lint is silenced here.
//
//nolint:gocritic // SDK-imposed signature.
func (f *fakeRegistrationClient) NewSkill(_ context.Context, params anthropic.BetaSkillNewParams, _ ...option.RequestOption) (*anthropic.BetaSkillNewResponse, error) {
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	rec := recordedUpload{
		DisplayTitle: params.DisplayTitle.Or(""),
		Bodies:       make(map[string][]byte),
	}
	for _, r := range params.Files {
		filename := "anonymous_file"
		if named, ok := r.(interface{ Filename() string }); ok {
			filename = named.Filename()
		}
		body, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		rec.Filenames = append(rec.Filenames, filename)
		rec.Bodies[filename] = body
	}
	id := "skill_" + nextID(&f.skillSeq)
	rec.SkillID = id
	f.mu.Lock()
	f.registers = append(f.registers, rec)
	f.mu.Unlock()
	version := "v" + nextID(&f.versionSeq)
	return &anthropic.BetaSkillNewResponse{
		ID:            id,
		LatestVersion: version,
		DisplayTitle:  rec.DisplayTitle,
	}, nil
}

func (f *fakeRegistrationClient) NewSkillVersion(_ context.Context, skillID string, params anthropic.BetaSkillVersionNewParams, _ ...option.RequestOption) (*anthropic.BetaSkillVersionNewResponse, error) {
	if f.versionErr != nil {
		return nil, f.versionErr
	}
	rec := recordedVersionUpload{
		SkillID: skillID,
		Bodies:  make(map[string][]byte),
	}
	for _, r := range params.Files {
		filename := "anonymous_file"
		if named, ok := r.(interface{ Filename() string }); ok {
			filename = named.Filename()
		}
		body, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		rec.Filenames = append(rec.Filenames, filename)
		rec.Bodies[filename] = body
	}
	version := "v" + nextID(&f.versionSeq)
	rec.Version = version
	f.mu.Lock()
	f.versions = append(f.versions, rec)
	f.mu.Unlock()
	return &anthropic.BetaSkillVersionNewResponse{
		ID:      "skill_version_" + version,
		SkillID: skillID,
		Version: version,
	}, nil
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

func newServiceFixture(t *testing.T) (*Service, *fakeRegistrationClient, *FileCache, string) {
	t.Helper()
	dir := t.TempDir()
	cache := NewFileCache(filepath.Join(dir, "skills.json"))
	client := &fakeRegistrationClient{}
	clock := func() time.Time { return time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC) }
	svc := New(cache, client, WithClock(clock))
	return svc, client, cache, dir
}

func writeSkillSource(t *testing.T, parent, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(parent, name)
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

func TestServiceUploadFirstTime(t *testing.T) {
	t.Parallel()
	svc, client, cache, dir := newServiceFixture(t)
	src := writeSkillSource(t, dir, "ship-feature", map[string]string{
		"SKILL.md":       "# ship-feature\nbody\n",
		"helpers/foo.sh": "#!/bin/sh\necho hi\n",
	})

	res, err := svc.Upload(context.Background(), "ship-feature", src)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	assertFirstUploadResult(t, &res)
	assertRegisteredFilenames(t, client, map[string]bool{
		"ship-feature/SKILL.md":       true,
		"ship-feature/helpers/foo.sh": true,
	})
	assertCacheMatchesUpload(t, cache, &res, src)
}

func assertFirstUploadResult(t *testing.T, res *SyncResult) {
	t.Helper()
	if res.Action != SyncReuploaded {
		t.Fatalf("action: want %s got %s", SyncReuploaded, res.Action)
	}
	if res.PreviousVersion != "" {
		t.Fatalf("first upload should have no PreviousVersion, got %s", res.PreviousVersion)
	}
}

func assertRegisteredFilenames(t *testing.T, client *fakeRegistrationClient, want map[string]bool) {
	t.Helper()
	if len(client.registers) != 1 {
		t.Fatalf("register count: want 1 got %d", len(client.registers))
	}
	rec := client.registers[0]
	if rec.DisplayTitle != "ship-feature" {
		t.Fatalf("display_title: want ship-feature got %s", rec.DisplayTitle)
	}
	if len(rec.Filenames) != len(want) {
		t.Fatalf("filename count: want %d got %d (%v)", len(want), len(rec.Filenames), rec.Filenames)
	}
	for _, fn := range rec.Filenames {
		if !want[fn] {
			t.Fatalf("unexpected upload filename %q", fn)
		}
	}
}

func assertCacheMatchesUpload(t *testing.T, cache *FileCache, res *SyncResult, src string) {
	t.Helper()
	stored, ok, err := cache.Get(context.Background(), "ship-feature")
	if err != nil {
		t.Fatalf("cache get: %v", err)
	}
	if !ok {
		t.Fatal("cache empty after upload")
	}
	if stored.SkillID != res.Entry.SkillID {
		t.Fatalf("cache skill_id mismatch: want %s got %s", res.Entry.SkillID, stored.SkillID)
	}
	if stored.LatestVersion == "" {
		t.Fatal("cache version not set")
	}
	if stored.SourcePath != src {
		t.Fatalf("cache source: want %s got %s", src, stored.SourcePath)
	}
	hash, err := DirHash(src)
	if err != nil {
		t.Fatalf("dir hash: %v", err)
	}
	if stored.ContentHash != hash {
		t.Fatalf("cache hash mismatch: want %s got %s", hash, stored.ContentHash)
	}
}

func TestServiceUploadIdempotentOnNoChange(t *testing.T) {
	t.Parallel()
	svc, client, _, dir := newServiceFixture(t)
	src := writeSkillSource(t, dir, "ship-feature", map[string]string{
		"SKILL.md": "# body\n",
	})

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
	if second.Entry.SkillID != first.Entry.SkillID {
		t.Fatalf("skill_id changed on identical content: %s -> %s", first.Entry.SkillID, second.Entry.SkillID)
	}
	if len(client.registers) != 1 {
		t.Fatalf("expected 1 register total, got %d", len(client.registers))
	}
	if len(client.versions) != 0 {
		t.Fatalf("expected 0 versions on no-change, got %d", len(client.versions))
	}
}

func TestServiceUploadVersionsOnDrift(t *testing.T) {
	t.Parallel()
	svc, client, cache, dir := newServiceFixture(t)
	src := writeSkillSource(t, dir, "ship-feature", map[string]string{
		"SKILL.md": "v1\n",
	})

	first, err := svc.Upload(context.Background(), "ship-feature", src)
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	second, err := svc.Upload(context.Background(), "ship-feature", src)
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if second.Action != SyncReuploaded {
		t.Fatalf("action: want %s got %s", SyncReuploaded, second.Action)
	}
	if second.PreviousVersion != first.Entry.LatestVersion {
		t.Fatalf("previous_version: want %s got %s", first.Entry.LatestVersion, second.PreviousVersion)
	}
	if second.Entry.SkillID != first.Entry.SkillID {
		t.Fatalf("skill_id changed across versions: %s -> %s", first.Entry.SkillID, second.Entry.SkillID)
	}
	if second.Entry.LatestVersion == first.Entry.LatestVersion {
		t.Fatalf("version should change on drift")
	}
	if len(client.registers) != 1 {
		t.Fatalf("expected 1 register, got %d", len(client.registers))
	}
	if len(client.versions) != 1 {
		t.Fatalf("expected 1 version upload, got %d", len(client.versions))
	}
	stored, _, err := cache.Get(context.Background(), "ship-feature")
	if err != nil {
		t.Fatalf("cache get: %v", err)
	}
	if stored.LatestVersion != second.Entry.LatestVersion {
		t.Fatalf("cache stale: want %s got %s", second.Entry.LatestVersion, stored.LatestVersion)
	}
}

func TestServiceUploadMissingSource(t *testing.T) {
	t.Parallel()
	svc, _, _, dir := newServiceFixture(t)
	missing := filepath.Join(dir, "does-not-exist")
	res, err := svc.Upload(context.Background(), "ship-feature", missing)
	if err == nil {
		t.Fatalf("expected error on missing source")
	}
	if !res.SourceMissing {
		t.Fatalf("SourceMissing not set on missing dir: %+v", res)
	}
}

func TestServiceUploadAPIError(t *testing.T) {
	t.Parallel()
	svc, client, cache, dir := newServiceFixture(t)
	src := writeSkillSource(t, dir, "x", map[string]string{
		"SKILL.md": "body\n",
	})
	client.registerErr = errors.New("boom")

	if _, err := svc.Upload(context.Background(), "x", src); err == nil {
		t.Fatalf("expected register error to propagate")
	}
	if _, ok, _ := cache.Get(context.Background(), "x"); ok {
		t.Fatalf("cache should not contain entry on failed register")
	}
}

func TestServiceLookupDriftAndMissing(t *testing.T) {
	t.Parallel()
	svc, _, _, dir := newServiceFixture(t)
	src := writeSkillSource(t, dir, "x", map[string]string{
		"SKILL.md": "v1\n",
	})
	if _, err := svc.Upload(context.Background(), "x", src); err != nil {
		t.Fatalf("upload: %v", err)
	}

	look, err := svc.Lookup(context.Background(), "x")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if look.Drifted {
		t.Fatalf("unexpected drift on identical content")
	}

	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	look, err = svc.Lookup(context.Background(), "x")
	if err != nil {
		t.Fatalf("lookup post-drift: %v", err)
	}
	if !look.Drifted {
		t.Fatalf("expected drift after source mutation")
	}

	if err := os.RemoveAll(src); err != nil {
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

	look, err = svc.Lookup(context.Background(), "no-such-skill")
	if err != nil {
		t.Fatalf("lookup unknown: %v", err)
	}
	if look.Found {
		t.Fatalf("Found should be false for unknown name")
	}
}

func TestServiceLookupTreatsLegacyEntryAsMissing(t *testing.T) {
	t.Parallel()
	svc, _, cache, _ := newServiceFixture(t)
	if err := cache.Put(context.Background(), "legacy", &Entry{
		ContentHash: "sha256:old",
		SourcePath:  "/wherever",
	}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	look, err := svc.Lookup(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if look.Found {
		t.Fatalf("legacy entry without skill_id should report Found=false (so the caller re-registers)")
	}
}

func TestServiceSync(t *testing.T) {
	t.Parallel()
	svc, client, _, dir := newServiceFixture(t)

	srcA := writeSkillSource(t, dir, "a", map[string]string{"SKILL.md": "alpha v1\n"})
	srcB := writeSkillSource(t, dir, "b", map[string]string{"SKILL.md": "beta v1\n"})
	srcC := writeSkillSource(t, dir, "c", map[string]string{"SKILL.md": "gamma v1\n"})
	for _, pair := range []struct{ name, src string }{
		{"a", srcA}, {"b", srcB}, {"c", srcC},
	} {
		if _, err := svc.Upload(context.Background(), pair.name, pair.src); err != nil {
			t.Fatalf("upload %s: %v", pair.name, err)
		}
	}
	registersAfterFirst := len(client.registers)
	versionsAfterFirst := len(client.versions)

	if err := os.WriteFile(filepath.Join(srcA, "SKILL.md"), []byte("alpha v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite a: %v", err)
	}
	if err := os.RemoveAll(srcB); err != nil {
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
	if len(client.registers) != registersAfterFirst {
		t.Fatalf("expected no new registers, got %d (was %d)", len(client.registers), registersAfterFirst)
	}
	if len(client.versions) != versionsAfterFirst+1 {
		t.Fatalf("expected exactly one new version upload, got total %d (was %d)",
			len(client.versions), versionsAfterFirst)
	}
}

func TestServiceSyncUploadFailureSurfacesError(t *testing.T) {
	t.Parallel()
	svc, client, _, dir := newServiceFixture(t)
	src := writeSkillSource(t, dir, "x", map[string]string{"SKILL.md": "v1\n"})
	if _, err := svc.Upload(context.Background(), "x", src); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	client.versionErr = errors.New("transient API failure")

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
		src := writeSkillSource(t, dir, n, map[string]string{"SKILL.md": n + " body\n"})
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
	want := filepath.Join(home, DefaultSourceDir, "ship-feature")
	if got != want {
		t.Fatalf("default path: want %s got %s", want, got)
	}
}

func TestResolveSourceOverride(t *testing.T) {
	t.Parallel()
	got, err := ResolveSource("ship-feature", "./relative/skill-dir")
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
