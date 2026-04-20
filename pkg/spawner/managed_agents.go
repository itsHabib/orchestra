// Package spawner — Managed Agents backend.
//
// The ManagedAgentsSpawner keeps a local cache of the Anthropic Managed Agents
// resources it has provisioned so that the same AgentSpec/EnvSpec does not
// trigger a fresh API round trip on every invocation. Two layers back the
// cache:
//
//  1. A persistent record in the Store (keyed by project+role or project+name)
//     remembers which MA resource was created and the hash of the spec that
//     produced it.
//  2. Every MA resource Orchestra creates is tagged with metadata
//     (orchestraMetadataProject / orchestraMetadataRole etc.) so that, if the
//     cache ever disappears, we can scan MA for an existing resource that
//     matches the cache key and adopt it instead of creating a duplicate.
//
// EnsureAgent / EnsureEnvironment are the entry points. Each acquires a
// per-key lock via the Store and then runs this pipeline:
//
//	resolveAgentFromCache  →  hit:  reuse (or update-in-place on spec drift)
//	                         miss: reconcileAgent
//	                                 findAdoptableAgent → adoptAgent
//	                                                    → createAgent
//
// Environments follow the same shape, except drift triggers
// archive-and-recreate (the MA API does not allow in-place env updates).
//
// The implementation is split across several files in this package:
//
//   - managed_agents.go       — this file: types, constructor, public API shells
//   - managed_agents_agent.go — agent Ensure pipeline (cache → reconcile → create/adopt)
//   - managed_agents_env.go   — env Ensure pipeline (mirror, with archive-and-recreate on drift)
//   - managed_agents_cache.go — cache record writers, key builders, metadata tag helpers, handle converters
//   - managed_agents_hash.go  — deterministic spec hashing
//   - managed_agents_params.go — MA API param builders

package spawner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"
	"github.com/itsHabib/orchestra/pkg/store"
)

const (
	managedAgentsBackend = "managed_agents"

	orchestraMetadataProject = "orchestra_project"
	orchestraMetadataRole    = "orchestra_role"
	orchestraMetadataEnv     = "orchestra_env"
	orchestraMetadataVersion = "orchestra_version"
	orchestraVersionV2       = "v2"

	// cacheKeySeparator joins the two components of a cache key. Project, role,
	// and env name are validated to not contain this sequence so that the
	// resulting key is unambiguous.
	cacheKeySeparator = "__"
)

// ManagedAgentsConfig controls the registry-cache behavior for managed agents.
type ManagedAgentsConfig struct {
	// MaxListPages caps how many pages listOrchestraAgents/Envs will scan
	// before giving up and creating a new resource. Protects against runaway
	// pagination against a large MA account.
	MaxListPages int

	// AgentLockTimeout and EnvLockTimeout bound how long EnsureAgent /
	// EnsureEnvironment will wait for the per-key lock before returning
	// ErrLockTimeout.
	AgentLockTimeout time.Duration
	EnvLockTimeout   time.Duration

	// SessionEventSeenLimit bounds the in-memory event ID dedupe ring.
	SessionEventSeenLimit int

	// API retry knobs for managed-agents API calls.
	APIMaxAttempts    int
	APIRetryBaseDelay time.Duration
	APIRetryMaxDelay  time.Duration
}

// ManagedAgentsOption customizes a ManagedAgentsSpawner.
type ManagedAgentsOption func(*ManagedAgentsSpawner)

// ManagedAgentsClock returns the current time. It is replaceable in tests.
type ManagedAgentsClock func() time.Time

type managedAgentAPI interface {
	New(context.Context, anthropic.BetaAgentNewParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error)
	Get(context.Context, string, anthropic.BetaAgentGetParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error)
	Update(context.Context, string, anthropic.BetaAgentUpdateParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error)
	List(context.Context, anthropic.BetaAgentListParams, ...option.RequestOption) (*pagination.PageCursor[anthropic.BetaManagedAgentsAgent], error)
}

type managedEnvironmentAPI interface {
	New(context.Context, anthropic.BetaEnvironmentNewParams, ...option.RequestOption) (*anthropic.BetaEnvironment, error)
	Get(context.Context, string, anthropic.BetaEnvironmentGetParams, ...option.RequestOption) (*anthropic.BetaEnvironment, error)
	Archive(context.Context, string, anthropic.BetaEnvironmentArchiveParams, ...option.RequestOption) (*anthropic.BetaEnvironment, error)
	List(context.Context, anthropic.BetaEnvironmentListParams, ...option.RequestOption) (*pagination.PageCursor[anthropic.BetaEnvironment], error)
}

// ManagedAgentsSpawner creates and reuses Anthropic Managed Agents resources.
type ManagedAgentsSpawner struct {
	store         store.Store
	agents        managedAgentAPI
	environments  managedEnvironmentAPI
	sessions      managedSessionAPI
	sessionEvents managedSessionEventsAPI
	logger        *slog.Logger
	cfg           ManagedAgentsConfig
	clock         ManagedAgentsClock
}

// NewManagedAgentsSpawner returns a managed-agents spawner backed by the given store and SDK client.
func NewManagedAgentsSpawner(st store.Store, client *anthropic.Client, opts ...ManagedAgentsOption) *ManagedAgentsSpawner {
	return newManagedAgentsSpawnerWithSessions(
		st,
		&client.Beta.Agents,
		&client.Beta.Environments,
		&client.Beta.Sessions,
		sdkSessionEventsAPI{events: &client.Beta.Sessions.Events},
		opts...,
	)
}

