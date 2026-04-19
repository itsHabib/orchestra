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

package spawner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
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
	store        store.Store
	agents       managedAgentAPI
	environments managedEnvironmentAPI
	logger       *slog.Logger
	cfg          ManagedAgentsConfig
	clock        ManagedAgentsClock
}

// NewManagedAgentsSpawner returns a managed-agents spawner backed by the given store and SDK client.
func NewManagedAgentsSpawner(st store.Store, client *anthropic.Client, opts ...ManagedAgentsOption) *ManagedAgentsSpawner {
	return newManagedAgentsSpawner(st, &client.Beta.Agents, &client.Beta.Environments, opts...)
}

func newManagedAgentsSpawner(
	st store.Store,
	agents managedAgentAPI,
	environments managedEnvironmentAPI,
	opts ...ManagedAgentsOption,
) *ManagedAgentsSpawner {
	s := &ManagedAgentsSpawner{
		store:        st,
		agents:       agents,
		environments: environments,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: ManagedAgentsConfig{
			MaxListPages:     10,
			AgentLockTimeout: 90 * time.Second,
			EnvLockTimeout:   90 * time.Second,
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
		return AgentHandle{}, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err)
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

// withAgentLock wraps the store agent lock with a timeout and contextual error.
func (s *ManagedAgentsSpawner) withAgentLock(ctx context.Context, key string, fn func(context.Context) error) error {
	lockCtx, cancel := context.WithTimeout(ctx, s.cfg.AgentLockTimeout)
	defer cancel()
	err := s.store.WithAgentLock(lockCtx, key, fn)
	if errors.Is(err, store.ErrLockTimeout) {
		return fmt.Errorf("%w: agent key %q", err, key)
	}
	return err
}

// ensureAgentLocked runs the cache-hit-then-reconcile pipeline inside the lock.
func (s *ManagedAgentsSpawner) ensureAgentLocked(ctx context.Context, key, hash string, spec *AgentSpec) (AgentHandle, error) {
	rec, found, err := s.store.GetAgent(ctx, key)
	if err != nil {
		return AgentHandle{}, err
	}
	if found {
		handle, err := s.resolveAgentFromCache(ctx, key, hash, spec, rec)
		if err != nil {
			return AgentHandle{}, err
		}
		if handle != nil {
			return *handle, nil
		}
	}
	return s.reconcileAgent(ctx, key, hash, spec)
}

// resolveAgentFromCache verifies a cached MA agent and either reuses it,
// updates it in place when the spec drifted, or signals the caller to
// reconcile.
//
// Returns:
//   - (handle, nil)  cache entry is still valid (possibly after an in-place update)
//   - (nil, nil)     cached agent is missing or archived — caller should reconcile
//   - (nil, err)     API or store failure
func (s *ManagedAgentsSpawner) resolveAgentFromCache(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
	rec *store.AgentRecord,
) (*AgentHandle, error) {
	agent, err := s.agents.Get(ctx, rec.AgentID, anthropic.BetaAgentGetParams{})
	switch {
	case IsAPIStatus(err, http.StatusNotFound):
		s.logger.Debug("cached agent missing; falling through to reconcile", "key", key, "agent_id", rec.AgentID)
		return nil, nil
	case err != nil:
		return nil, err
	case isAgentArchived(agent):
		s.logger.Debug("cached agent archived; falling through to reconcile", "key", key, "agent_id", rec.AgentID)
		return nil, nil
	}

	if rec.SpecHash != hash {
		return s.updateAgentForSpecDrift(ctx, key, hash, spec, agent)
	}
	return s.refreshAgentRecord(ctx, key, rec, agent)
}

// refreshAgentRecord updates the cache record's last-used timestamp (and
// records an out-of-band version change if one happened) then returns the
// existing agent handle.
func (s *ManagedAgentsSpawner) refreshAgentRecord(
	ctx context.Context,
	key string,
	rec *store.AgentRecord,
	agent *anthropic.BetaManagedAgentsAgent,
) (*AgentHandle, error) {
	if rec.Version != int(agent.Version) {
		s.logger.Warn("agent version drifted outside orchestra",
			"key", key, "previous_version", rec.Version, "current_version", agent.Version)
		rec.Version = int(agent.Version)
	} else {
		s.logger.Debug("agent cache hit", "key", key, "agent_id", agent.ID)
	}
	rec.LastUsed = s.now()
	if err := s.store.PutAgent(ctx, key, rec); err != nil {
		return nil, err
	}
	handle := handleFromMAAgent(agent)
	return &handle, nil
}

// updateAgentForSpecDrift issues an MA Update to bring an existing agent back
// in line with the current spec, then refreshes the cache record.
func (s *ManagedAgentsSpawner) updateAgentForSpecDrift(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
	agent *anthropic.BetaManagedAgentsAgent,
) (*AgentHandle, error) {
	updated, err := s.agents.Update(ctx, agent.ID, toAgentUpdateParams(spec, key, agent.Version))
	if err != nil {
		return nil, err
	}
	s.logger.Info("agent updated due to spec drift", "key", key, "new_version", updated.Version)
	if err := s.putAgentRecord(ctx, key, hash, spec, updated); err != nil {
		return nil, err
	}
	handle := handleFromMAAgent(updated)
	return &handle, nil
}

// reconcileAgent runs when the cache has no usable record. It looks for an
// existing orchestra-tagged MA agent to adopt; if none exists it creates a
// fresh one.
func (s *ManagedAgentsSpawner) reconcileAgent(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
) (AgentHandle, error) {
	candidate, err := s.findAdoptableAgent(ctx, spec, key)
	if err != nil {
		return AgentHandle{}, err
	}
	if candidate == nil {
		return s.createAgent(ctx, key, hash, spec)
	}
	return s.adoptAgent(ctx, key, hash, spec, candidate)
}

// findAdoptableAgent scans MA pages (bounded by MaxListPages) for orchestra-
// tagged agents matching key. Returns nil when none exist; when multiple
// candidates exist the most recently updated one is returned and a warning is
// logged so operators can clean up duplicates.
func (s *ManagedAgentsSpawner) findAdoptableAgent(
	ctx context.Context,
	spec *AgentSpec,
	key string,
) (*anthropic.BetaManagedAgentsAgent, error) {
	matches, err := s.listOrchestraAgents(ctx, spec.Project, spec.Role, key)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		sort.Slice(matches, func(i, j int) bool {
			return matches[i].UpdatedAt.After(matches[j].UpdatedAt)
		})
		s.logger.Warn("multiple orchestra-tagged MA agents match key; adopting most recently updated",
			"key", key, "match_ids", agentIDs(matches))
		return &matches[0], nil
	}
}

// createAgent provisions a brand-new MA agent and writes the cache record.
func (s *ManagedAgentsSpawner) createAgent(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
) (AgentHandle, error) {
	created, err := s.agents.New(ctx, toAgentCreateParams(spec, key))
	if err != nil {
		return AgentHandle{}, err
	}
	s.logger.Info("agent created", "key", key, "agent_id", created.ID)
	if err := s.putAgentRecord(ctx, key, hash, spec, created); err != nil {
		s.logger.Error("failed to cache created managed agent", "key", key, "agent_id", created.ID, "error", err)
		return AgentHandle{}, err
	}
	return handleFromMAAgent(created), nil
}

// adoptAgent takes over an existing orchestra-tagged MA agent. If the agent's
// current spec hash matches our desired hash we simply record it; otherwise we
// push an Update to bring it in line before recording.
func (s *ManagedAgentsSpawner) adoptAgent(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
	agent *anthropic.BetaManagedAgentsAgent,
) (AgentHandle, error) {
	adoptHash, err := hashFromMAAgent(agent)
	if err != nil {
		return AgentHandle{}, err
	}
	if adoptHash == hash {
		s.logger.Debug("agent adopted", "key", key, "agent_id", agent.ID)
		if err := s.putAgentRecord(ctx, key, hash, spec, agent); err != nil {
			return AgentHandle{}, err
		}
		return handleFromMAAgent(agent), nil
	}

	updated, err := s.agents.Update(ctx, agent.ID, toAgentUpdateParams(spec, key, agent.Version))
	if err != nil {
		return AgentHandle{}, err
	}
	s.logger.Info("adopted agent updated to match spec",
		"key", key, "agent_id", updated.ID, "new_version", updated.Version)
	if err := s.putAgentRecord(ctx, key, hash, spec, updated); err != nil {
		return AgentHandle{}, err
	}
	return handleFromMAAgent(updated), nil
}

// listOrchestraAgents pages through MA agents (up to MaxListPages) and returns
// every non-archived match for the given key. Unlike a typical "find first"
// scan, this collects all matches so adoptable duplicates on later pages are
// visible to findAdoptableAgent's dedup warning.
func (s *ManagedAgentsSpawner) listOrchestraAgents(
	ctx context.Context,
	project string,
	role string,
	key string,
) ([]anthropic.BetaManagedAgentsAgent, error) {
	var matches []anthropic.BetaManagedAgentsAgent
	params := anthropic.BetaAgentListParams{Limit: anthropic.Int(100)}
	for pageNum := 0; pageNum < s.cfg.MaxListPages; pageNum++ {
		page, err := s.agents.List(ctx, params)
		if err != nil {
			return nil, err
		}
		for i := range page.Data {
			agent := &page.Data[i]
			if isAgentArchived(agent) {
				continue
			}
			if agent.Name == key && taggedAgent(agent.Metadata, project, role) {
				matches = append(matches, *agent)
			}
		}
		if page.NextPage == "" {
			return matches, nil
		}
		params.Page = param.NewOpt(page.NextPage)
	}
	s.logger.Info("agent adopt scan reached page ceiling", "key", key, "max_pages", s.cfg.MaxListPages)
	return matches, nil
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
		return EnvHandle{}, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err)
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

func (s *ManagedAgentsSpawner) withEnvLock(ctx context.Context, key string, fn func(context.Context) error) error {
	lockCtx, cancel := context.WithTimeout(ctx, s.cfg.EnvLockTimeout)
	defer cancel()
	err := s.store.WithEnvLock(lockCtx, key, fn)
	if errors.Is(err, store.ErrLockTimeout) {
		return fmt.Errorf("%w: env key %q", err, key)
	}
	return err
}

func (s *ManagedAgentsSpawner) ensureEnvLocked(ctx context.Context, key, hash string, spec *EnvSpec) (EnvHandle, error) {
	rec, found, err := s.store.GetEnv(ctx, key)
	if err != nil {
		return EnvHandle{}, err
	}
	if found {
		handle, err := s.resolveEnvFromCache(ctx, key, hash, spec, rec)
		if err != nil {
			return EnvHandle{}, err
		}
		if handle != nil {
			return *handle, nil
		}
	}
	return s.reconcileEnv(ctx, key, hash, spec)
}

// resolveEnvFromCache mirrors resolveAgentFromCache for environments. The one
// structural difference: the MA API cannot update an environment in place, so
// drift triggers archive-and-recreate via replaceEnv.
func (s *ManagedAgentsSpawner) resolveEnvFromCache(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
	rec *store.EnvRecord,
) (*EnvHandle, error) {
	env, err := s.environments.Get(ctx, rec.EnvID, anthropic.BetaEnvironmentGetParams{})
	switch {
	case IsAPIStatus(err, http.StatusNotFound):
		s.logger.Debug("cached environment missing; falling through to reconcile", "key", key, "env_id", rec.EnvID)
		return nil, nil
	case err != nil:
		return nil, err
	case isEnvArchived(env):
		s.logger.Debug("cached environment archived; falling through to reconcile", "key", key, "env_id", rec.EnvID)
		return nil, nil
	}

	if rec.SpecHash != hash {
		return s.replaceEnv(ctx, key, hash, spec, env)
	}

	s.logger.Debug("environment cache hit", "key", key, "env_id", env.ID)
	rec.LastUsed = s.now()
	if err := s.store.PutEnv(ctx, key, rec); err != nil {
		return nil, err
	}
	handle := handleFromMAEnv(env)
	return &handle, nil
}

// reconcileEnv handles an env cache miss: adopt an existing orchestra-tagged
// environment if one exists, otherwise create a new one.
func (s *ManagedAgentsSpawner) reconcileEnv(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
) (EnvHandle, error) {
	candidate, err := s.findAdoptableEnv(ctx, spec, key)
	if err != nil {
		return EnvHandle{}, err
	}
	if candidate == nil {
		return s.createEnv(ctx, key, hash, spec)
	}
	return s.adoptEnv(ctx, key, hash, spec, candidate)
}

func (s *ManagedAgentsSpawner) findAdoptableEnv(
	ctx context.Context,
	spec *EnvSpec,
	key string,
) (*anthropic.BetaEnvironment, error) {
	matches, err := s.listOrchestraEnvs(ctx, spec.Project, spec.Name, key)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		sort.Slice(matches, func(i, j int) bool {
			return parseMATime(matches[i].UpdatedAt).After(parseMATime(matches[j].UpdatedAt))
		})
		s.logger.Warn("multiple orchestra-tagged MA environments match key; adopting most recently updated",
			"key", key, "match_ids", envIDs(matches))
		return &matches[0], nil
	}
}

