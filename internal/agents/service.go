package agents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/itsHabib/orchestra/internal/machost"
	"github.com/itsHabib/orchestra/pkg/store"
)

// New returns an agent service backed by the given store and Managed Agents client.
func New(st store.Store, ma MAClient, opts ...Option) *Service {
	s := &Service{
		store:  st,
		ma:     ma,
		logger: defaultLogger(),
		cfg: Config{
			MaxListPages:     defaultMaxListPages,
			AgentLockTimeout: defaultAgentLockTimeout,
			ListWorkers:      defaultListWorkers,
			ListPageLimit:    defaultListPageLimit,
		},
		clock: func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.cfg.MaxListPages <= 0 {
		s.cfg.MaxListPages = defaultMaxListPages
	}
	if s.cfg.AgentLockTimeout <= 0 {
		s.cfg.AgentLockTimeout = defaultAgentLockTimeout
	}
	if s.cfg.ListWorkers <= 0 {
		s.cfg.ListWorkers = defaultListWorkers
	}
	if s.cfg.ListPageLimit <= 0 {
		s.cfg.ListPageLimit = defaultListPageLimit
	}
	if s.logger == nil {
		s.logger = defaultLogger()
	}
	if s.clock == nil {
		s.clock = func() time.Time { return time.Now().UTC() }
	}
	return s
}

// NewFromSDK returns a service backed by the SDK client's Managed Agents API.
func NewFromSDK(st store.Store, client *anthropic.Client, opts ...Option) *Service {
	return New(st, &client.Beta.Agents, opts...)
}

// NewHostService builds a host-authenticated Managed Agents service.
func NewHostService(st store.Store, opts ...Option) (*Service, error) {
	client, err := machost.NewClient()
	if err != nil {
		return nil, err
	}
	return NewFromSDK(st, &client, opts...), nil
}

// EnsureAgent returns an active managed-agent resource for spec.
//
//nolint:gocritic // AgentSpec is intentionally passed by value to match the spawner API.
func (s *Service) EnsureAgent(ctx context.Context, spec AgentSpec) (AgentHandle, error) {
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

// Get returns the live Managed Agents status for one agent ID.
func (s *Service) Get(ctx context.Context, agentID string) (Status, error) {
	if s.ma == nil {
		return StatusUnreachable, errors.New("agents.Get: nil MA client")
	}
	agent, err := s.ma.Get(ctx, agentID, anthropic.BetaAgentGetParams{})
	switch {
	case IsAPIStatus(err, http.StatusNotFound):
		return StatusMissing, nil
	case err != nil:
		return StatusUnreachable, err
	case isAgentArchived(agent):
		return StatusArchived, nil
	default:
		return StatusActive, nil
	}
}

// List returns every cache record annotated with its live Managed Agents status.
func (s *Service) List(ctx context.Context) ([]Summary, error) {
	records, err := s.store.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("agents.List: %w", err)
	}
	return s.Annotate(ctx, records), nil
}

// Annotate returns the provided records annotated with live Managed Agents status.
func (s *Service) Annotate(ctx context.Context, records []store.AgentRecord) []Summary {
	rows := make([]Summary, len(records))
	if len(records) == 0 {
		return rows
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := min(s.cfg.ListWorkers, max(1, len(records)))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				rec := records[idx]
				status, err := s.Get(ctx, rec.AgentID)
				rows[idx] = Summary{Record: rec, Status: status, Err: err}
			}
		}()
	}
	for i := range records {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return rows
}

// Prune evaluates and optionally deletes stale local cache records.
func (s *Service) Prune(ctx context.Context, opts PruneOpts) (*PruneReport, error) {
	rows, err := s.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("agents.Prune: %w", err)
	}

	now := s.now()
	report := &PruneReport{
		Considered: make([]store.AgentRecord, 0, len(rows)),
		Now:        now,
		MaxAge:     opts.MaxAge,
	}
	for i := range rows {
		row := rows[i]
		report.Considered = append(report.Considered, row.Record)
		if opts.Protect != nil && opts.Protect(row.Record.Key, row.Record.AgentID) {
			continue
		}
		if StaleReason(&row, now, opts.MaxAge) != "" {
			report.Stale = append(report.Stale, row)
		}
	}

	if !opts.Apply {
		return report, nil
	}
	for i := range report.Stale {
		key := report.Stale[i].Record.Key
		err := s.store.DeleteAgent(ctx, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("agents.Prune: delete %q: %w", key, err)
		}
		report.Deleted = append(report.Deleted, key)
	}
	return report, nil
}

