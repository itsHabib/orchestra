package spawner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/itsHabib/orchestra/pkg/store"
)

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

func envCacheKey(spec *EnvSpec) (string, error) {
	if spec.Project == "" || spec.Name == "" {
		return "", fmt.Errorf("%w: environment spec requires project and name", store.ErrInvalidArgument)
	}
	if strings.Contains(spec.Project, cacheKeySeparator) || strings.Contains(spec.Name, cacheKeySeparator) {
		return "", fmt.Errorf("%w: environment project/name must not contain %q", store.ErrInvalidArgument, cacheKeySeparator)
	}
	return spec.Project + cacheKeySeparator + spec.Name, nil
}

func normalizeEnvSpec(spec *EnvSpec) {
	spec.Project = firstNonEmpty(spec.Project, spec.Metadata[orchestraMetadataProject])
}

func taggedEnv(metadata map[string]string, project, name string) bool {
	return metadata[orchestraMetadataProject] == project &&
		metadata[orchestraMetadataEnv] == name &&
		metadata[orchestraMetadataVersion] == orchestraVersionV2
}

func isEnvArchived(env *anthropic.BetaEnvironment) bool {
	return env != nil && env.ArchivedAt != ""
}

func handleFromMAEnv(env *anthropic.BetaEnvironment) EnvHandle {
	return EnvHandle{
		ID:       env.ID,
		Backend:  managedAgentsBackend,
		Name:     env.Name,
		Metadata: cloneStringMap(env.Metadata),
	}
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

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
