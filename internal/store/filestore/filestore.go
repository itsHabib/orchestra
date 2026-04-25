package filestore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/itsHabib/orchestra/internal/fsutil"
	"github.com/itsHabib/orchestra/internal/store"
)

const lockRetryDelay = 10 * time.Millisecond

// FileStore persists store documents in the local filesystem layout.
type FileStore struct {
	workspacePath string
	configDir     string
	stateMu       sync.Mutex
	registryMu    sync.Mutex
}

// Option customizes a FileStore.
type Option func(*FileStore)

// WithConfigDir overrides the user-scoped registry directory.
func WithConfigDir(path string) Option {
	return func(s *FileStore) {
		s.configDir = path
	}
}

// New returns a filesystem-backed store rooted at workspacePath.
func New(workspacePath string, opts ...Option) *FileStore {
	if workspacePath == "" {
		workspacePath = ".orchestra"
	}
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		userConfigDir = os.TempDir()
	}
	s := &FileStore{
		workspacePath: workspacePath,
		configDir:     filepath.Join(userConfigDir, "orchestra"),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// LoadRunState reads the active state.json document.
func (s *FileStore) LoadRunState(ctx context.Context) (*store.RunState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.loadRunState()
}

// SaveRunState atomically writes the active state.json document.
func (s *FileStore) SaveRunState(ctx context.Context, state *store.RunState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.saveRunState(state)
}

// UpdateTeamState performs a serialized read-modify-write on one team entry.
func (s *FileStore) UpdateTeamState(ctx context.Context, team string, fn func(*store.TeamState)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	state, err := s.loadRunState()
	if err != nil {
		return err
	}
	if state.Teams == nil {
		state.Teams = make(map[string]store.TeamState)
	}
	ts := state.Teams[team]
	fn(&ts)
	state.Teams[team] = ts
	return s.saveRunState(state)
}

// ArchiveRun moves active workspace files into archive/<runID>.
func (s *FileStore) ArchiveRun(ctx context.Context, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	resolved, err := s.resolveArchiveRunID(runID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	}

	archiveDir := filepath.Join(s.workspacePath, "archive", safePathPart(resolved))
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return fmt.Errorf("creating archive directory: %w", err)
	}

	for _, name := range []string{"state.json", "registry.json", "results", "logs", "messages", "coordinator"} {
		src := filepath.Join(s.workspacePath, name)
		if _, err := os.Stat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		dst := filepath.Join(archiveDir, name)
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("preparing archive path %s: %w", dst, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("archiving %s: %w", src, err)
		}
	}
	return nil
}

// resolveArchiveRunID returns the run ID to use as the archive directory
// name. When the caller passed an explicit ID, it is used as-is. Otherwise
// the active run's RunID is read; if that is also empty, a timestamp
// stands in so the archive still has a unique path. Returns
// store.ErrNotFound when there is no active run to archive.
func (s *FileStore) resolveArchiveRunID(passed string) (string, error) {
	if passed != "" {
		return passed, nil
	}
	state, err := s.loadRunState()
	if err != nil {
		return "", err
	}
	if state.RunID != "" {
		return state.RunID, nil
	}
	return time.Now().UTC().Format("20060102T150405Z"), nil
}

// AcquireRunLock takes a file-backed run lock for the workspace.
func (s *FileStore) AcquireRunLock(ctx context.Context, mode store.LockMode) (func(), error) {
	if err := os.MkdirAll(s.workspacePath, 0o755); err != nil {
		return nil, fmt.Errorf("creating workspace directory: %w", err)
	}
	return acquireFileLock(ctx, s.runLockPath(), mode, "workspace="+s.workspacePath)
}

// GetAgent returns one user-scoped agent cache entry.
func (s *FileStore) GetAgent(ctx context.Context, key string) (*store.AgentRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	reg, err := s.readAgentRegistry()
	if err != nil {
		return nil, false, err
	}
	rec, ok := reg[key]
	if !ok {
		return nil, false, nil
	}
	out := rec
	return &out, true, nil
}

// PutAgent writes one user-scoped agent cache entry.
func (s *FileStore) PutAgent(ctx context.Context, key string, rec *store.AgentRecord) error {
	return s.updateAgents(ctx, func(reg map[string]store.AgentRecord) error {
		if rec == nil {
			return fmt.Errorf("%w: nil agent record", store.ErrInvalidArgument)
		}
		next := *rec
		next.Key = key
		reg[key] = next
		return nil
	})
}

