package memstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/itsHabib/orchestra/pkg/store"
)

// MemStore is an in-memory Store implementation for tests.
type MemStore struct {
	mu        sync.Mutex
	runState  *store.RunState
	agents    map[string]store.AgentRecord
	envs      map[string]store.EnvRecord
	exclusive bool
	shared    int
	locks     map[string]chan struct{}
}

// New returns an empty in-memory store.
func New() *MemStore {
	return &MemStore{
		agents: make(map[string]store.AgentRecord),
		envs:   make(map[string]store.EnvRecord),
		locks:  make(map[string]chan struct{}),
	}
}

// LoadRunState reads the active in-memory run state.
func (m *MemStore) LoadRunState(ctx context.Context) (*store.RunState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runState == nil {
		return nil, store.ErrNotFound
	}
	return cloneRunState(m.runState), nil
}

// SaveRunState replaces the active in-memory run state.
func (m *MemStore) SaveRunState(ctx context.Context, s *store.RunState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runState = cloneRunState(s)
	return nil
}

// UpdateTeamState performs a serialized read-modify-write on one team entry.
func (m *MemStore) UpdateTeamState(ctx context.Context, team string, fn func(*store.TeamState)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runState == nil {
		return store.ErrNotFound
	}
	if m.runState.Teams == nil {
		m.runState.Teams = make(map[string]store.TeamState)
	}
	ts := m.runState.Teams[team]
	fn(&ts)
	m.runState.Teams[team] = ts
	return nil
}

// ArchiveRun clears the active in-memory run state.
func (m *MemStore) ArchiveRun(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runState = nil
	return nil
}

// AcquireRunLock takes an in-memory run lock.
func (m *MemStore) AcquireRunLock(ctx context.Context, mode store.LockMode) (func(), error) {
	ctx, cancel := withDefaultDeadline(ctx)
	defer cancel()

	for {
		if release, ok := m.tryRunLock(mode); ok {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, lockErr(ctx)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (m *MemStore) tryRunLock(mode store.LockMode) (func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch mode {
	case store.LockShared:
		if m.exclusive {
			return nil, false
		}
		m.shared++
	default:
		if m.exclusive || m.shared > 0 {
			return nil, false
		}
		m.exclusive = true
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if mode == store.LockShared {
				m.shared--
			} else {
				m.exclusive = false
			}
		})
	}, true
}

// GetAgent returns one in-memory agent cache entry.
func (m *MemStore) GetAgent(ctx context.Context, key string) (*store.AgentRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.agents[key]
	if !ok {
		return nil, false, nil
	}
	return cloneAgentRecord(&rec), true, nil
}

// PutAgent writes one in-memory agent cache entry.
func (m *MemStore) PutAgent(ctx context.Context, key string, rec *store.AgentRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("%w: nil agent record", store.ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := *rec
	next.Key = key
	m.agents[key] = next
	return nil
}

// DeleteAgent removes one in-memory agent cache entry.
func (m *MemStore) DeleteAgent(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[key]; !ok {
		return store.ErrNotFound
	}
	delete(m.agents, key)
	return nil
}

// ListAgents returns all in-memory agent cache entries sorted by key.
func (m *MemStore) ListAgents(ctx context.Context) ([]store.AgentRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.AgentRecord, 0, len(m.agents))
	for key := range m.agents {
		out = append(out, m.agents[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// WithAgentLock serializes work for a single agent cache key.
func (m *MemStore) WithAgentLock(ctx context.Context, key string, fn func(context.Context) error) error {
	return m.withKeyLock(ctx, "agent:"+key, fn)
}

// GetEnv returns one in-memory environment cache entry.
func (m *MemStore) GetEnv(ctx context.Context, key string) (*store.EnvRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.envs[key]
	if !ok {
		return nil, false, nil
	}
	return cloneEnvRecord(&rec), true, nil
}

// PutEnv writes one in-memory environment cache entry.
func (m *MemStore) PutEnv(ctx context.Context, key string, rec *store.EnvRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("%w: nil env record", store.ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := *rec
	next.Key = key
	m.envs[key] = next
	return nil
}

// DeleteEnv removes one in-memory environment cache entry.
func (m *MemStore) DeleteEnv(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.envs[key]; !ok {
		return store.ErrNotFound
	}
	delete(m.envs, key)
	return nil
}

// ListEnvs returns all in-memory environment cache entries sorted by key.
func (m *MemStore) ListEnvs(ctx context.Context) ([]store.EnvRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.EnvRecord, 0, len(m.envs))
	for key := range m.envs {
		out = append(out, m.envs[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// WithEnvLock serializes work for a single environment cache key.
func (m *MemStore) WithEnvLock(ctx context.Context, key string, fn func(context.Context) error) error {
	return m.withKeyLock(ctx, "env:"+key, fn)
}

func (m *MemStore) withKeyLock(ctx context.Context, key string, fn func(context.Context) error) error {
	ctx, cancel := withDefaultDeadline(ctx)
	defer cancel()

	ch := m.lockFor(key)
	select {
	case <-ch:
		defer func() { ch <- struct{}{} }()
		return fn(ctx)
	case <-ctx.Done():
		return lockErr(ctx)
	}
}

func (m *MemStore) lockFor(key string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := m.locks[key]
	if ch == nil {
		ch = make(chan struct{}, 1)
		ch <- struct{}{}
		m.locks[key] = ch
	}
	return ch
}

func withDefaultDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 90*time.Second)
}

func lockErr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return store.ErrLockTimeout
	}
	return ctx.Err()
}

func cloneRunState(s *store.RunState) *store.RunState {
	if s == nil {
		return nil
	}
	out := *s
	if s.Teams != nil {
		out.Teams = make(map[string]store.TeamState, len(s.Teams))
		for k := range s.Teams {
			v := s.Teams[k]
			v.RepositoryArtifacts = append([]store.RepositoryArtifact(nil), v.RepositoryArtifacts...)
			v.Artifacts = append([]string(nil), v.Artifacts...)
			out.Teams[k] = v
		}
	}
	return &out
}

func cloneAgentRecord(rec *store.AgentRecord) *store.AgentRecord {
	out := *rec
	return &out
}

func cloneEnvRecord(rec *store.EnvRecord) *store.EnvRecord {
	out := *rec
	return &out
}
