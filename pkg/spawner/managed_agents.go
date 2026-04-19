package spawner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
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
)

// ManagedAgentsConfig controls the registry-cache behavior for managed agents.
type ManagedAgentsConfig struct {
	MaxListPages     int
	AgentLockTimeout time.Duration
	EnvLockTimeout   time.Duration
	AdoptListWorkers int
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
			AdoptListWorkers: 5,
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
	if s.cfg.AdoptListWorkers <= 0 {
		s.cfg.AdoptListWorkers = 5
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
	hash := specHash(&spec)

	lockCtx, cancel := context.WithTimeout(ctx, s.cfg.AgentLockTimeout)
	defer cancel()

	var handle AgentHandle
	err = s.store.WithAgentLock(lockCtx, key, func(ctx context.Context) error {
		rec, found, err := s.store.GetAgent(ctx, key)
		if err != nil {
			return err
		}
		if found {
			h, resolved, err := s.resolveAgentFromCache(ctx, key, hash, &spec, rec)
			if err != nil {
				return err
			}
			if resolved {
				handle = h
				return nil
			}
		}
		h, err := s.adoptOrCreateAgent(ctx, key, hash, &spec)
		if err != nil {
			return err
		}
		handle = h
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrLockTimeout) {
			return AgentHandle{}, fmt.Errorf("%w: agent key %q", err, key)
		}
		return AgentHandle{}, err
	}
	return handle, nil
}

func (s *ManagedAgentsSpawner) resolveAgentFromCache(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
	rec *store.AgentRecord,
) (AgentHandle, bool, error) {
	agent, err := s.agents.Get(ctx, rec.AgentID, anthropic.BetaAgentGetParams{})
	switch {
	case isMAStatus(err, http.StatusNotFound):
		s.logger.Debug("cached agent missing; falling through to adopt", "key", key, "agent_id", rec.AgentID)
		return AgentHandle{}, false, nil
	case err != nil:
		return AgentHandle{}, false, err
	case isAgentArchived(agent):
		s.logger.Debug("cached agent archived; falling through to adopt", "key", key, "agent_id", rec.AgentID)
		return AgentHandle{}, false, nil
	case rec.SpecHash == hash && rec.Version == int(agent.Version):
		s.logger.Debug("agent cache hit", "key", key, "agent_id", agent.ID)
		rec.LastUsed = s.now()
		if err := s.store.PutAgent(ctx, key, rec); err != nil {
			return AgentHandle{}, false, err
		}
		return handleFromMAAgent(agent), true, nil
	case rec.SpecHash != hash:
		updated, err := s.agents.Update(ctx, agent.ID, toAgentUpdateParams(spec, key, agent.Version))
		if err != nil {
			return AgentHandle{}, false, err
		}
		s.logger.Info("agent updated due to spec drift", "key", key, "new_version", updated.Version)
		if err := s.putAgentRecord(ctx, key, hash, spec, updated); err != nil {
			return AgentHandle{}, false, err
		}
		return handleFromMAAgent(updated), true, nil
	default:
		s.logger.Warn("agent version drifted outside orchestra", "key", key, "previous_version", rec.Version, "current_version", agent.Version)
		rec.Version = int(agent.Version)
		rec.LastUsed = s.now()
		if err := s.store.PutAgent(ctx, key, rec); err != nil {
			return AgentHandle{}, false, err
		}
		return handleFromMAAgent(agent), true, nil
	}
}

func (s *ManagedAgentsSpawner) adoptOrCreateAgent(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
) (AgentHandle, error) {
	matches, err := s.listOrchestraAgents(ctx, spec.Project, spec.Role, key)
	if err != nil {
		return AgentHandle{}, err
	}

	switch len(matches) {
	case 0:
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
	case 1:
		return s.adoptSingleAgent(ctx, key, hash, spec, &matches[0])
	default:
		sort.Slice(matches, func(i, j int) bool { return matches[i].UpdatedAt.After(matches[j].UpdatedAt) })
		s.logger.Warn("multiple orchestra-tagged MA agents match key; adopting most recently updated", "key", key, "match_ids", agentIDs(matches))
		return s.adoptSingleAgent(ctx, key, hash, spec, &matches[0])
	}
}

func (s *ManagedAgentsSpawner) adoptSingleAgent(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
	agent *anthropic.BetaManagedAgentsAgent,
) (AgentHandle, error) {
	adoptHash, ok := hashFromMAAgent(agent)
	if ok && adoptHash == hash {
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
	s.logger.Info("adopted agent updated to match spec", "key", key, "agent_id", updated.ID, "new_version", updated.Version)
	if err := s.putAgentRecord(ctx, key, hash, spec, updated); err != nil {
		return AgentHandle{}, err
	}
	return handleFromMAAgent(updated), nil
}

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
		if len(matches) > 0 || page.NextPage == "" {
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
	hash := envSpecHash(&spec)

	lockCtx, cancel := context.WithTimeout(ctx, s.cfg.EnvLockTimeout)
	defer cancel()

	var handle EnvHandle
	err = s.store.WithEnvLock(lockCtx, key, func(ctx context.Context) error {
		rec, found, err := s.store.GetEnv(ctx, key)
		if err != nil {
			return err
		}
		if found {
			h, resolved, err := s.resolveEnvFromCache(ctx, key, hash, &spec, rec)
			if err != nil {
				return err
			}
			if resolved {
				handle = h
				return nil
			}
		}
		h, err := s.adoptOrCreateEnv(ctx, key, hash, &spec)
		if err != nil {
			return err
		}
		handle = h
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrLockTimeout) {
			return EnvHandle{}, fmt.Errorf("%w: env key %q", err, key)
		}
		return EnvHandle{}, err
	}
	return handle, nil
}

func (s *ManagedAgentsSpawner) resolveEnvFromCache(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
	rec *store.EnvRecord,
) (EnvHandle, bool, error) {
	env, err := s.environments.Get(ctx, rec.EnvID, anthropic.BetaEnvironmentGetParams{})
	switch {
	case isMAStatus(err, http.StatusNotFound):
		s.logger.Debug("cached environment missing; falling through to adopt", "key", key, "env_id", rec.EnvID)
		return EnvHandle{}, false, nil
	case err != nil:
		return EnvHandle{}, false, err
	case isEnvArchived(env):
		s.logger.Debug("cached environment archived; falling through to adopt", "key", key, "env_id", rec.EnvID)
		return EnvHandle{}, false, nil
	case rec.SpecHash == hash:
		s.logger.Debug("environment cache hit", "key", key, "env_id", env.ID)
		rec.LastUsed = s.now()
		if err := s.store.PutEnv(ctx, key, rec); err != nil {
			return EnvHandle{}, false, err
		}
		return handleFromMAEnv(env), true, nil
	default:
		return s.replaceEnv(ctx, key, hash, spec, env)
	}
}

func (s *ManagedAgentsSpawner) adoptOrCreateEnv(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
) (EnvHandle, error) {
	matches, err := s.listOrchestraEnvs(ctx, spec.Project, spec.Name, key)
	if err != nil {
		return EnvHandle{}, err
	}
	switch len(matches) {
	case 0:
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
	case 1:
		return s.adoptSingleEnv(ctx, key, hash, spec, &matches[0])
	default:
		sort.Slice(matches, func(i, j int) bool { return parseMATime(matches[i].UpdatedAt).After(parseMATime(matches[j].UpdatedAt)) })
		s.logger.Warn("multiple orchestra-tagged MA environments match key; adopting most recently updated", "key", key, "match_ids", envIDs(matches))
		return s.adoptSingleEnv(ctx, key, hash, spec, &matches[0])
	}
}

func (s *ManagedAgentsSpawner) adoptSingleEnv(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
	env *anthropic.BetaEnvironment,
) (EnvHandle, error) {
	adoptHash := hashFromMAEnv(env)
	if adoptHash == hash {
		s.logger.Debug("environment adopted", "key", key, "env_id", env.ID)
		if err := s.putEnvRecord(ctx, key, hash, spec, env); err != nil {
			return EnvHandle{}, err
		}
		return handleFromMAEnv(env), nil
	}
	return s.replaceEnvHandle(ctx, key, hash, spec, env)
}

func (s *ManagedAgentsSpawner) replaceEnv(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
	oldEnv *anthropic.BetaEnvironment,
) (EnvHandle, bool, error) {
	handle, err := s.replaceEnvHandle(ctx, key, hash, spec, oldEnv)
	return handle, err == nil, err
}

func (s *ManagedAgentsSpawner) replaceEnvHandle(
	ctx context.Context,
	key string,
	hash string,
	spec *EnvSpec,
	oldEnv *anthropic.BetaEnvironment,
) (EnvHandle, error) {
	// The Managed Agents API archives environments instead of updating them in place;
	// existing sessions continue on their original environment while future sessions use the replacement.
	if _, err := s.environments.Archive(ctx, oldEnv.ID, anthropic.BetaEnvironmentArchiveParams{}); err != nil {
		return EnvHandle{}, err
	}
	created, err := s.environments.New(ctx, toEnvCreateParams(spec, key))
	if err != nil {
		return EnvHandle{}, err
	}
	s.logger.Info("environment recreated due to spec drift", "key", key, "old_env_id", oldEnv.ID, "new_env_id", created.ID)
	if err := s.putEnvRecord(ctx, key, hash, spec, created); err != nil {
		return EnvHandle{}, err
	}
	return handleFromMAEnv(created), nil
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
		if len(matches) > 0 || page.NextPage == "" {
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
	return spec.Project + "__" + spec.Role, nil
}

func envCacheKey(spec *EnvSpec) (string, error) {
	if spec.Project == "" || spec.Name == "" {
		return "", fmt.Errorf("%w: environment spec requires project and name", store.ErrInvalidArgument)
	}
	return spec.Project + "__" + spec.Name, nil
}

func normalizeAgentSpec(spec *AgentSpec) {
	spec.Project = firstNonEmpty(spec.Project, spec.Metadata[orchestraMetadataProject])
	spec.Role = firstNonEmpty(spec.Role, spec.Metadata[orchestraMetadataRole])
}

func normalizeEnvSpec(spec *EnvSpec) {
	spec.Project = firstNonEmpty(spec.Project, spec.Metadata[orchestraMetadataProject])
}

func isMAStatus(err error, code int) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
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