// DeleteAgent removes one user-scoped agent cache entry.
func (s *FileStore) DeleteAgent(ctx context.Context, key string) error {
	return s.updateAgents(ctx, func(reg map[string]store.AgentRecord) error {
		if _, ok := reg[key]; !ok {
			return store.ErrNotFound
		}
		delete(reg, key)
		return nil
	})
}

// ListAgents returns all user-scoped agent cache entries sorted by key.
func (s *FileStore) ListAgents(ctx context.Context) ([]store.AgentRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reg, err := s.readAgentRegistry()
	if err != nil {
		return nil, err
	}
	out := make([]store.AgentRecord, 0, len(reg))
	for key := range reg {
		out = append(out, reg[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// WithAgentLock serializes work for a single agent cache key.
func (s *FileStore) WithAgentLock(ctx context.Context, key string, fn func(context.Context) error) error {
	return s.withKeyLock(ctx, "agent", key, fn)
}

// GetEnv returns one user-scoped environment cache entry.
func (s *FileStore) GetEnv(ctx context.Context, key string) (*store.EnvRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	reg, err := s.readEnvRegistry()
	if err != nil {
		return nil, false, err
	}
	rec, ok := reg[key]
	if !ok {
		return nil, false, nil
	}
	out := rec
	return &out, true, nil
}

// PutEnv writes one user-scoped environment cache entry.
func (s *FileStore) PutEnv(ctx context.Context, key string, rec *store.EnvRecord) error {
	return s.updateEnvs(ctx, func(reg map[string]store.EnvRecord) error {
		if rec == nil {
			return fmt.Errorf("%w: nil env record", store.ErrInvalidArgument)
		}
		next := *rec
		next.Key = key
		reg[key] = next
		return nil
	})
}

// DeleteEnv removes one user-scoped environment cache entry.
func (s *FileStore) DeleteEnv(ctx context.Context, key string) error {
	return s.updateEnvs(ctx, func(reg map[string]store.EnvRecord) error {
		if _, ok := reg[key]; !ok {
			return store.ErrNotFound
		}
		delete(reg, key)
		return nil
	})
}

// ListEnvs returns all user-scoped environment cache entries sorted by key.
func (s *FileStore) ListEnvs(ctx context.Context) ([]store.EnvRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reg, err := s.readEnvRegistry()
	if err != nil {
		return nil, err
	}
	out := make([]store.EnvRecord, 0, len(reg))
	for key := range reg {
		out = append(out, reg[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// WithEnvLock serializes work for a single environment cache key.
func (s *FileStore) WithEnvLock(ctx context.Context, key string, fn func(context.Context) error) error {
	return s.withKeyLock(ctx, "env", key, fn)
}

func (s *FileStore) loadRunState() (*store.RunState, error) {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	var state store.RunState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Teams == nil {
		state.Teams = make(map[string]store.TeamState)
	}
	return &state, nil
}

func (s *FileStore) saveRunState(state *store.RunState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.workspacePath, 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.statePath(), data)
}

func (s *FileStore) updateAgents(ctx context.Context, fn func(map[string]store.AgentRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	release, err := acquireFileLockOnly(ctx, s.registryLockPath("agents"), "agents registry")
	if err != nil {
		return err
	}
	defer release()

	reg, err := s.readAgentRegistry()
	if err != nil {
		return err
	}
	if err := fn(reg); err != nil {
		return err
	}
	return s.writeAgentRegistry(reg)
}

func (s *FileStore) updateEnvs(ctx context.Context, fn func(map[string]store.EnvRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	release, err := acquireFileLockOnly(ctx, s.registryLockPath("envs"), "envs registry")
	if err != nil {
		return err
	}
	defer release()

	reg, err := s.readEnvRegistry()
	if err != nil {
		return err
	}
	if err := fn(reg); err != nil {
		return err
	}
	return s.writeEnvRegistry(reg)
}

type agentRegistryFile struct {
	Agents map[string]store.AgentRecord `json:"agents"`
}

type envRegistryFile struct {
	Envs map[string]store.EnvRecord `json:"envs"`
}

func (s *FileStore) readAgentRegistry() (map[string]store.AgentRecord, error) {
	data, err := os.ReadFile(s.agentsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]store.AgentRecord), nil
		}
		return nil, err
	}
	var file agentRegistryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.Agents == nil {
		file.Agents = make(map[string]store.AgentRecord)
	}
	return file.Agents, nil
}

func (s *FileStore) writeAgentRegistry(reg map[string]store.AgentRecord) error {
	data, err := json.MarshalIndent(agentRegistryFile{Agents: reg}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.configDir, 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.agentsPath(), data)
}

func (s *FileStore) readEnvRegistry() (map[string]store.EnvRecord, error) {
	data, err := os.ReadFile(s.envsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]store.EnvRecord), nil
		}
		return nil, err
	}
	var file envRegistryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.Envs == nil {
		file.Envs = make(map[string]store.EnvRecord)
	}
	return file.Envs, nil
}

func (s *FileStore) writeEnvRegistry(reg map[string]store.EnvRecord) error {
	data, err := json.MarshalIndent(envRegistryFile{Envs: reg}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.configDir, 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.envsPath(), data)
}

func (s *FileStore) withKeyLock(ctx context.Context, kind, key string, fn func(context.Context) error) error {
	ctx, cancel := withDefaultDeadline(ctx)
	defer cancel()
	lockPath := filepath.Join(s.configDir, "."+kind+"-"+safeLockPart(key)+".lock")
	release, err := acquireFileLockOnly(ctx, lockPath, kind+" key "+key)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

func acquireFileLock(ctx context.Context, path string, mode store.LockMode, body string) (func(), error) {
	ctx, cancel := withDefaultDeadline(ctx)
	defer cancel()
	f := flock.New(path, flock.SetFlag(os.O_CREATE|os.O_RDWR), flock.SetPermissions(0o644))
	var (
		locked bool
		err    error
	)
	if mode == store.LockShared {
		locked, err = f.TryRLockContext(ctx, lockRetryDelay)
	} else {
		locked, err = f.TryLockContext(ctx, lockRetryDelay)
	}
	if err != nil {
		return nil, lockError(path, body, err)
	}
	if !locked {
		return nil, lockError(path, body, store.ErrLockTimeout)
	}
	// Refresh holder metadata in a sibling file so a stale exclusive record
	// from a prior holder cannot mislead lockError after an
	// exclusive→released→shared transition. For concurrent shared holders
	// this is last-writer-wins, which is fine for best-effort diagnostics.
	// The metadata lives next to the lockfile (not in it) because Windows
	// LockFileEx is mandatory: any other handle — including one in the same
	// process — is blocked from writing to the locked byte range.
	if err := os.WriteFile(holderPath(path), []byte(lockBody(body)), 0o644); err != nil {
		_ = f.Unlock()
		return nil, fmt.Errorf("writing lockfile %s: %w", path, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = f.Unlock()
		})
	}, nil
}

func acquireFileLockOnly(ctx context.Context, path, body string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	return acquireFileLock(ctx, path, store.LockExclusive, body)
}

func withDefaultDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 90*time.Second)
}

func lockError(path, body string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, store.ErrLockTimeout) {
		holder := readHolder(path)
		if holder != "" {
			return fmt.Errorf("%w: %s held by %s", store.ErrLockTimeout, body, holder)
		}
		return fmt.Errorf("%w: %s", store.ErrLockTimeout, body)
	}
	return err
}

func readHolder(path string) string {
	data, err := os.ReadFile(holderPath(path))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func holderPath(path string) string {
	return path + ".holder"
}

func lockBody(body string) string {
	return fmt.Sprintf("pid=%d\ncreated_at=%s\n%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339), body)
}

func safePathPart(s string) string {
	if s == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func safeLockPart(s string) string {
	if s == "" {
		return "default"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func (s *FileStore) statePath() string { return filepath.Join(s.workspacePath, "state.json") }
func (s *FileStore) runLockPath() string {
	return filepath.Join(s.workspacePath, "run.lock")
}
func (s *FileStore) agentsPath() string { return filepath.Join(s.configDir, "agents.json") }
func (s *FileStore) envsPath() string   { return filepath.Join(s.configDir, "envs.json") }
func (s *FileStore) registryLockPath(name string) string {
	return filepath.Join(s.configDir, "."+name+".json.lock")
}
