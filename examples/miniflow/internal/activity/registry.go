package activity

import (
	"context"
	"fmt"
	"sync"
)

// HandlerFunc is the signature for activity handlers.
type HandlerFunc func(ctx context.Context, input string) (string, error)

// Registry holds registered activity handlers.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]HandlerFunc
}

// NewRegistry creates a new empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		handlers: make(map[string]HandlerFunc),
	}
}

// Register adds a handler for the given activity name.
func (r *Registry) Register(name string, handler HandlerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[name] = handler
}

// Execute runs the handler registered under the given name.
// It returns an error if no handler is registered for that name.
func (r *Registry) Execute(ctx context.Context, name string, input string) (string, error) {
	r.mu.RLock()
	handler, ok := r.handlers[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("activity type not registered: %s", name)
	}
	return handler(ctx, input)
}