func (s *ManagedAgentsSpawner) createEnv(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
) (EnvHandle, error) {
	created, err := s.environments.New(ctx, toEnvCreateParams(spec, key))
	if err != nil {
		return EnvHandle{}, err
	}
	s.logger.Info("environment created", "key", key, "env_id", created.ID)
	if err := s.putEnvRecord(ctx, key, hash, spec, created); err != nil {
		s.logger.Error("failed to cache created managed environment", "key", key, "env_id", created.ID, "error", err)
		return EnvHandle{}, err
	}
	return handleFromMAEnv(created), nil
}

// adoptEnv takes over an existing orchestra-tagged env. If the env's current
// spec hash matches we record it as-is; if it drifted we archive and recreate
// (the MA API does not support in-place env updates).
func (s *ManagedAgentsSpawner) adoptEnv(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
	env *anthropic.BetaEnvironment,
) (EnvHandle, error) {
	adoptHash, err := hashFromMAEnv(env)
	if err != nil {
		return EnvHandle{}, err
	}
	if adoptHash == hash {
		s.logger.Debug("environment adopted", "key", key, "env_id", env.ID)
		if err := s.putEnvRecord(ctx, key, hash, spec, env); err != nil {
			return EnvHandle{}, err
		}
		return handleFromMAEnv(env), nil
	}
	handle, err := s.replaceEnv(ctx, key, hash, spec, env)
	if err != nil {
		return EnvHandle{}, err
	}
	return *handle, nil
}

