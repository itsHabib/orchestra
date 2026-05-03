package config

import (
	"fmt"
	"strconv"

	"github.com/itsHabib/orchestra/internal/ghhost"
)

// DefaultRepoMountPath is the working-copy mount applied when
// RepositorySpec.MountPath is empty.
const DefaultRepoMountPath = "/workspace/repo"

// DefaultRepoDefaultBranch is the assumed default branch when
// RepositorySpec.DefaultBranch is empty (design Q2 lean).
const DefaultRepoDefaultBranch = "main"

// EffectiveRepository returns the RepositorySpec that applies to a — the
// per-agent override if set, else the project-level managed-agents repository,
// else nil. Callers must treat the returned pointer as read-only.
func (a *Agent) EffectiveRepository(cfg *Config) *RepositorySpec {
	if a.EnvironmentOverride.Repository != nil {
		return a.EnvironmentOverride.Repository
	}
	if cfg == nil || cfg.Backend.ManagedAgents == nil {
		return nil
	}
	return cfg.Backend.ManagedAgents.Repository
}

func (s *RepositorySpec) resolveDefaults() {
	if s == nil {
		return
	}
	if s.MountPath == "" {
		s.MountPath = DefaultRepoMountPath
	}
	if s.DefaultBranch == "" {
		s.DefaultBranch = DefaultRepoDefaultBranch
	}
}

// resolveRepositoryDefaults walks the project-level and per-agent repository
// specs and fills in MountPath / DefaultBranch defaults.
func (c *Config) resolveRepositoryDefaults() {
	if c.Backend.ManagedAgents != nil {
		c.Backend.ManagedAgents.Repository.resolveDefaults()
	}
	for i := range c.Agents {
		c.Agents[i].EnvironmentOverride.Repository.resolveDefaults()
	}
}

// validateRepositoryHard returns hard errors for repository configuration.
// Only consulted when Backend.Kind == "managed_agents".
func (c *Config) validateRepositoryHard() []ConfigError {
	if c.Backend.Kind != "managed_agents" {
		return nil
	}
	var errs []ConfigError

	if c.Backend.ManagedAgents != nil && c.Backend.ManagedAgents.Repository != nil {
		basePath := "backend.managed_agents.repository"
		baseField := []string{"backend", "managed_agents", "repository"}
		errs = append(errs, validateRepositorySpec(basePath, baseField, "", c.Backend.ManagedAgents.Repository)...)
	}
	for i := range c.Agents {
		spec := c.Agents[i].EnvironmentOverride.Repository
		if spec == nil {
			continue
		}
		basePath := fmt.Sprintf("agents[%q].environment_override.repository", c.Agents[i].Name)
		baseField := []string{"agents", strconv.Itoa(i), "environment_override", "repository"}
		errs = append(errs, validateRepositorySpec(basePath, baseField, c.Agents[i].Name, spec)...)
	}

	if c.Backend.ManagedAgents != nil && c.Backend.ManagedAgents.OpenPullRequests {
		for i := range c.Agents {
			if c.Agents[i].EffectiveRepository(c) == nil {
				errs = append(errs, ConfigError{
					Field:   []string{"backend", "managed_agents", "open_pull_requests"},
					Agent:   c.Agents[i].Name,
					Message: "backend.managed_agents.open_pull_requests requires a repository (set backend.managed_agents.repository or environment_override.repository)",
				})
			}
		}
	}
	return errs
}

func validateRepositorySpec(basePath string, baseField []string, agent string, spec *RepositorySpec) []ConfigError {
	if spec.URL == "" {
		return []ConfigError{{
			Field:   append(append([]string{}, baseField...), "url"),
			Agent:   agent,
			Message: basePath + ".url is required",
		}}
	}
	if _, _, err := ghhost.ParseRepoURL(spec.URL); err != nil {
		return []ConfigError{{
			Field:   append(append([]string{}, baseField...), "url"),
			Agent:   agent,
			Message: fmt.Sprintf("%s.url %q invalid: %v", basePath, spec.URL, err),
		}}
	}
	return nil
}

// validateRepositoryWarnings emits a warning for each agent whose
// EnvironmentOverride.Repository points at a different repo than one of its
// upstreams. Cross-repo dependency wiring is out of scope for P1.5; the
// upstream's branch will not be mounted under the downstream session.
func (c *Config) validateRepositoryWarnings() []Warning {
	if c.Backend.Kind != "managed_agents" {
		return nil
	}
	agentByName := make(map[string]*Agent, len(c.Agents))
	for i := range c.Agents {
		agentByName[c.Agents[i].Name] = &c.Agents[i]
	}
	var warnings []Warning
	for i := range c.Agents {
		a := &c.Agents[i]
		myRepo := repoURL(a.EffectiveRepository(c))
		for _, dep := range a.DependsOn {
			up, ok := agentByName[dep]
			if !ok {
				continue
			}
			depRepo := repoURL(up.EffectiveRepository(c))
			if myRepo == "" || depRepo == "" || myRepo == depRepo {
				continue
			}
			warnings = append(warnings, Warning{
				Field:   []string{"agents", strconv.Itoa(i), "depends_on"},
				Agent:   a.Name,
				Message: fmt.Sprintf("depends on %q which uses a different repository (%q vs %q); cross-repo upstream branches are not mounted", dep, depRepo, myRepo),
			})
		}
	}
	return warnings
}

func repoURL(spec *RepositorySpec) string {
	if spec == nil {
		return ""
	}
	return spec.URL
}
