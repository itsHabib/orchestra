// Package mcp serves the orchestra control surface as an MCP server. The
// parent Claude session attaches over stdio (default) or HTTP and reads run
// state through the v1 generic tool surface (list_runs, get_run; run plus
// message tools land in the follow-up PR).
//
// Subprocess isolation is non-negotiable: a panic in a request handler must
// not kill an active run, and runs survive MCP server restarts because they
// are independent processes. The package uses os/exec with a context.
// WithoutCancel-derived parent so request-handler cancellation never
// propagates to the run subprocess.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/itsHabib/orchestra/internal/fsutil"
	"gopkg.in/yaml.v3"
)

// orchestraSubdir is the workspace child directory the engine writes
// state.json into. It mirrors cmd/root.go's `workspaceDir` constant; the MCP
// server does not pass a custom workspace through, the subprocess uses the
// engine default relative to its cmd.Dir.
const orchestraSubdir = ".orchestra"

// Entry is one row of the MCP run registry. Persisted under the registry
// JSON file at DefaultRegistryPath; one Entry per ship_design_docs call. The
// registry is the only on-disk index of MCP-managed runs — list_jobs reads
// it, then dereferences each entry's WorkspaceDir to load that run's
// state.json.
type Entry struct {
	RunID        string    `json:"run_id"`
	WorkspaceDir string    `json:"workspace_dir"`
	YAMLPath     string    `json:"yaml_path"`
	LogPath      string    `json:"log_path,omitempty"`
	RepoURL      string    `json:"repo_url"`
	DocPaths     []string  `json:"doc_paths"`
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
}

// Registry persists the run id → Entry map under a single JSON file. The file
// is rewritten atomically (.tmp → rename) on every mutation; concurrent
// access is serialized via the in-process mutex. The MCP server is the sole
// writer in the expected deployment (one process per parent Claude session),
// so a flock-style cross-process lock is not warranted at v0 — the atomic
// rename keeps readers consistent in the rare two-server case.
type Registry struct {
	path string
	mu   sync.Mutex
}

// NewRegistry returns a Registry persisting to path. The file is created
// lazily on first write. Path must be absolute or resolvable from the MCP
// server's cwd; the loader does not synthesize one.
func NewRegistry(path string) *Registry {
	return &Registry{path: path}
}

// Path returns the underlying file path. Useful for diagnostics and tests.
func (r *Registry) Path() string { return r.path }

// List returns every entry sorted by run id (lexicographic on the timestamp
// format used by NewRunID, which makes it chronological for MCP-managed
// runs). Returns an empty slice when the registry file does not exist —
// that is the expected state for a fresh MCP server.
func (r *Registry) List(ctx context.Context) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, err := r.readLocked()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(reg))
	for id := range reg {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Entry, 0, len(reg))
	for _, id := range ids {
		out = append(out, reg[id])
	}
	return out, nil
}

// Get returns the entry for runID. ok is false when no such entry exists.
func (r *Registry) Get(ctx context.Context, runID string) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	if runID == "" {
		return Entry{}, false, errors.New("mcp: registry get: empty run id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, err := r.readLocked()
	if err != nil {
		return Entry{}, false, err
	}
	e, ok := reg[runID]
	return e, ok, nil
}

// Put writes one entry, replacing any existing one with the same run id. The
// caller is expected to populate every field; nothing is filled in here.
func (r *Registry) Put(ctx context.Context, e *Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil {
		return errors.New("mcp: registry put: nil entry")
	}
	if e.RunID == "" {
		return errors.New("mcp: registry put: empty run id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, err := r.readLocked()
	if err != nil {
		return err
	}
	reg[e.RunID] = *e
	return r.writeLocked(reg)
}

// Delete removes one entry. Missing entries are silently ignored — the MCP
// server uses Delete for best-effort cleanup of crashed runs.
func (r *Registry) Delete(ctx context.Context, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runID == "" {
		return errors.New("mcp: registry delete: empty run id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, err := r.readLocked()
	if err != nil {
		return err
	}
	delete(reg, runID)
	return r.writeLocked(reg)
}

type registryFile struct {
	Runs map[string]Entry `json:"runs"`
}

func (r *Registry) readLocked() (map[string]Entry, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]Entry), nil
		}
		return nil, fmt.Errorf("mcp: read registry %s: %w", r.path, err)
	}
	if len(data) == 0 {
		return make(map[string]Entry), nil
	}
	var f registryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("mcp: parse registry %s: %w", r.path, err)
	}
	if f.Runs == nil {
		f.Runs = make(map[string]Entry)
	}
	return f.Runs, nil
}

