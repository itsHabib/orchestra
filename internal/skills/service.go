package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/itsHabib/orchestra/internal/machost"
)

// Clock returns the current time. Replaceable in tests.
type Clock func() time.Time

// Service registers skill directories with the Anthropic Beta Skills API and
// caches the returned skill_ids for reuse on session creation.
type Service struct {
	cache  Cache
	client RegistrationClient
	logger *slog.Logger
	clock  Clock
}

// Option customizes a Service.
type Option func(*Service)

// WithLogger overrides the default discard logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) { s.logger = l }
}

// WithClock overrides the default time.Now-based clock.
func WithClock(c Clock) Option {
	return func(s *Service) { s.clock = c }
}

// New returns a Service backed by the given cache and registration client.
func New(cache Cache, client RegistrationClient, opts ...Option) *Service {
	s := &Service{
		cache:  cache,
		client: client,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clock:  func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.logger == nil {
		s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.clock == nil {
		s.clock = func() time.Time { return time.Now().UTC() }
	}
	return s
}

// NewHostService wires a Service against the host's Anthropic credentials and
// the default cache path.
func NewHostService(opts ...Option) (*Service, error) {
	client, err := machost.NewClient()
	if err != nil {
		return nil, fmt.Errorf("skills: build Anthropic client: %w", err)
	}
	cache := NewFileCache(DefaultCachePath())
	return New(cache, &skillsAdapter{svc: &client.Beta.Skills}, opts...), nil
}

// Lookup recomputes the source directory's hash and reports drift against the
// cache. Useful for `skills ls` and for engine-side resolution prior to
// session creation. SourceMissing is set when the cached source_path no longer
// exists, in which case Drifted is left false and CurrentHash is empty.
func (s *Service) Lookup(ctx context.Context, name string) (Lookup, error) {
	if name == "" {
		return Lookup{}, errors.New("skills: lookup: empty name")
	}
	entry, found, err := s.cache.Get(ctx, name)
	if err != nil {
		return Lookup{}, err
	}
	out := Lookup{Name: name, Entry: entry, Found: found}
	if !found || entry.SkillID == "" {
		// SkillID may be empty on a cache file written by an earlier
		// (Files-API) version of the package; treat such rows as
		// not-found so the caller re-registers via Upload.
		out.Found = false
		return out, nil
	}
	if entry.SourcePath == "" {
		return out, errors.New("skills: lookup: cached entry has no source_path")
	}
	hash, err := DirHash(entry.SourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.SourceMissing = true
			return out, nil
		}
		return out, fmt.Errorf("skills: lookup: hash %s: %w", entry.SourcePath, err)
	}
	out.CurrentHash = hash
	out.Drifted = hash != entry.ContentHash
	return out, nil
}

// Upload registers the skill rooted at sourcePath under the given name. If the
// cache already has an entry whose content_hash matches the source, the
// registration is skipped and the existing entry is returned with
// action=SyncUpToDate. On drift, a new skill version is published under the
// existing skill_id and the cache is updated.
//
// SourcePath in the cache is normalized to absolute so later `sync` runs can
// resolve it from any working directory.
func (s *Service) Upload(ctx context.Context, name, sourcePath string) (SyncResult, error) {
	if name == "" {
		return SyncResult{}, errors.New("skills: upload: empty name")
	}
	if sourcePath == "" {
		return SyncResult{}, errors.New("skills: upload: empty source path")
	}
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return SyncResult{}, fmt.Errorf("skills: upload: resolve %s: %w", sourcePath, err)
	}
	files, err := WalkSkillFiles(abs)
	if err != nil {
		return SyncResult{Name: name, SourceMissing: errors.Is(err, os.ErrNotExist)},
			fmt.Errorf("skills: upload: %w", err)
	}
	hash, err := DirHash(abs)
	if err != nil {
		return SyncResult{}, fmt.Errorf("skills: upload: hash %s: %w", abs, err)
	}

	prior, found, err := s.cache.Get(ctx, name)
	if err != nil {
		return SyncResult{}, err
	}
	if found && prior.SkillID != "" && prior.ContentHash == hash {
		// Source unchanged and a skill_id is already cached. Refresh
		// the SourcePath in case the user re-ran `upload --from
		// <new-path>` on identical content; the more recent path is
		// the better diagnostic.
		if prior.SourcePath != abs {
			prior.SourcePath = abs
			if err := s.cache.Put(ctx, name, &prior); err != nil {
				return SyncResult{}, err
			}
		}
		s.logger.Debug("skill upload skipped — content unchanged",
			"name", name, "skill_id", prior.SkillID, "version", prior.LatestVersion, "hash", hash)
		return SyncResult{Name: name, Entry: prior, Action: SyncUpToDate}, nil
	}

	entry, prevVersion, err := s.registerOrVersion(ctx, name, abs, files, hash, found, &prior)
	if err != nil {
		return SyncResult{Name: name}, err
	}
	if err := s.cache.Put(ctx, name, &entry); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Name: name, Entry: entry, Action: SyncReuploaded, PreviousVersion: prevVersion}, nil
}

