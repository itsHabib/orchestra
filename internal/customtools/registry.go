package customtools

import (
	"errors"
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Handler{}
)

// Register inserts h into the package-level registry under its tool name.
// Idempotent: a second Register for the same name overwrites the prior
// entry. Engine startup may call Register multiple times across runs (sync.
// Once-style guarding caused awkward interactions with Reset in tests), so
// the registry treats re-registration as a no-op safety, not an error.
//
// Returns an error only on a malformed handler (nil or empty Tool().Name).
func Register(h Handler) error {
	if h == nil {
		return errors.New("customtools: register nil handler")
	}
	def := h.Tool()
	if def.Name == "" {
		return errors.New("customtools: register: tool name is empty")
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	registry[def.Name] = h
	return nil
}

// MustRegister wraps Register and panics on error. Use only in package init
// or trivially-correct startup paths where a handler-shape bug should
// fail loud.
func MustRegister(h Handler) {
	if err := Register(h); err != nil {
		panic(err)
	}
}

// Lookup returns the handler registered for name. The boolean is false when
// no handler exists; the engine reports this back as is_error so the agent
// learns it called an unknown tool.
func Lookup(name string) (Handler, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	h, ok := registry[name]
	return h, ok
}

// Definitions returns a snapshot of registered tool definitions, sorted by
// name. Useful for engine-side resolution when populating AgentSpec.Tools
// from a list of CustomTool refs (P2). Returning Definitions (not Handlers)
// keeps the AgentSpec wiring decoupled from the dispatch path.
func Definitions() []Definition {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Definition, 0, len(registry))
	for name := range registry {
		out = append(out, registry[name].Tool())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Reset empties the registry. Test-only — exported so tests in sibling
// packages (pkg/orchestra, cmd) can scrub between runs without circumventing
// Register.
func Reset() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Handler{}
}
