package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/itsHabib/orchestra/internal/fsutil"
)

const (
	defaultLockTimeout = 30 * time.Second
	lockRetryDelay     = 10 * time.Millisecond
)

// Cache stores skill upload metadata. Implementations must be safe for use
// across processes — the engine and the CLI both touch the cache.
type Cache interface {
	Get(ctx context.Context, name string) (Entry, bool, error)
	Put(ctx context.Context, name string, entry *Entry) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) (map[string]Entry, error)
	Path() string
}

// FileCache is the on-disk Cache used by the CLI and engine. It serializes
// access with both an in-process mutex and a cross-process flock, and writes
// atomically (.tmp → rename) so a partial write never leaves a torn file.
type FileCache struct {
	path        string
	lockTimeout time.Duration
	mu          sync.Mutex
}

// CacheOption customizes a FileCache.
type CacheOption func(*FileCache)

// WithLockTimeout overrides the default cross-process lock timeout.
func WithLockTimeout(d time.Duration) CacheOption {
	return func(c *FileCache) {
		c.lockTimeout = d
	}
}

// NewFileCache returns a FileCache writing to the given path. Use
// DefaultCachePath for the standard `<user-config-dir>/orchestra/skills.json`
// location.
func NewFileCache(path string, opts ...CacheOption) *FileCache {
	c := &FileCache{
		path:        path,
		lockTimeout: defaultLockTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// DefaultCachePath returns the platform-appropriate skills cache location.
// Falls back to the OS temp dir if UserConfigDir fails.
func DefaultCachePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "orchestra", "skills.json")
}

// Path returns the cache file path. Useful for diagnostics.
func (c *FileCache) Path() string { return c.path }

// Get returns the cached entry for name, or ok=false if absent.
func (c *FileCache) Get(ctx context.Context, name string) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	if name == "" {
		return Entry{}, false, errors.New("skills: cache get: empty name")
	}
	reg, err := c.read()
	if err != nil {
		return Entry{}, false, err
	}
	entry, ok := reg[name]
	return entry, ok, nil
}

// List returns all cached entries.
func (c *FileCache) List(ctx context.Context) (map[string]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.read()
}

// Put writes one entry, replacing any existing one with the same name.
func (c *FileCache) Put(ctx context.Context, name string, entry *Entry) error {
	if name == "" {
		return errors.New("skills: cache put: empty name")
	}
	if entry == nil {
		return errors.New("skills: cache put: nil entry")
	}
	return c.update(ctx, func(reg map[string]Entry) error {
		reg[name] = *entry
		return nil
	})
}

// Delete removes one entry. Missing entries are silently ignored.
func (c *FileCache) Delete(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("skills: cache delete: empty name")
	}
	return c.update(ctx, func(reg map[string]Entry) error {
		delete(reg, name)
		return nil
	})
}

func (c *FileCache) update(ctx context.Context, fn func(map[string]Entry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("skills: prepare cache dir: %w", err)
	}
	release, err := c.acquireFileLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	reg, err := c.readLocked()
	if err != nil {
		return err
	}
	if err := fn(reg); err != nil {
		return err
	}
	return c.writeLocked(reg)
}

func (c *FileCache) acquireFileLock(ctx context.Context) (func(), error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.lockTimeout)
		defer cancel()
	}
	lockPath := c.path + ".lock"
	f := flock.New(lockPath, flock.SetFlag(os.O_CREATE|os.O_RDWR), flock.SetPermissions(0o644))
	locked, err := f.TryLockContext(ctx, lockRetryDelay)
	if err != nil {
		return nil, fmt.Errorf("skills: lock %s: %w", lockPath, err)
	}
	if !locked {
		return nil, fmt.Errorf("skills: lock %s: timeout", lockPath)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = f.Unlock()
		})
	}, nil
}

type cacheFile struct {
	Skills map[string]Entry `json:"skills"`
}

func (c *FileCache) read() (map[string]Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readLocked()
}

func (c *FileCache) readLocked() (map[string]Entry, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]Entry), nil
		}
		return nil, fmt.Errorf("skills: read cache %s: %w", c.path, err)
	}
	if len(data) == 0 {
		return make(map[string]Entry), nil
	}
	var file cacheFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("skills: parse cache %s: %w", c.path, err)
	}
	if file.Skills == nil {
		file.Skills = make(map[string]Entry)
	}
	return file.Skills, nil
}

func (c *FileCache) writeLocked(reg map[string]Entry) error {
	data, err := json.MarshalIndent(cacheFile{Skills: reg}, "", "  ")
	if err != nil {
		return fmt.Errorf("skills: marshal cache: %w", err)
	}
	if err := fsutil.AtomicWrite(c.path, data); err != nil {
		return fmt.Errorf("skills: write cache %s: %w", c.path, err)
	}
	return nil
}

// SortedNames returns the names from a registry sorted lexicographically.
func SortedNames(reg map[string]Entry) []string {
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
