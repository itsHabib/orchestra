package config

import (
	"fmt"

	"github.com/itsHabib/orchestra/internal/ghhost"
)

// DefaultRepoMountPath is the working-copy mount applied when
// RepositorySpec.MountPath is empty.
const DefaultRepoMountPath = "/workspace/repo"

// DefaultRepoDefaultBranch is the assumed default branch when
// RepositorySpec.DefaultBranch is empty (design Q2 lean).
const DefaultRepoDefaultBranch = "main"

// EffectiveRepository returns the RepositorySpec that applies to t — the
// per-team override if set, else the project-level managed-agents repository,
// else nil. Callers must treat the returned pointer as read-only.
func (t *Team) EffectiveRepository(cfg *Config) *RepositorySpec {
	if t.EnvironmentOverride.Repository != nil {
		return t.EnvironmentOverride.Repository
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

// resolveRepositoryDefaults walks the project-level and per-team repository
// specs and fills in MountPath / DefaultBranch defaults.
func (c *Config) resolveRepositoryDefaults() {
	if c.Backend.ManagedAgents != nil {
		c.Backend.ManagedAgents.Repository.resolveDefaults()
	}
	for i := range c.Teams {
		c.Teams[i].EnvironmentOverride.Repository.resolveDefaults()
	}
}

// validateRepositoryHard returns hard errors for repository configuration.
// Only consulted when Backend.Kind == "managed_agents".
func (c *Config) validateRepositoryHard() []string {
	if c.Backend.Kind != "managed_agents" {
		return nil
	}
	var errs []string

	if c.Backend.ManagedAgents != nil && c.Backend.ManagedAgents.Repository != nil {
		errs = append(errs, validateRepositorySpec("backend.managed_agents.repository", c.Backend.ManagedAgents.Repository)...)
	}
	for i := range c.Teams {
		spec := c.Teams[i].EnvironmentOverride.Repository
		if spec == nil {
			continue
		}
		path := fmt.Sprintf("teams[%q].environment_override.repository", c.Teams[i].Name)
		errs = append(errs, validateRepositorySpec(path, spec)...)
	}

	if c.Backend.ManagedAgents != nil && c.Backend.ManagedAgents.OpenPullRequests {
		for i := range c.Teams {
			if c.Teams[i].EffectiveRepository(c) == nil {
				errs = append(errs, fmt.Sprintf("team %q: backend.managed_agents.open_pull_requests requires a repository (set backend.managed_agents.repository or environment_override.repository)", c.Teams[i].Name))
			}
		}
	}
	return errs
}

func validateRepositorySpec(path string, spec *RepositorySpec) []string {
	if spec.URL == "" {
		return []string{path + ".url is required"}
	}
	if _, _, err := ghhost.ParseRepoURL(spec.URL); err != nil {
		return []string{fmt.Sprintf("%s.url %q invalid: %v", path, spec.URL, err)}
	}
	return nil
}

// validateRepositoryWarnings emits a warning for each team whose
// EnvironmentOverride.Repository points at a different repo than one of its
// upstreams. Cross-repo dependency wiring is out of scope for P1.5; the
// upstream's branch will not be mounted under the downstream session.
func (c *Config) validateRepositoryWarnings() []Warning {
	if c.Backend.Kind != "managed_agents" {
		return nil
	}
	teamByName := make(map[string]*Team, len(c.Teams))
	for i := range c.Teams {
		teamByName[c.Teams[i].Name] = &c.Teams[i]
	}
	var warnings []Warning
	for i := range c.Teams {
		t := &c.Teams[i]
		myRepo := repoURL(t.EffectiveRepository(c))
		for _, dep := range t.DependsOn {
			up, ok := teamByName[dep]
			if !ok {
				continue
			}
			depRepo := repoURL(up.EffectiveRepository(c))
			if myRepo == "" || depRepo == "" || myRepo == depRepo {
				continue
			}
			warnings = append(warnings, Warning{
				Team:    t.Name,
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
