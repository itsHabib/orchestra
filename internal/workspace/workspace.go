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

// Workspace manages file-backed helpers under a .orchestra/ directory.
type Workspace struct {
	Path       string
	registryMu sync.Mutex
}

// ForPath returns a helper for path without touching the filesystem.
func ForPath(path string) *Workspace {
	if path == "" {
		path = ".orchestra"
	}
	return &Workspace{Path: path}
}

// Ensure creates the workspace helper directories if needed.
func Ensure(path string) (*Workspace, error) {
	if path == "" {
		path = ".orchestra"
	}
	for _, dir := range []string{path, filepath.Join(path, "results"), filepath.Join(path, "logs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating workspace dir %s: %w", dir, err)
		}
	}
	return &Workspace{Path: path}, nil
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

func (w *Workspace) registryPath() string { return filepath.Join(w.Path, "registry.json") }
func (w *Workspace) resultPath(name string) string {
	return filepath.Join(w.Path, "results", safeWorkspacePathPart(name)+".json")
}
func (w *Workspace) summaryPath(name string) string {
	return filepath.Join(w.Path, "results", safeWorkspacePathPart(name), "summary.md")
}
func (w *Workspace) logPath(name string) string {
	return filepath.Join(w.Path, "logs", safeWorkspacePathPart(name)+".log")
}
func (w *Workspace) ndjsonLogPath(name string) string {
	return filepath.Join(w.Path, "logs", safeWorkspacePathPart(name)+".ndjson")
}

// MessagesPath returns the path to the messages directory.
func (w *Workspace) MessagesPath() string {
	return filepath.Join(w.Path, "messages")
}

// atomicWrite writes data to a temp file then renames it to the target path.
func atomicWrite(path string, data []byte) error {
	return fsutil.AtomicWrite(path, data)
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

// SeedRegistry creates a fresh pending registry from the config.
func (w *Workspace) SeedRegistry(cfg *config.Config) error {
	reg := &Registry{Project: cfg.Name}
	for i := range cfg.Agents {
		reg.Agents = append(reg.Agents, RegistryEntry{
			Name:   cfg.Agents[i].Name,
			Status: "pending",
		})
	}
	return w.WriteRegistry(reg)
}

// UpdateRegistryEntry performs a read-modify-write on a single registry entry.
func (w *Workspace) UpdateRegistryEntry(name string, fn func(*RegistryEntry)) error {
	w.registryMu.Lock()
	defer w.registryMu.Unlock()

	reg, err := w.ReadRegistry()
	if err != nil {
		return err
	}
	for i := range reg.Agents {
		if reg.Agents[i].Name == name {
			fn(&reg.Agents[i])
			return w.WriteRegistry(reg)
		}
	}
	return fmt.Errorf("agent %q not found in registry", name)
}

// WriteResult writes an agent result atomically.
func (w *Workspace) WriteResult(r *AgentResult) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(w.resultPath(r.Agent), data)
}

// WriteSummary writes a text-only managed-agents deliverable atomically.
func (w *Workspace) WriteSummary(agentName, text string) error {
	path := w.summaryPath(agentName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicWrite(path, []byte(text))
}

// ReadResult reads an agent result by agent name.
func (w *Workspace) ReadResult(name string) (*AgentResult, error) {
	data, err := os.ReadFile(w.resultPath(name))
	if err != nil {
		return nil, err
	}
	var r AgentResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// LogWriter returns a writer for the agent's log file.
func (w *Workspace) LogWriter(agentName string) (io.WriteCloser, error) {
	return os.Create(w.logPath(agentName))
}

// NDJSONLogWriter returns a raw event log writer for managed-agents streams.
func (w *Workspace) NDJSONLogWriter(agentName string) (io.WriteCloser, error) {
	return os.Create(w.ndjsonLogPath(agentName))
}

func safeWorkspacePathPart(s string) string {
	if s == "" {
		return "default"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r)
		case r >= '0' && r <= '9':
			out = append(out, r)
		case r == '-' || r == '_' || r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	safe := string(out)
	if safe == "" || safe == "." || safe == ".." {
		return "default"
	}
	return safe
}