func (r *Registry) writeLocked(reg map[string]Entry) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("mcp: prepare registry dir: %w", err)
	}
	data, err := json.MarshalIndent(registryFile{Runs: reg}, "", "  ")
	if err != nil {
		return fmt.Errorf("mcp: marshal registry: %w", err)
	}
	if err := fsutil.AtomicWrite(r.path, data); err != nil {
		return fmt.Errorf("mcp: write registry %s: %w", r.path, err)
	}
	return nil
}

// DefaultRegistryPath returns the platform-appropriate registry file location
// at <userDataDir>/orchestra/mcp-runs.json. Mirrors skills.DefaultCachePath's
// shape but lands in the data dir (not config dir) because the workspace tree
// for managed runs lives under the same parent.
func DefaultRegistryPath() string {
	return filepath.Join(userDataDir(), "orchestra", "mcp-runs.json")
}

// DefaultWorkspaceRoot returns the parent directory under which each
// MCP-managed run gets its own workspace at <root>/<run_id>/. The MCP server
// shells `orchestra run` with cmd.Dir set to the per-run directory; the
// engine then writes state.json into <run_dir>/.orchestra/state.json.
func DefaultWorkspaceRoot() string {
	return filepath.Join(userDataDir(), "orchestra", "mcp-runs")
}

// userDataDir picks the per-platform data directory. Go's stdlib only ships
// UserConfigDir / UserCacheDir; for a "data" dir we follow XDG on Unix
// ($XDG_DATA_HOME or $HOME/.local/share), Library/Application Support on
// macOS, and %LOCALAPPDATA% on Windows. Falls back to os.TempDir on any
// failure so callers always get a usable path; diagnostics tests should pass
// an explicit override rather than relying on the fallback.
func userDataDir() string {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support")
		}
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return v
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share")
		}
	}
	return os.TempDir()
}

// stateDir is where state.json lives for a given workspace dir. The engine
// writes everything under <workspace>/.orchestra/, mirroring cmd/root.go's
// workspaceDir constant.
func stateDir(workspaceDir string) string {
	return filepath.Join(workspaceDir, orchestraSubdir)
}

// NewRunID returns a UTC nanosecond-resolution timestamp suitable for use as
// a run identifier. Matches the format internal/run/service.go uses for
// engine-side run ids so the MCP-side and engine-side ids are visually
// consistent (though they identify different things — the MCP run id is the
// registry key, the engine run id lives inside state.json and is generated
// independently by the subprocess).
func NewRunID(now time.Time) string {
	return now.UTC().Format("20060102T150405.000000000Z")
}

// Spawner abstracts subprocess launch so tests can fake it without forking.
// Production callers wire ExecSpawner; tests pass a stub that just records
// the call.
type Spawner interface {
	// Start launches the run subprocess for entry e. The returned Process
	// is stored only so the caller can record a PID; the spawner is not
	// required to keep the process attached to itself. Implementations
	// must use context.WithoutCancel internally so request-handler
	// cancellation does not kill the run (DESIGN §8.4).
	Start(ctx context.Context, e *Entry) (*os.Process, error)
}

