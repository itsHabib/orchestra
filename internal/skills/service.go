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
	"sort"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/itsHabib/orchestra/internal/machost"
)

// Clock returns the current time. Replaceable in tests.
type Clock func() time.Time

// Service uploads SKILL.md files to the Anthropic Files API and caches the
// returned file_ids for reuse on session creation.
type Service struct {
	cache  Cache
	client UploadClient
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

// New returns a Service backed by the given cache and Files API client.
func New(cache Cache, client UploadClient, opts ...Option) *Service {
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
	return New(cache, &client.Beta.Files, opts...), nil
}

// Lookup recomputes the source file's hash and reports drift against the cache.
// Useful for `skills ls` and for engine-side resolution prior to session
// creation. SourceMissing is set when the cached source_path no longer exists,
// in which case Drifted is left false and CurrentHash is empty.
func (s *Service) Lookup(ctx context.Context, name string) (Lookup, error) {
	if name == "" {
		return Lookup{}, errors.New("skills: lookup: empty name")
	}
	entry, found, err := s.cache.Get(ctx, name)
	if err != nil {
		return Lookup{}, err
	}
	out := Lookup{Name: name, Entry: entry, Found: found}
	if !found {
		return out, nil
	}
	if entry.SourcePath == "" {
		return out, errors.New("skills: lookup: cached entry has no source_path")
	}
	data, err := os.ReadFile(entry.SourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.SourceMissing = true
			return out, nil
		}
		return out, fmt.Errorf("skills: lookup: read %s: %w", entry.SourcePath, err)
	}
	hash := ContentHash(data)
	out.CurrentHash = hash
	out.Drifted = hash != entry.ContentHash
	return out, nil
}

// Upload uploads SKILL.md from sourcePath under the given name, caching the
// returned file_id. If the cache already has an entry whose content_hash
// matches the source, the upload is skipped and the existing entry is
// returned with action=SyncUpToDate.
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
	data, err := os.ReadFile(abs)
	if err != nil {
		return SyncResult{Name: name, SourceMissing: errors.Is(err, os.ErrNotExist)},
			fmt.Errorf("skills: upload: read %s: %w", abs, err)
	}
	hash := ContentHash(data)

	prior, found, err := s.cache.Get(ctx, name)
	if err != nil {
		return SyncResult{}, err
	}
	if found && prior.ContentHash == hash && prior.FileID != "" {
		// Source unchanged and a file_id is already cached. Refresh the
		// SourcePath in case the user re-ran `upload --from <new-path>` on
		// identical content; the more recent path is the better diagnostic.
		if prior.SourcePath != abs {
			prior.SourcePath = abs
			if err := s.cache.Put(ctx, name, &prior); err != nil {
				return SyncResult{}, err
			}
		}
		s.logger.Debug("skill upload skipped — content unchanged",
			"name", name, "file_id", prior.FileID, "hash", hash)
		return SyncResult{Name: name, Entry: prior, Action: SyncUpToDate}, nil
	}

	entry, err := s.uploadAndCache(ctx, name, abs, data, hash)
	if err != nil {
		return SyncResult{Name: name}, err
	}
	result := SyncResult{Name: name, Entry: entry, Action: SyncReuploaded}
	if found {
		result.PreviousFile = prior.FileID
	}
	return result, nil
}

// Sync re-uploads any cached skills whose source has drifted. Skills whose
// source files no longer exist are skipped (SourceMissing=true) without
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
	data, err := os.ReadFile(entry.SourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SyncResult{Name: name, Entry: *entry, Action: SyncSkipped, SourceMissing: true,
				Err: fmt.Errorf("source missing: %s", entry.SourcePath)}
		}
		return SyncResult{Name: name, Entry: *entry, Action: SyncSkipped,
			Err: fmt.Errorf("read source: %w", err)}
	}
	hash := ContentHash(data)
	if hash == entry.ContentHash {
		return SyncResult{Name: name, Entry: *entry, Action: SyncUpToDate}
	}
	updated, err := s.uploadAndCache(ctx, name, entry.SourcePath, data, hash)
	if err != nil {
		return SyncResult{Name: name, Entry: *entry, Err: err}
	}
	return SyncResult{
		Name:         name,
		Entry:        updated,
		Action:       SyncReuploaded,
		PreviousFile: entry.FileID,
	}
}

func (s *Service) uploadAndCache(ctx context.Context, name, sourcePath string, data []byte, hash string) (Entry, error) {
	filename := filepath.Base(sourcePath)
	resp, err := s.client.Upload(ctx, anthropic.BetaFileUploadParams{
		File: &namedReader{Reader: bytes.NewReader(data), name: filename},
	})
	if err != nil {
		return Entry{}, fmt.Errorf("skills: upload %s: %w", name, err)
	}
	entry := Entry{
		FileID:      resp.ID,
		ContentHash: hash,
		SourcePath:  sourcePath,
		Filename:    resp.Filename,
		UploadedAt:  s.clock(),
	}
	if entry.Filename == "" {
		entry.Filename = filename
	}
	if err := s.cache.Put(ctx, name, &entry); err != nil {
		return Entry{}, err
	}
	s.logger.Info("skill uploaded",
		"name", name, "file_id", entry.FileID, "filename", entry.Filename, "hash", hash)
	return entry, nil
}

// SortedLookups returns Lookups for each cached skill, sorted by name. Useful
// for `skills ls` printing.
func (s *Service) SortedLookups(ctx context.Context) ([]Lookup, error) {
	reg, err := s.cache.List(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Lookup, 0, len(names))
	for _, name := range names {
		look, err := s.Lookup(ctx, name)
		if err != nil {
			return nil, err
		}
		out = append(out, look)
	}
	return out, nil
}

// namedReader gives the SDK's multipart encoder a filename. Without it the
// uploaded file ends up as `anonymous_file`, which is harder to recognize in
// the dashboard and in `Files.List` output.
type namedReader struct {
	io.Reader
	name string
}

func (r *namedReader) Filename() string { return r.name }