// Orphans returns orchestra-tagged Managed Agents not captured by exclude.
func (s *Service) Orphans(ctx context.Context, exclude func(key, agentID string) bool) ([]Orphan, error) {
	var out []Orphan
	err := s.listOrchestraAgents(ctx, true, func(agent *anthropic.BetaManagedAgentsAgent) {
		key, ok := AgentCacheKeyFromMetadata(agent.Metadata)
		if !ok {
			return
		}
		if exclude != nil && exclude(key, agent.ID) {
			return
		}
		status := StatusActive
		if isAgentArchived(agent) {
			status = StatusArchived
		}
		out = append(out, Orphan{
			Key:     key,
			AgentID: agent.ID,
			Version: agent.Version,
			Status:  status,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("agents.Orphans: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].AgentID < out[j].AgentID
	})
	return out, nil
}

// StaleReason returns the CLI-facing stale reason for one summary.
func StaleReason(row *Summary, now time.Time, olderThan time.Duration) string {
	if row == nil {
		return ""
	}
	switch row.Status {
	case StatusMissing:
		return "MA 404"
	case StatusArchived:
		return "archived on MA"
	case StatusActive, StatusUnreachable:
	}
	if olderThan > 0 && (row.Record.LastUsed.IsZero() || row.Record.LastUsed.Before(now.Add(-olderThan))) {
		return "last used older than " + olderThan.String()
	}
	return ""
}

func (s *Service) withAgentLock(ctx context.Context, key string, fn func(context.Context) error) error {
	lockCtx, cancel := context.WithTimeout(ctx, s.cfg.AgentLockTimeout)
	defer cancel()
	err := s.store.WithAgentLock(lockCtx, key, fn)
	if errors.Is(err, store.ErrLockTimeout) {
		return fmt.Errorf("%w: agent key %q", err, key)
	}
	return err
}

func (s *Service) ensureAgentLocked(ctx context.Context, key, hash string, spec *AgentSpec) (AgentHandle, error) {
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

func (s *Service) resolveAgentFromCache(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
	rec *store.AgentRecord,
) (*AgentHandle, error) {
	agent, err := s.ma.Get(ctx, rec.AgentID, anthropic.BetaAgentGetParams{})
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

func (s *Service) refreshAgentRecord(
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

func (s *Service) updateAgentForSpecDrift(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
	agent *anthropic.BetaManagedAgentsAgent,
) (*AgentHandle, error) {
	updated, err := s.ma.Update(ctx, agent.ID, toAgentUpdateParams(spec, key, agent.Version))
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

func (s *Service) reconcileAgent(
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

func (s *Service) findAdoptableAgent(
	ctx context.Context,
	spec *AgentSpec,
	key string,
) (*anthropic.BetaManagedAgentsAgent, error) {
	var matches []anthropic.BetaManagedAgentsAgent
	err := s.listOrchestraAgents(ctx, false, func(agent *anthropic.BetaManagedAgentsAgent) {
		if agent.Name == key && taggedAgent(agent.Metadata, spec.Project, spec.Role) {
			matches = append(matches, *agent)
		}
	})
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

func (s *Service) createAgent(
	ctx context.Context,
	key string,
	hash string,
	spec *AgentSpec,
) (AgentHandle, error) {
	created, err := s.ma.New(ctx, toAgentCreateParams(spec, key))
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

func (s *Service) adoptAgent(
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

	updated, err := s.ma.Update(ctx, agent.ID, toAgentUpdateParams(spec, key, agent.Version))
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

func (s *Service) listOrchestraAgents(
	ctx context.Context,
	includeArchived bool,
	visit func(*anthropic.BetaManagedAgentsAgent),
) error {
	params := anthropic.BetaAgentListParams{
		Limit: anthropic.Int(int64(s.cfg.ListPageLimit)),
	}
	if includeArchived {
		params.IncludeArchived = anthropic.Bool(true)
	}
	for pageNum := 0; pageNum < s.cfg.MaxListPages; pageNum++ {
		page, err := s.ma.List(ctx, params)
		if err != nil {
			return err
		}
		for i := range page.Data {
			agent := &page.Data[i]
			if !includeArchived && isAgentArchived(agent) {
				continue
			}
			visit(agent)
		}
		if page.NextPage == "" {
			return nil
		}
		params.Page = param.NewOpt(page.NextPage)
	}
	s.logger.Info("agent scan reached page ceiling", "max_pages", s.cfg.MaxListPages)
	return nil
}

func (s *Service) putAgentRecord(
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

func (s *Service) now() time.Time {
	return s.clock().UTC()
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

// AgentCacheKeyFromMetadata reconstructs a cache key from orchestra metadata.
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

func normalizeAgentSpec(spec *AgentSpec) {
	spec.Project = firstNonEmpty(spec.Project, spec.Metadata[orchestraMetadataProject])
	spec.Role = firstNonEmpty(spec.Role, spec.Metadata[orchestraMetadataRole])
}

func taggedAgent(metadata map[string]string, project, role string) bool {
	return metadata[orchestraMetadataProject] == project &&
		metadata[orchestraMetadataRole] == role &&
		metadata[orchestraMetadataVersion] == orchestraVersionV2
}

func isAgentArchived(agent *anthropic.BetaManagedAgentsAgent) bool {
	return agent != nil && !agent.ArchivedAt.IsZero()
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

func agentIDs(agents []anthropic.BetaManagedAgentsAgent) []string {
	out := make([]string, 0, len(agents))
	for i := range agents {
		out = append(out, agents[i].ID)
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

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// IsAPIStatus reports whether err is an Anthropic API error carrying code.
func IsAPIStatus(err error, code int) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}
