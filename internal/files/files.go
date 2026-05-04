// Package files uploads local files to the Anthropic Files API and resolves
// orchestra agent file declarations into the spawner's [spawner.ResourceRef]
// shape so MA sessions get them mounted at the configured paths.
//
// The Service is the host-side coordinator: it reads each declared local
// path, hashes the content, hits the on-disk cache (so a re-run with the
// same orchestra.yaml and same file content does not re-upload), and falls
// back to [BetaFileService.Upload] on cache miss. The upload result feeds
// directly into [spawner.ResourceRef]{Type:"file"}.
//
// Files are mounted read-only inside the MA container (per the Anthropic
// docs). Agents that need to modify content write to a new path inside the
// container; this package is strictly an *input* mechanism. Outputs come
// back via the artifact substrate (signal_completion artifacts) or via
// [BetaFileService.List]+[BetaFileService.Download] scoped to the session.
package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/itsHabib/orchestra/internal/fsutil"
)

// CacheFileName is the basename of the on-disk cache under the user's
// orchestra config dir. Mirrors [credentials.FileName] in shape.
const CacheFileName = "files-cache.json"

// FileResource is the config-side declaration of a file an agent needs
// mounted in its container. Path is a host-side filesystem path read at
// resolution time. MountPath is the absolute container path; an empty
// value defaults to /workspace/<basename(Path)>.
type FileResource struct {
	Path      string
	MountPath string
}

// ResolvedFile is the result of uploading one [FileResource]: the MA file
// id is now allocated and ready to plumb into [spawner.ResourceRef].
type ResolvedFile struct {
	FileID    string
	MountPath string
}

// Uploader is the narrow slice of [BetaFileService] the resolver needs.
// Centralized here so tests can pass a stub without depending on the
// full SDK service.
type Uploader interface {
	Upload(ctx context.Context, params anthropic.BetaFileUploadParams, opts ...option.RequestOption) (*anthropic.FileMetadata, error)
}

// Service coordinates upload + cache lookups. Construct via [New] and
// share across the run — the cache file is the only shared state.
type Service struct {
	uploader Uploader
	cache    *Cache
}

// Option customizes a [Service].
type Option func(*Service)

// WithCache overrides the cache. Default is a [Cache] rooted at
// [DefaultCachePath]. Tests pass a [Cache] rooted under [t.TempDir] so they
// don't pollute the user's config dir.
func WithCache(c *Cache) Option {
	return func(s *Service) {
		if c != nil {
			s.cache = c
		}
	}
}

