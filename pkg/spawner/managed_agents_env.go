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
