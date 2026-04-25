package store

import "context"

// Store persists run state and user-scoped managed resource registries.
type Store interface {
	// LoadRunState reads the active run state document.
	LoadRunState(ctx context.Context) (*RunState, error)

	// SaveRunState replaces the active run state document atomically.
	SaveRunState(ctx context.Context, s *RunState) error

	// UpdateTeamState performs an atomic read-modify-write for a single team.
	UpdateTeamState(ctx context.Context, team string, fn func(*TeamState)) error

	// ArchiveRun retires the active run so a future load reports ErrNotFound.
	ArchiveRun(ctx context.Context, runID string) error

	// AcquireRunLock takes the workspace run lock and returns an idempotent release function.
	AcquireRunLock(ctx context.Context, mode LockMode) (release func(), err error)

	// GetAgent returns one cached agent record by key.
	GetAgent(ctx context.Context, key string) (*AgentRecord, bool, error)

	// PutAgent writes one cached agent record by key.
	PutAgent(ctx context.Context, key string, rec *AgentRecord) error

	// DeleteAgent removes one cached agent record by key.
	DeleteAgent(ctx context.Context, key string) error

	// ListAgents returns all cached agent records sorted by key.
	ListAgents(ctx context.Context) ([]AgentRecord, error)

	// WithAgentLock serializes a callback for one agent key.
	WithAgentLock(ctx context.Context, key string, fn func(context.Context) error) error

	// GetEnv returns one cached environment record by key.
	GetEnv(ctx context.Context, key string) (*EnvRecord, bool, error)

	// PutEnv writes one cached environment record by key.
	PutEnv(ctx context.Context, key string, rec *EnvRecord) error

	// DeleteEnv removes one cached environment record by key.
	DeleteEnv(ctx context.Context, key string) error

	// ListEnvs returns all cached environment records sorted by key.
	ListEnvs(ctx context.Context) ([]EnvRecord, error)

	// WithEnvLock serializes a callback for one environment key.
	WithEnvLock(ctx context.Context, key string, fn func(context.Context) error) error
}
