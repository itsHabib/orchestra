package workspace

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/fsutil"
)

// Workspace manages the .orchestra/ directory for a run.
type Workspace struct {
	Path string
	mu   sync.Mutex
}

// Init creates a new .orchestra/ workspace seeded from the config.
func Init(cfg *config.Config) (*Workspace, error) {
	wsPath := ".orchestra"
	for _, dir := range []string{wsPath, filepath.Join(wsPath, "results"), filepath.Join(wsPath, "logs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating workspace dir %s: %w", dir, err)
		}
	}

	ws := &Workspace{Path: wsPath}

	// Seed state
	state := &State{
		Project: cfg.Name,
		Teams:   make(map[string]TeamState),
	}
	for i := range cfg.Teams {
		state.Teams[cfg.Teams[i].Name] = TeamState{Status: "pending"}
	}
	if err := ws.WriteState(state); err != nil {
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
	return &Workspace{Path: path}, nil
}

func (w *Workspace) statePath() string    { return filepath.Join(w.Path, "state.json") }
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
func (w *Workspace) ReadState() (*State, error) {
	data, err := os.ReadFile(w.statePath())
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// WriteState writes state.json atomically.
func (w *Workspace) WriteState(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(w.statePath(), data)
}

// UpdateTeamState performs a read-modify-write on the state for a single team.
func (w *Workspace) UpdateTeamState(name string, fn func(*TeamState)) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	state, err := w.ReadState()
	if err != nil {
		return err
	}
	ts := state.Teams[name]
	fn(&ts)
	state.Teams[name] = ts
	return w.WriteState(state)
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
	w.mu.Lock()
	defer w.mu.Unlock()

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