// replaceEnv archives oldEnv and creates a fresh environment from spec. Used
// when a cached or adopted environment has drifted from the desired spec —
// the MA API archives environments instead of updating them in place, so
// existing sessions keep running on the old env while new sessions use the
// replacement.
func (s *ManagedAgentsSpawner) replaceEnv(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
	oldEnv *anthropic.BetaEnvironment,
) (*EnvHandle, error) {
	if _, err := s.environments.Archive(ctx, oldEnv.ID, anthropic.BetaEnvironmentArchiveParams{}); err != nil {
		return nil, err
	}
	created, err := s.environments.New(ctx, toEnvCreateParams(spec, key))
	if err != nil {
		return nil, err
	}
	s.logger.Info("environment recreated due to spec drift",
		"key", key, "old_env_id", oldEnv.ID, "new_env_id", created.ID)
	if err := s.putEnvRecord(ctx, key, hash, spec, created); err != nil {
		return nil, err
	}
	handle := handleFromMAEnv(created)
	return &handle, nil
}

func (s *ManagedAgentsSpawner) listOrchestraEnvs(
	ctx context.Context,
	project string,
	name string,
	key string,
) ([]anthropic.BetaEnvironment, error) {
	var matches []anthropic.BetaEnvironment
	params := anthropic.BetaEnvironmentListParams{Limit: anthropic.Int(100)}
	for pageNum := 0; pageNum < s.cfg.MaxListPages; pageNum++ {
		page, err := s.environments.List(ctx, params)
		if err != nil {
			return nil, err
		}
		for i := range page.Data {
			env := &page.Data[i]
			if isEnvArchived(env) {
				continue
			}
			if env.Name == key && taggedEnv(env.Metadata, project, name) {
				matches = append(matches, *env)
			}
		}
		if page.NextPage == "" {
			return matches, nil
		}
		params.Page = param.NewOpt(page.NextPage)
	}
	s.logger.Info("environment adopt scan reached page ceiling", "key", key, "max_pages", s.cfg.MaxListPages)
	return matches, nil
}

