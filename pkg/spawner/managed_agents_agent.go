package spawner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/itsHabib/orchestra/pkg/store"
)

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
// updates it in place when the spec drifted, or returns a nil handle to
// signal the caller should reconcile.
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
