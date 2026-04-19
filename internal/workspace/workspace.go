package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/fsutil"
	"github.com/itsHabib/orchestra/pkg/store"
	"github.com/itsHabib/orchestra/pkg/store/filestore"
)

// Workspace manages the .orchestra/ directory for a run.
type Workspace struct {
	Path       string
	store      *filestore.FileStore
	registryMu sync.Mutex
}

// Init creates a new .orchestra/ workspace seeded from the config.
func Init(ctx context.Context, cfg *config.Config) (*Workspace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wsPath := ".orchestra"
	for _, dir := range []string{wsPath, filepath.Join(wsPath, "results"), filepath.Join(wsPath, "logs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating workspace dir %s: %w", dir, err)
		}
	}

	ws := &Workspace{Path: wsPath, store: filestore.New(wsPath)}

	// Seed state
	now := time.Now().UTC()
	state := &State{
		Project:   cfg.Name,
		Backend:   "local",
		RunID:     now.Format("20060102T150405.000000000Z"),
		StartedAt: now,
		Teams:     make(map[string]TeamState),
	}
	for i := range cfg.Teams {
		state.Teams[cfg.Teams[i].Name] = TeamState{Status: "pending"}
	}
	if err := ws.WriteState(ctx, state); err != nil {
		return nil, fmt.Errorf("seeding state: %w", err)
	}

	// Seed registry
	reg := &Registry{Project: cfg.Name}
	for i := range cfg.Teams {
		reg.Teams = append(reg.Teams, RegistryEntry{
			Name:   cfg.Teams[i].Name,
			Status: "pending",
		})
	}
	if err := ws.WriteRegistry(reg); err != nil {
		return nil, fmt.Errorf("seeding registry: %w", err)
	}

	return ws, nil
}

// Open opens an existing workspace at the given path.
func Open(path string) (*Workspace, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("opening workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace path %s is not a directory", path)
	}
	return &Workspace{Path: path, store: filestore.New(path)}, nil
}

// AcquireRunLock takes the workspace run lock without otherwise opening the workspace.
func AcquireRunLock(ctx context.Context, path string, mode LockMode) (func(), error) {
	return filestore.New(path).AcquireRunLock(ctx, mode)
}

// ArchiveExistingRun moves a previous run's stateful workspace files under archive/.
func ArchiveExistingRun(ctx context.Context, path string) error {
	err := filestore.New(path).ArchiveRun(ctx, "")
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func (w *Workspace) registryPath() string { return filepath.Join(w.Path, "registry.json") }
func (w *Workspace) resultPath(name string) string {
	return filepath.Join(w.Path, "results", name+".json")
}
func (w *Workspace) logPath(name string) string {
	return filepath.Join(w.Path, "logs", name+".log")
}

// MessagesPath returns the path to the messages directory.
func (w *Workspace) MessagesPath() string {
	return filepath.Join(w.Path, "messages")
}

// atomicWrite writes data to a temp file then renames it to the target path.
func atomicWrite(path string, data []byte) error {
	return fsutil.AtomicWrite(path, data)
}

// ReadState reads state.json from the workspace.
func (w *Workspace) ReadState(ctx context.Context) (*State, error) {
	return w.store.LoadRunState(ctx)
}

// WriteState writes state.json atomically.
func (w *Workspace) WriteState(ctx context.Context, s *State) error {
	return w.store.SaveRunState(ctx, s)
}

// UpdateTeamState performs a read-modify-write on the state for a single team.
func (w *Workspace) UpdateTeamState(ctx context.Context, name string, fn func(*TeamState)) error {
	return w.store.UpdateTeamState(ctx, name, fn)
}

// ReadRegistry reads registry.json from the workspace.
func (w *Workspace) ReadRegistry() (*Registry, error) {
	data, err := os.ReadFile(w.registryPath())
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// WriteRegistry writes registry.json atomically.
func (w *Workspace) WriteRegistry(r *Registry) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(w.registryPath(), data)
}

// UpdateRegistryEntry performs a read-modify-write on a single registry entry.
func (w *Workspace) UpdateRegistryEntry(name string, fn func(*RegistryEntry)) error {
	w.registryMu.Lock()
	defer w.registryMu.Unlock()

	reg, err := w.ReadRegistry()
	if err != nil {
		return err
	}
	for i := range reg.Teams {
		if reg.Teams[i].Name == name {
			fn(&reg.Teams[i])
			return w.WriteRegistry(reg)
		}
	}
	return fmt.Errorf("team %q not found in registry", name)
}

// WriteResult writes a team result atomically.
func (w *Workspace) WriteResult(r *TeamResult) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(w.resultPath(r.Team), data)
}

// ReadResult reads a team result by team name.
func (w *Workspace) ReadResult(name string) (*TeamResult, error) {
	data, err := os.ReadFile(w.resultPath(name))
	if err != nil {
		return nil, err
	}
	var r TeamResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// LogWriter returns a writer for the team's log file.
func (w *Workspace) LogWriter(teamName string) (io.WriteCloser, error) {
	return os.Create(w.logPath(teamName))
}