func (s *ManagedAgentsSpawner) putAgentRecord(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
	agent *anthropic.BetaManagedAgentsAgent,
) error {
	now := s.now()
	return s.store.PutAgent(ctx, key, &store.AgentRecord{
		Key:       key,
		Project:   spec.Project,
		Role:      spec.Role,
		AgentID:   agent.ID,
		Version:   int(agent.Version),
		SpecHash:  hash,
		UpdatedAt: now,
		LastUsed:  now,
	})
}

func (s *ManagedAgentsSpawner) putEnvRecord(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
	env *anthropic.BetaEnvironment,
) error {
	now := s.now()
	return s.store.PutEnv(ctx, key, &store.EnvRecord{
		Key:       key,
		Project:   spec.Project,
		Name:      spec.Name,
		EnvID:     env.ID,
		SpecHash:  hash,
		UpdatedAt: now,
		LastUsed:  now,
	})
}

func (s *ManagedAgentsSpawner) now() time.Time {
	return s.clock().UTC()
}

// StartSession is implemented in P1.4.
func (s *ManagedAgentsSpawner) StartSession(context.Context, StartSessionRequest) (Session, error) {
	return nil, ErrUnsupported
}

// ResumeSession is implemented in P1.8.
func (s *ManagedAgentsSpawner) ResumeSession(context.Context, string) (Session, error) {
	return nil, ErrUnsupported
}