// ExecSpawner is the production Spawner. It writes the engine config to
// e.YAMLPath, opens e.LogPath for stdout/stderr, then launches the orchestra
// binary with cmd.Dir = e.WorkspaceDir. The orchestra binary path is taken
// from os.Executable() so the subprocess matches the running MCP server's
// build.
type ExecSpawner struct {
	// BinaryPath overrides the orchestra binary location. Empty defaults
	// to os.Executable() at Start time.
	BinaryPath string
}

// Start implements Spawner. The caller is expected to have already populated
// e.WorkspaceDir, e.YAMLPath, and e.LogPath; Start does not create files
// other than opening the log for append.
func (s *ExecSpawner) Start(ctx context.Context, e *Entry) (*os.Process, error) {
	if e == nil {
		return nil, errors.New("mcp: spawn: nil entry")
	}
	if e.WorkspaceDir == "" || e.YAMLPath == "" {
		return nil, errors.New("mcp: spawn: workspace_dir and yaml_path required")
	}
	bin := s.BinaryPath
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("mcp: spawn: locate orchestra binary: %w", err)
		}
		bin = exe
	}

	logFile, err := openLog(e.LogPath)
	if err != nil {
		return nil, err
	}
	// context.WithoutCancel detaches the subprocess lifetime from the
	// MCP request handler's ctx — a panic or client disconnect that
	// cancels the parent must not kill an active run.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), bin, "run", e.YAMLPath) //nolint:gosec // bin is os.Executable / tested override; yaml is recipe-generated
	cmd.Dir = e.WorkspaceDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Process-group isolation: ctx-detachment alone does not isolate from
	// OS signals (SIGTERM, Ctrl-C, Ctrl-Break) delivered to the parent's
	// group. detachProcessGroup is the per-platform Setpgid /
	// CREATE_NEW_PROCESS_GROUP shim that completes the §8.4 "runs survive
	// MCP server restarts" guarantee.
	detachProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("mcp: spawn orchestra run: %w", err)
	}
	// Reap on background; without Wait the process becomes a zombie on
	// Unix. The reaper closes the log file once the run exits so disk is
	// reclaimable. Deliberately fire-and-forget — list_jobs gets its
	// status from state.json, not from cmd.Wait's exit code.
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
	}()
	return cmd.Process, nil
}

func openLog(path string) (*os.File, error) {
	if path == "" {
		return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mcp: prepare log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("mcp: open log %s: %w", path, err)
	}
	return f, nil
}

// PrepareRun seeds the workspace for a new MCP-managed run: creates the per-
// run directory, marshals the engine config to YAML at <workspace>/orchestra.
// yaml, and computes the log path. Returns a populated Entry ready for
// Spawner.Start and Registry.Put.
//
// PrepareRun does NOT spawn anything; that is the caller's job. Splitting
// the seeding from the spawn keeps the code path testable without forking.
func PrepareRun(workspaceRoot, runID string, cfg any, repoURL string, docPaths []string) (*Entry, error) {
	if workspaceRoot == "" {
		return nil, errors.New("mcp: prepare run: empty workspace root")
	}
	if runID == "" {
		return nil, errors.New("mcp: prepare run: empty run id")
	}
	if cfg == nil {
		return nil, errors.New("mcp: prepare run: nil config")
	}
	dir := filepath.Join(workspaceRoot, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mcp: prepare workspace %s: %w", dir, err)
	}
	yamlPath := filepath.Join(dir, "orchestra.yaml")
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal config: %w", err)
	}
	if err := fsutil.AtomicWrite(yamlPath, data); err != nil {
		return nil, fmt.Errorf("mcp: write %s: %w", yamlPath, err)
	}
	return &Entry{
		RunID:        runID,
		WorkspaceDir: dir,
		YAMLPath:     yamlPath,
		LogPath:      filepath.Join(dir, "orchestra.log"),
		RepoURL:      repoURL,
		DocPaths:     append([]string(nil), docPaths...),
		StartedAt:    time.Now().UTC(),
	}, nil
}