// Sync re-registers any cached skills whose source has drifted. Skills whose
// source directories no longer exist are skipped (SourceMissing=true) without
// touching the cache; the user can decide whether to re-point or delete them.
//
// Returned slice is sorted by name for deterministic CLI output.
func (s *Service) Sync(ctx context.Context) ([]SyncResult, error) {
	reg, err := s.cache.List(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]SyncResult, 0, len(reg))
	for _, name := range SortedNames(reg) {
		entry := reg[name]
		res := s.syncOne(ctx, name, &entry)
		results = append(results, res)
	}
	return results, nil
}

func (s *Service) syncOne(ctx context.Context, name string, entry *Entry) SyncResult {
	if entry.SourcePath == "" {
		return SyncResult{Name: name, Entry: *entry, Action: SyncSkipped,
			Err: errors.New("skills: sync: cached entry has no source_path")}
	}
	files, err := WalkSkillFiles(entry.SourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SyncResult{Name: name, Entry: *entry, Action: SyncSkipped, SourceMissing: true,
				Err: fmt.Errorf("source missing: %s", entry.SourcePath)}
		}
		return SyncResult{Name: name, Entry: *entry, Action: SyncSkipped,
			Err: fmt.Errorf("walk source: %w", err)}
	}
	hash, err := DirHash(entry.SourcePath)
	if err != nil {
		return SyncResult{Name: name, Entry: *entry, Action: SyncSkipped,
			Err: fmt.Errorf("hash source: %w", err)}
	}
	if entry.SkillID != "" && hash == entry.ContentHash {
		return SyncResult{Name: name, Entry: *entry, Action: SyncUpToDate}
	}
	updated, prevVersion, err := s.registerOrVersion(ctx, name, entry.SourcePath, files, hash, entry.SkillID != "", entry)
	if err != nil {
		// Tag the result as Skipped so the CLI's switch surfaces r.Err under
		// the "skipped" branch rather than falling through to the empty
		// default case (which would print just the skill name and lose the
		// upload error entirely).
		return SyncResult{Name: name, Entry: *entry, Action: SyncSkipped, Err: err}
	}
	if err := s.cache.Put(ctx, name, &updated); err != nil {
		return SyncResult{Name: name, Entry: *entry, Action: SyncSkipped, Err: err}
	}
	return SyncResult{
		Name:            name,
		Entry:           updated,
		Action:          SyncReuploaded,
		PreviousVersion: prevVersion,
	}
}