func agentCacheKey(spec *AgentSpec) (string, error) {
	if spec.Project == "" || spec.Role == "" {
		return "", fmt.Errorf("%w: agent spec requires project and role", store.ErrInvalidArgument)
	}
	if strings.Contains(spec.Project, cacheKeySeparator) || strings.Contains(spec.Role, cacheKeySeparator) {
		return "", fmt.Errorf("%w: agent project/role must not contain %q", store.ErrInvalidArgument, cacheKeySeparator)
	}
	return spec.Project + cacheKeySeparator + spec.Role, nil
}

func envCacheKey(spec *EnvSpec) (string, error) {
	if spec.Project == "" || spec.Name == "" {
		return "", fmt.Errorf("%w: environment spec requires project and name", store.ErrInvalidArgument)
	}
	if strings.Contains(spec.Project, cacheKeySeparator) || strings.Contains(spec.Name, cacheKeySeparator) {
		return "", fmt.Errorf("%w: environment project/name must not contain %q", store.ErrInvalidArgument, cacheKeySeparator)
	}
	return spec.Project + cacheKeySeparator + spec.Name, nil
}

// AgentCacheKeyFromMetadata reconstructs the cache key for an orchestra-tagged
// MA agent from its metadata map. Returns ("", false) if the agent isn't
// tagged as v2 orchestra-managed or is missing required tags.
func AgentCacheKeyFromMetadata(metadata map[string]string) (string, bool) {
	if metadata[orchestraMetadataVersion] != orchestraVersionV2 {
		return "", false
	}
	project := metadata[orchestraMetadataProject]
	role := metadata[orchestraMetadataRole]
	if project == "" || role == "" {
		return "", false
	}
	return project + cacheKeySeparator + role, true
}

// IsAPIStatus reports whether err is an Anthropic API error carrying the given
// HTTP status code.
func IsAPIStatus(err error, code int) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

func normalizeAgentSpec(spec *AgentSpec) {
	spec.Project = firstNonEmpty(spec.Project, spec.Metadata[orchestraMetadataProject])
	spec.Role = firstNonEmpty(spec.Role, spec.Metadata[orchestraMetadataRole])
}

func normalizeEnvSpec(spec *EnvSpec) {
	spec.Project = firstNonEmpty(spec.Project, spec.Metadata[orchestraMetadataProject])
}

func taggedAgent(metadata map[string]string, project, role string) bool {
	return metadata[orchestraMetadataProject] == project &&
		metadata[orchestraMetadataRole] == role &&
		metadata[orchestraMetadataVersion] == orchestraVersionV2
}

func taggedEnv(metadata map[string]string, project, name string) bool {
	return metadata[orchestraMetadataProject] == project &&
		metadata[orchestraMetadataEnv] == name &&
		metadata[orchestraMetadataVersion] == orchestraVersionV2
}

func isAgentArchived(agent *anthropic.BetaManagedAgentsAgent) bool {
	return agent != nil && !agent.ArchivedAt.IsZero()
}

func isEnvArchived(env *anthropic.BetaEnvironment) bool {
	return env != nil && env.ArchivedAt != ""
}

func handleFromMAAgent(agent *anthropic.BetaManagedAgentsAgent) AgentHandle {
	return AgentHandle{
		ID:       agent.ID,
		Backend:  managedAgentsBackend,
		Name:     agent.Name,
		Version:  int(agent.Version),
		Model:    agent.Model.ID,
		Metadata: cloneStringMap(agent.Metadata),
	}
}

func handleFromMAEnv(env *anthropic.BetaEnvironment) EnvHandle {
	return EnvHandle{
		ID:       env.ID,
		Backend:  managedAgentsBackend,
		Name:     env.Name,
		Metadata: cloneStringMap(env.Metadata),
	}
}

func agentIDs(agents []anthropic.BetaManagedAgentsAgent) []string {
	out := make([]string, 0, len(agents))
	for i := range agents {
		out = append(out, agents[i].ID)
	}
	return out
}

func envIDs(envs []anthropic.BetaEnvironment) []string {
	out := make([]string, 0, len(envs))
	for i := range envs {
		out = append(out, envs[i].ID)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseMATime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