func newManagedAgentsSpawner(
	st store.Store,
	agents managedAgentAPI,
	environments managedEnvironmentAPI,
	opts ...ManagedAgentsOption,
) *ManagedAgentsSpawner {
	return newManagedAgentsSpawnerWithSessions(st, agents, environments, unsupportedSessionAPI{}, unsupportedSessionEventsAPI{}, opts...)
}

func newManagedAgentsSpawnerWithSessions(
	st store.Store,
	agents managedAgentAPI,
	environments managedEnvironmentAPI,
	sessions managedSessionAPI,
	sessionEvents managedSessionEventsAPI,
	opts ...ManagedAgentsOption,
) *ManagedAgentsSpawner {
	s := &ManagedAgentsSpawner{
		store:         st,
		agents:        agents,
		environments:  environments,
		sessions:      sessions,
		sessionEvents: sessionEvents,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: ManagedAgentsConfig{
			MaxListPages:          10,
			AgentLockTimeout:      90 * time.Second,
			EnvLockTimeout:        90 * time.Second,
			SessionEventSeenLimit: defaultSessionSeenLimit,
			APIMaxAttempts:        defaultAPIMaxAttempts,
			APIRetryBaseDelay:     defaultAPIRetryBase,
			APIRetryMaxDelay:      defaultAPIRetryMax,
		},
		clock: func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.cfg.MaxListPages <= 0 {
		s.cfg.MaxListPages = 10
	}
	if s.cfg.AgentLockTimeout <= 0 {
		s.cfg.AgentLockTimeout = 90 * time.Second
	}
	if s.cfg.EnvLockTimeout <= 0 {
		s.cfg.EnvLockTimeout = 90 * time.Second
	}
	if s.cfg.SessionEventSeenLimit <= 0 {
		s.cfg.SessionEventSeenLimit = defaultSessionSeenLimit
	}
	if s.cfg.APIMaxAttempts <= 0 {
		s.cfg.APIMaxAttempts = defaultAPIMaxAttempts
	}
	if s.cfg.APIRetryBaseDelay <= 0 {
		s.cfg.APIRetryBaseDelay = defaultAPIRetryBase
	}
	if s.cfg.APIRetryMaxDelay <= 0 {
		s.cfg.APIRetryMaxDelay = defaultAPIRetryMax
	}
	if s.logger == nil {
		s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.clock == nil {
		s.clock = func() time.Time { return time.Now().UTC() }
	}
	return s
}

// WithManagedAgentsLogger sets the logger used for cache decisions.
func WithManagedAgentsLogger(logger *slog.Logger) ManagedAgentsOption {
	return func(s *ManagedAgentsSpawner) {
		s.logger = logger
	}
}

// WithManagedAgentsConfig overrides managed-agents cache defaults.
func WithManagedAgentsConfig(cfg ManagedAgentsConfig) ManagedAgentsOption {
	return func(s *ManagedAgentsSpawner) {
		s.cfg = cfg
	}
}

func withManagedAgentsClock(clock ManagedAgentsClock) ManagedAgentsOption {
	return func(s *ManagedAgentsSpawner) {
		s.clock = clock
	}
}

// EnsureAgent returns an active managed-agent resource for spec.
//
//nolint:gocritic // Spawner interface intentionally takes AgentSpec by value.
func (s *ManagedAgentsSpawner) EnsureAgent(ctx context.Context, spec AgentSpec) (AgentHandle, error) {
	normalizeAgentSpec(&spec)
	key, err := agentCacheKey(&spec)
	if err != nil {
		return AgentHandle{}, err
	}
	hash, err := specHash(&spec)
	if err != nil {
		return AgentHandle{}, fmt.Errorf("%w: %w", store.ErrInvalidArgument, err)
	}

	var handle AgentHandle
	err = s.withAgentLock(ctx, key, func(ctx context.Context) error {
		h, err := s.ensureAgentLocked(ctx, key, hash, &spec)
		if err != nil {
			return err
		}
		handle = h
		return nil
	})
	if err != nil {
		return AgentHandle{}, err
	}
	return handle, nil
}

// EnsureEnvironment returns an active managed-agents environment resource for spec.
//
//nolint:gocritic // Spawner interface intentionally takes EnvSpec by value.
func (s *ManagedAgentsSpawner) EnsureEnvironment(ctx context.Context, spec EnvSpec) (EnvHandle, error) {
	normalizeEnvSpec(&spec)
	key, err := envCacheKey(&spec)
	if err != nil {
		return EnvHandle{}, err
	}
	hash, err := envSpecHash(&spec)
	if err != nil {
		return EnvHandle{}, fmt.Errorf("%w: %w", store.ErrInvalidArgument, err)
	}

	var handle EnvHandle
	err = s.withEnvLock(ctx, key, func(ctx context.Context) error {
		h, err := s.ensureEnvLocked(ctx, key, hash, &spec)
		if err != nil {
			return err
		}
		handle = h
		return nil
	})
	if err != nil {
		return EnvHandle{}, err
	}
	return handle, nil
}

func (s *ManagedAgentsSpawner) now() time.Time {
	return s.clock().UTC()
}

// IsAPIStatus reports whether err is an Anthropic API error carrying the given
// HTTP status code.
func IsAPIStatus(err error, code int) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}