// New returns a Service. uploader is the SDK [BetaFileService] in
// production; tests pass a stub.
func New(uploader Uploader, opts ...Option) *Service {
	s := &Service{
		uploader: uploader,
		cache:    NewCache(DefaultCachePath()),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Resolve uploads each declared file (or hits the cache) and returns the
// resolved file ids in the same order. Caller is expected to plumb the
// result into [spawner.StartSessionRequest.Resources] before
// [spawner.Spawner.StartSession]. Empty input returns nil, nil.
func (s *Service) Resolve(ctx context.Context, files []FileResource) ([]ResolvedFile, error) {
	if len(files) == 0 {
		return nil, nil
	}
	out := make([]ResolvedFile, 0, len(files))
	for i := range files {
		resolved, err := s.resolveOne(ctx, &files[i])
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

func (s *Service) resolveOne(ctx context.Context, fr *FileResource) (ResolvedFile, error) {
	if fr.Path == "" {
		return ResolvedFile{}, errors.New("files: path is required")
	}
	// Paths reach the file service already absolute — config.Load resolves
	// relative `files.path` entries against the YAML file's directory at
	// load time, and inline_dag callers are responsible for passing
	// absolute paths. Reject relative paths defensively so a bypass of the
	// loader does not silently upload a CWD-relative file.
	if !filepath.IsAbs(fr.Path) {
		return ResolvedFile{}, fmt.Errorf("files: path %q must be absolute (config.Load canonicalizes relative paths against the YAML's directory)", fr.Path)
	}
	// Read the file once and hash from memory: hashing then re-opening for
	// upload was a TOCTOU window where a mid-write modification produced a
	// cache entry mapping hash(v1) → file_id(v2). On the next run with v1
	// content the cache would hit but return a file id pointing at v2.
	// File-mount sizes are bounded by MA's per-file limit; reading into
	// memory is acceptable.
	data, err := os.ReadFile(fr.Path) //nolint:gosec // path is operator-controlled YAML; not a tainted input
	if err != nil {
		return ResolvedFile{}, fmt.Errorf("files: read %s: %w", fr.Path, err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	mount := fr.MountPath
	if mount == "" {
		mount = defaultMount(fr.Path)
	}

	if cached, ok := s.cache.Get(hash); ok {
		return ResolvedFile{FileID: cached, MountPath: mount}, nil
	}

	uploaded, err := s.uploader.Upload(ctx, anthropic.BetaFileUploadParams{File: bytes.NewReader(data)})
	if err != nil {
		return ResolvedFile{}, fmt.Errorf("files: upload %s: %w", fr.Path, err)
	}
	if uploaded == nil || uploaded.ID == "" {
		return ResolvedFile{}, fmt.Errorf("files: upload %s: empty file id", fr.Path)
	}

	if err := s.cache.Put(hash, uploaded.ID); err != nil {
		// Cache write failure is non-fatal: the upload succeeded, so the
		// next run will re-upload (extra cost, no correctness issue). Log
		// to stderr so a failing cache eventually gets attention.
		fmt.Fprintf(os.Stderr, "files: cache write skipped: %v\n", err)
	}
	return ResolvedFile{FileID: uploaded.ID, MountPath: mount}, nil
}

// defaultMount returns /workspace/<basename(path)> as the mount path when
// the agent did not specify one. Matches the convention the Anthropic docs
// use in their examples.
func defaultMount(path string) string {
	return "/workspace/" + filepath.Base(path)
}

// Cache maps content-hash → file-id so re-runs with unchanged files do not
// re-upload. The on-disk format is a single JSON object keyed by hash; the
// mutex serializes RMW within a single process. Cross-process writers are
// not serialized — orchestra runs are not expected to overlap on the same
// host.
type Cache struct {
	path string
	mu   sync.Mutex
}

// NewCache returns a Cache rooted at path. The file is created lazily on
// first [Cache.Put]; reads against a missing file return ok=false.
func NewCache(path string) *Cache {
	return &Cache{path: path}
}

// Path returns the underlying cache file path. Useful for diagnostics and
// tests.
func (c *Cache) Path() string { return c.path }

// Get returns the cached file id for hash, if any. ok is false on miss
// or any I/O error (including a missing file) — the resolver treats either
// as "upload again", which is always safe. A read error (corrupt JSON, fs
// outage) gets a stderr breadcrumb so a silently-broken cache still
// surfaces; the run itself proceeds because re-uploading is harmless.
func (c *Cache) Get(hash string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := c.readLocked()
	if err != nil {
		fmt.Fprintf(os.Stderr, "files: cache read skipped: %v\n", err)
		return "", false
	}
	e, ok := entries[hash]
	if !ok {
		return "", false
	}
	return e.FileID, true
}

// Put records hash → fileID and rewrites the cache file atomically.
func (c *Cache) Put(hash, fileID string) error {
	if hash == "" || fileID == "" {
		return errors.New("files: cache put: empty hash or file id")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := c.readLocked()
	if err != nil {
		return err
	}
	entries[hash] = cacheEntry{FileID: fileID, At: time.Now().UTC()}
	return c.writeLocked(entries)
}

// Delete removes hash from the cache. Missing entries are silently ignored
// — callers use Delete when they detect a stale entry (the SDK rejects the
// cached id as no-longer-extant) and want to force re-upload on the next
// resolve.
func (c *Cache) Delete(hash string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := c.readLocked()
	if err != nil {
		return err
	}
	if _, ok := entries[hash]; !ok {
		return nil
	}
	delete(entries, hash)
	return c.writeLocked(entries)
}

type cacheEntry struct {
	FileID string    `json:"file_id"`
	At     time.Time `json:"at"`
}

type cacheFile struct {
	Entries map[string]cacheEntry `json:"entries"`
}

func (c *Cache) readLocked() (map[string]cacheEntry, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]cacheEntry), nil
		}
		return nil, fmt.Errorf("files: read cache %s: %w", c.path, err)
	}
	if len(data) == 0 {
		return make(map[string]cacheEntry), nil
	}
	var f cacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("files: parse cache %s: %w", c.path, err)
	}
	if f.Entries == nil {
		f.Entries = make(map[string]cacheEntry)
	}
	return f.Entries, nil
}

func (c *Cache) writeLocked(entries map[string]cacheEntry) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("files: prepare cache dir: %w", err)
	}
	// Sort keys so the on-disk file is byte-stable across runs — useful
	// when the cache file ends up under version control or diffed by hand.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	stable := make(map[string]cacheEntry, len(entries))
	for _, k := range keys {
		stable[k] = entries[k]
	}
	data, err := json.MarshalIndent(cacheFile{Entries: stable}, "", "  ")
	if err != nil {
		return fmt.Errorf("files: marshal cache: %w", err)
	}
	if err := fsutil.AtomicWrite(c.path, data); err != nil {
		return fmt.Errorf("files: write cache %s: %w", c.path, err)
	}
	return nil
}

// DefaultCachePath returns the platform-appropriate cache file location at
// <userConfigDir>/orchestra/files-cache.json. Mirrors the credentials
// package's path convention so the same XDG / APPDATA layout governs both.
func DefaultCachePath() string {
	return filepath.Join(userConfigDir(), "orchestra", CacheFileName)
}

func userConfigDir() string {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("APPDATA"); v != "" {
			return v
		}
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return dir
	}
	return os.TempDir()
}