// registerOrVersion either registers a brand-new skill or publishes a new
// version under an existing skill_id. previousFound==true and prior!=nil
// signal "we have a skill_id already, version it"; otherwise it's a fresh
// registration. Returns the new cache entry and the prior version (if any) so
// SyncResult can report what was replaced.
func (s *Service) registerOrVersion(ctx context.Context, name, sourcePath string, files []SkillFile, hash string, previousFound bool, prior *Entry) (Entry, string, error) {
	if previousFound && prior != nil && prior.SkillID != "" {
		readers := readersForFiles(name, files)
		resp, err := s.client.NewSkillVersion(ctx, prior.SkillID, anthropic.BetaSkillVersionNewParams{
			Files: readers,
		})
		if err != nil {
			return Entry{}, "", fmt.Errorf("skills: new version %s: %w", name, err)
		}
		entry := Entry{
			SkillID:       prior.SkillID,
			LatestVersion: resp.Version,
			ContentHash:   hash,
			SourcePath:    sourcePath,
			RegisteredAt:  s.clock(),
		}
		s.logger.Info("skill new version",
			"name", name, "skill_id", entry.SkillID, "version", entry.LatestVersion, "hash", hash)
		return entry, prior.LatestVersion, nil
	}

	readers := readersForFiles(name, files)
	resp, err := s.client.NewSkill(ctx, anthropic.BetaSkillNewParams{
		DisplayTitle: anthropic.String(name),
		Files:        readers,
	})
	if err != nil {
		return Entry{}, "", fmt.Errorf("skills: register %s: %w", name, err)
	}
	entry := Entry{
		SkillID:       resp.ID,
		LatestVersion: resp.LatestVersion,
		ContentHash:   hash,
		SourcePath:    sourcePath,
		RegisteredAt:  s.clock(),
	}
	s.logger.Info("skill registered",
		"name", name, "skill_id", entry.SkillID, "version", entry.LatestVersion, "hash", hash)
	return entry, "", nil
}

// readersForFiles wraps each SkillFile in a multipart-friendly reader whose
// Filename returns `<name>/<rel>` — the Anthropic Skills API requires every
// uploaded file share a top-level directory and that SKILL.md sits at its
// root, so we synthesize that prefix from the orchestra-side skill name. The
// directory name doesn't affect runtime behavior; it's just the label the
// server shows when listing the skill version.
func readersForFiles(name string, files []SkillFile) []io.Reader {
	out := make([]io.Reader, 0, len(files))
	for i := range files {
		out = append(out, &namedReader{
			Reader: bytes.NewReader(files[i].Content),
			name:   name + "/" + files[i].RelPath,
		})
	}
	return out
}

// SortedLookups returns Lookups for each cached skill, sorted by name. Useful
// for `skills ls` printing.
func (s *Service) SortedLookups(ctx context.Context) ([]Lookup, error) {
	reg, err := s.cache.List(ctx)
	if err != nil {
		return nil, err
	}
	names := SortedNames(reg)
	out := make([]Lookup, 0, len(names))
	for _, n := range names {
		look, err := s.Lookup(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, look)
	}
	return out, nil
}

// namedReader gives the SDK's multipart encoder a filename. Without it the
// uploaded file ends up as `anonymous_file`, which is harder to recognize in
// the dashboard and in skill-version listings.
type namedReader struct {
	io.Reader
	name string
}

func (r *namedReader) Filename() string { return r.name }

// skillsAdapter implements RegistrationClient against the Anthropic SDK's
// BetaSkillService. NewSkill calls Beta.Skills.New (initial registration);
// NewSkillVersion calls Beta.Skills.Versions.New (subsequent versions under
// an existing skill_id). The two SDK services live on the same struct, so a
// single *BetaSkillService is enough to dispatch both.
type skillsAdapter struct {
	svc *anthropic.BetaSkillService
}

// The params arguments come straight from the Anthropic SDK, which takes
// these structs by value. The size triggers gocritic's hugeParam lint, but
// changing the signature here would mean diverging from the SDK call shape;
// the adapter is a thin pass-through so the local cost is acceptable.

//nolint:gocritic // SDK-imposed signature.
func (a *skillsAdapter) NewSkill(ctx context.Context, params anthropic.BetaSkillNewParams, opts ...option.RequestOption) (*anthropic.BetaSkillNewResponse, error) {
	return a.svc.New(ctx, params, opts...)
}

//nolint:gocritic // SDK-imposed signature.
func (a *skillsAdapter) NewSkillVersion(ctx context.Context, skillID string, params anthropic.BetaSkillVersionNewParams, opts ...option.RequestOption) (*anthropic.BetaSkillVersionNewResponse, error) {
	return a.svc.Versions.New(ctx, skillID, params, opts...)
}
