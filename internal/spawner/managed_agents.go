// Package spawner — Managed Agents backend.
//
// The ManagedAgentsSpawner owns Managed Agents environment and session
// lifecycle. Agent cache policy lives in internal/agents.Service, which this
// spawner delegates to for EnsureAgent. Environment resources still keep a
// local cache here so the same EnvSpec does not trigger a fresh API round trip
// on every invocation. Two layers back the cache:
//
//  1. A persistent record in the Store (keyed by project+role or project+name)
//     remembers which MA resource was created and the hash of the spec that
//     produced it.
//  2. Every MA resource Orchestra creates is tagged with metadata
//     (orchestraMetadataProject / orchestraMetadataRole etc.) so that, if the
//     cache ever disappears, we can scan MA for an existing resource that
//     matches the cache key and adopt it instead of creating a duplicate.
//
// EnsureAgent delegates to internal/agents.Service. EnsureEnvironment acquires
// a per-key lock via the Store and then runs this pipeline:
//
//	resolveEnvFromCache  →  hit:  reuse (or archive-and-recreate on spec drift)
//	                       miss: reconcileEnv
//	                               findAdoptableEnv → adoptEnv
//	                                                → createEnv
//
// Environments follow the same shape, except drift triggers
// archive-and-recreate (the MA API does not allow in-place env updates).
//
// The implementation is split across several files in this package:
//
//   - managed_agents.go      — this file: types, constructor, public API shells
//   - managed_agents_env.go  — env Ensure pipeline, cache helpers, hashing, and MA params
//   - managed_agents_session.go — session lifecycle and stream translation

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
	agentservice "github.com/itsHabib/orchestra/internal/agents"
	"github.com/itsHabib/orchestra/internal/machost"
	"github.com/itsHabib/orchestra/internal/store"
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

	// defaultStartSessionConcurrency caps in-flight Beta.Sessions.New calls.
	// This is a concurrency bound, not a rate limit — it bounds bursts but does
	// not enforce MA's 60-creates/min org limit on its own. Per-minute throttling
	// is handled reactively by withRetry's 429 + Retry-After path. The cap keeps
	// short bursts from blowing the budget; the retry layer handles the rest.
	// Override via WithManagedAgentsConcurrency.
	defaultStartSessionConcurrency = 20
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

	// SessionEventSeenLimit is the initial capacity hint for the event-ID
	// dedupe map. The map grows unbounded over the life of a session — the
	// reconnect backfill replays events from the start and must not re-apply
	// any previously seen event.
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

type agentEnsurer interface {
	EnsureAgent(context.Context, AgentSpec) (AgentHandle, error)
}

// ManagedAgentsSpawner creates and reuses Anthropic Managed Agents resources.
type ManagedAgentsSpawner struct {
	store         store.Store
	agentService  agentEnsurer
	environments  managedEnvironmentAPI
	sessions      managedSessionAPI
	sessionEvents managedSessionEventsAPI
	logger        *slog.Logger
	cfg           ManagedAgentsConfig
	clock         ManagedAgentsClock
	// startSem caps in-flight Beta.Sessions.New calls. Buffered, capacity ==
	// configured concurrency. nil means unbounded (only for test isolation).
	startSem chan struct{}
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

// NewHostManagedAgentsSpawner returns a host-authenticated managed-agents spawner.
func NewHostManagedAgentsSpawner(st store.Store, opts ...ManagedAgentsOption) (*ManagedAgentsSpawner, error) {
	client, err := machost.NewClient()
	if err != nil {
		return nil, err
	}
	return NewManagedAgentsSpawner(st, &client, opts...), nil
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
	if s.startSem == nil {
		s.startSem = make(chan struct{}, defaultStartSessionConcurrency)
	}
	s.agentService = agentservice.New(st, agents,
		agentservice.WithLogger(s.logger),
		agentservice.WithClock(agentservice.Clock(s.clock)),
		agentservice.WithConfig(agentservice.Config{
			MaxListPages:     s.cfg.MaxListPages,
			AgentLockTimeout: s.cfg.AgentLockTimeout,
		}),
	)
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

// WithManagedAgentsConcurrency caps the number of in-flight StartSession
// (Beta.Sessions.New) calls. n <= 0 falls back to defaultStartSessionConcurrency.
// One spawner instance, shared across all teams in a run, owns the semaphore;
// per-team spawner instances would defeat the cap.
func WithManagedAgentsConcurrency(n int) ManagedAgentsOption {
	return func(s *ManagedAgentsSpawner) {
		if n <= 0 {
			n = defaultStartSessionConcurrency
		}
		s.startSem = make(chan struct{}, n)
	}
}

//nolint:unused // Reserved for future test-only clock injection; cleanup tracked separately.
func withManagedAgentsClock(clock ManagedAgentsClock) ManagedAgentsOption {
	return func(s *ManagedAgentsSpawner) {
		s.clock = clock
	}
}

// EnsureAgent returns an active managed-agents agent resource for spec.
//
//nolint:gocritic // Spawner interface intentionally takes AgentSpec by value.
func (s *ManagedAgentsSpawner) EnsureAgent(ctx context.Context, spec AgentSpec) (AgentHandle, error) {
	if s.agentService == nil {
		return AgentHandle{}, ErrUnsupported
	}
	return s.agentService.EnsureAgent(ctx, spec)
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
