package spawner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/itsHabib/orchestra/pkg/store"
)

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
