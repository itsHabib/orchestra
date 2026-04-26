package config

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level orchestra configuration.
type Config struct {
	Name        string      `yaml:"name"`
	Backend     Backend     `yaml:"backend,omitempty"`
	Defaults    Defaults    `yaml:"defaults"`
	Coordinator Coordinator `yaml:"coordinator,omitempty"`
	Teams       []Team      `yaml:"teams"`
}

// Backend selects the runtime backend. It accepts either:
//
//	backend: local
//
// or:
//
//	backend:
//	  kind: managed_agents
//	  managed_agents:
//	    repository: { url: "https://github.com/owner/repo" }
//	    open_pull_requests: true
type Backend struct {
	Kind          string                `yaml:"kind,omitempty" json:"kind,omitempty"`
	ManagedAgents *ManagedAgentsBackend `yaml:"managed_agents,omitempty" json:"managed_agents,omitempty"`
}

// ManagedAgentsBackend captures managed-agents-specific backend settings. Only
// consulted when Backend.Kind is "managed_agents".
type ManagedAgentsBackend struct {
	Repository       *RepositorySpec `yaml:"repository,omitempty" json:"repository,omitempty"`
	OpenPullRequests bool            `yaml:"open_pull_requests,omitempty" json:"open_pull_requests,omitempty"`
}

// RepositorySpec describes a GitHub repository attached to managed-agents
// sessions. URL is the canonical https URL; MountPath defaults to
// "/workspace/repo"; DefaultBranch defaults to "main".
type RepositorySpec struct {
	URL           string `yaml:"url" json:"url"`
	MountPath     string `yaml:"mount_path,omitempty" json:"mount_path,omitempty"`
	DefaultBranch string `yaml:"default_branch,omitempty" json:"default_branch,omitempty"`
}

// EnvironmentOverride lets a single team substitute backend-level
// environment fields (currently just Repository) without touching others.
type EnvironmentOverride struct {
	Repository *RepositorySpec `yaml:"repository,omitempty" json:"repository,omitempty"`
}

// UnmarshalYAML accepts the older scalar backend spelling and the newer
// object spelling used by the managed-agents design docs.
func (b *Backend) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		b.Kind = strings.TrimSpace(value.Value)
		return nil
	case yaml.MappingNode:
		type rawBackend Backend
		var raw rawBackend
		if err := value.Decode(&raw); err != nil {
			return err
		}
		*b = Backend(raw)
		return nil
	default:
		return errors.New("backend must be a string or mapping")
	}
}

// Coordinator configures the top-level coordinator agent.
type Coordinator struct {
	Enabled  bool   `yaml:"enabled"`
	Model    string `yaml:"model,omitempty"`
	MaxTurns int    `yaml:"max_turns,omitempty"`
}

// Defaults holds default values applied to all teams unless overridden.
type Defaults struct {
	Model                string `yaml:"model" json:"model"`
	MaxTurns             int    `yaml:"max_turns" json:"max_turns"`
	PermissionMode       string `yaml:"permission_mode" json:"permission_mode"`
	TimeoutMinutes       int    `yaml:"timeout_minutes" json:"timeout_minutes"`
	InboxPollInterval    string `yaml:"inbox_poll_interval" json:"inbox_poll_interval"`
	MAConcurrentSessions int    `yaml:"ma_concurrent_sessions,omitempty" json:"ma_concurrent_sessions,omitempty"`
}

// DefaultMAConcurrentSessions caps how many managed-agents StartSession calls
// can be in flight at once. Bounds the create rate against MA's 60/min org
// limit. Override via Defaults.MAConcurrentSessions.
const DefaultMAConcurrentSessions = 20

// Team represents a single team or solo agent in the orchestration.
type Team struct {
	Name                string              `yaml:"name"`
	Lead                Lead                `yaml:"lead"`
	Members             []Member            `yaml:"members"`
	Tasks               []Task              `yaml:"tasks"`
	DependsOn           []string            `yaml:"depends_on"`
	Context             string              `yaml:"context"`
	EnvironmentOverride EnvironmentOverride `yaml:"environment_override,omitempty"`
}

// Task represents a unit of work assigned to a team.
type Task struct {
	Summary      string   `yaml:"summary" json:"summary"`
	Details      string   `yaml:"details" json:"details,omitempty"`
	Deliverables []string `yaml:"deliverables" json:"deliverables,omitempty"`
	Verify       string   `yaml:"verify" json:"verify,omitempty"`
}

// Lead represents the team lead configuration.
type Lead struct {
	Role  string `yaml:"role" json:"role"`
	Model string `yaml:"model" json:"model,omitempty"`
}

// Member represents a team member.
type Member struct {
	Role  string `yaml:"role" json:"role"`
	Focus string `yaml:"focus" json:"focus"`
}

// HasMembers returns true if the team has members (is a real team, not solo).
func (t *Team) HasMembers() bool {
	return len(t.Members) > 0
}

// TeamByName returns a pointer to the team with the given name, or nil.
func (c *Config) TeamByName(name string) *Team {
	for i := range c.Teams {
		if c.Teams[i].Name == name {
			return &c.Teams[i]
		}
	}
	return nil
}

// ResolveDefaults fills zero-value fields with defaults.
func (c *Config) ResolveDefaults() {
	if c.Backend.Kind == "" {
		c.Backend.Kind = "local"
	}
	if c.Defaults.Model == "" {
		c.Defaults.Model = "sonnet"
	}
	if c.Defaults.MaxTurns == 0 {
		c.Defaults.MaxTurns = 200
	}
	if c.Defaults.PermissionMode == "" {
		c.Defaults.PermissionMode = "acceptEdits"
	}
	if c.Defaults.TimeoutMinutes == 0 {
		c.Defaults.TimeoutMinutes = 30
	}
	if c.Defaults.InboxPollInterval == "" {
		c.Defaults.InboxPollInterval = "5m"
	}
	if c.Defaults.MAConcurrentSessions == 0 {
		c.Defaults.MAConcurrentSessions = DefaultMAConcurrentSessions
	}
	for i := range c.Teams {
		if c.Teams[i].Lead.Model == "" {
			c.Teams[i].Lead.Model = c.Defaults.Model
		}
	}
	// Coordinator defaults
	if c.Coordinator.Model == "" {
		c.Coordinator.Model = c.Defaults.Model
	}
	if c.Coordinator.MaxTurns == 0 {
		c.Coordinator.MaxTurns = 500
	}
	c.resolveRepositoryDefaults()
}

// Warning represents a non-fatal validation issue surfaced by [Load]
// or [Config.Validate]. It does not block execution.
//
// pkg/orchestra re-exports this type as orchestra.Warning.
type Warning struct {
	// Field is the structured YAML path to the offending node, e.g.
	// {"teams", "0", "tasks", "2", "verify"} for a missing verify on
	// team 0's third task. Empty for project-level issues.
	Field []string
	// Team is the denormalized team name when Field points into a team
	// subtree; empty otherwise. Exists for ergonomic display so
	// String() can render `team "foo": message` without walking Field
	// back into Config. Programmatic consumers should prefer Field.
	Team string
	// Message is the human-readable description of the issue.
	Message string
}

// String returns the human-readable form: `team "foo": message` when
// Team is non-empty, else just Message.
func (w Warning) String() string {
	if w.Team != "" {
		return fmt.Sprintf("team %q: %s", w.Team, w.Message)
	}
	return w.Message
}

// Validate checks the config for errors and returns warnings.
// Hard errors prevent execution. Warnings are returned separately.
func (c *Config) Validate() ([]Warning, error) {
	var warnings []Warning
	errs := c.validateTopLevel()

	nameValidation := c.validateTeamNames()
	errs = append(errs, nameValidation.errs...)

	teamValidation := c.validateTeams(nameValidation.seen)
	warnings = append(warnings, teamValidation.warnings...)
	errs = append(errs, teamValidation.errs...)
	warnings = append(warnings, c.validateBackendWarnings()...)
	warnings = append(warnings, c.validateRepositoryWarnings()...)
	errs = append(errs, c.validateRepositoryHard()...)

	// Check for cycles using DFS
	if err := detectCycles(c.Teams); err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return warnings, fmt.Errorf("validation errors:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return warnings, nil
}

func (c *Config) validateTopLevel() []string {
	var errs []string
	if c.Name == "" {
		errs = append(errs, "project name is required")
	}
	backend := c.Backend.Kind
	if backend == "" {
		backend = "local"
	}
	switch backend {
	case "local", "managed_agents":
	default:
		errs = append(errs, fmt.Sprintf("backend.kind must be one of: local, managed_agents (got %q)", c.Backend.Kind))
	}
	if len(c.Teams) == 0 {
		errs = append(errs, "at least one team is required")
	}
	if c.Defaults.MAConcurrentSessions < 0 {
		errs = append(errs, fmt.Sprintf("defaults.ma_concurrent_sessions must be >= 0 (got %d)", c.Defaults.MAConcurrentSessions))
	}
	return errs
}

func (c *Config) validateBackendWarnings() []Warning {
	if c.Backend.Kind != "managed_agents" {
		return nil
	}
	var warnings []Warning
	if c.Coordinator.Enabled {
		warnings = append(warnings, Warning{Message: "coordinator is not supported under backend.kind=managed_agents"})
	}
	for i := range c.Teams {
		if c.Teams[i].HasMembers() {
			warnings = append(warnings, Warning{
				Team:    c.Teams[i].Name,
				Message: "members are not supported under backend.kind=managed_agents",
			})
		}
	}
	return warnings
}

type teamNameValidation struct {
	seen map[string]bool
	errs []string
}

type validationResult struct {
	warnings []Warning
	errs     []string
}

func (c *Config) validateTeamNames() teamNameValidation {
	seen := make(map[string]bool)
	var errs []string
	for i := range c.Teams {
		t := &c.Teams[i]
		if t.Name == "" {
			errs = append(errs, "team name cannot be empty")
			continue
		}
		if seen[t.Name] {
			errs = append(errs, fmt.Sprintf("duplicate team name: %q", t.Name))
		}
		seen[t.Name] = true
	}
	return teamNameValidation{seen: seen, errs: errs}
}

func (c *Config) validateTeams(seen map[string]bool) validationResult {
	var warnings []Warning
	var errs []string
	for i := range c.Teams {
		t := &c.Teams[i]
		if t.Name == "" {
			continue
		}
		taskValidation := validateTasks(t)
		warnings = append(warnings, taskValidation.warnings...)
		errs = append(errs, taskValidation.errs...)
		errs = append(errs, validateDependencies(t, seen)...)
		warnings = append(warnings, validateTeamSize(t)...)
		warnings = append(warnings, validateTaskRatio(t)...)
	}
	return validationResult{warnings: warnings, errs: errs}
}

func validateTasks(t *Team) validationResult {
	var warnings []Warning
	var errs []string
	if len(t.Tasks) == 0 {
		errs = append(errs, fmt.Sprintf("team %q: at least one task is required", t.Name))
	}
	for i, task := range t.Tasks {
		if task.Summary == "" {
			errs = append(errs, fmt.Sprintf("team %q: task %d has empty summary", t.Name, i+1))
		}
		if task.Details == "" {
			warnings = append(warnings, Warning{Team: t.Name, Message: fmt.Sprintf("task %d (%q) has empty details", i+1, task.Summary)})
		}
		if task.Verify == "" {
			warnings = append(warnings, Warning{Team: t.Name, Message: fmt.Sprintf("task %d (%q) has empty verify command", i+1, task.Summary)})
		}
	}
	return validationResult{warnings: warnings, errs: errs}
}

func validateDependencies(t *Team, seen map[string]bool) []string {
	var errs []string
	for _, dep := range t.DependsOn {
		if dep == t.Name {
			errs = append(errs, fmt.Sprintf("team %q: cannot depend on itself", t.Name))
		}
		if !seen[dep] {
			errs = append(errs, fmt.Sprintf("team %q: depends on unknown team %q", t.Name, dep))
		}
	}
	return errs
}

func validateTeamSize(t *Team) []Warning {
	if len(t.Members) <= 5 {
		return nil
	}
	return []Warning{{
		Team:    t.Name,
		Message: fmt.Sprintf("has %d members (recommended: 3-5); consider splitting into smaller teams", len(t.Members)),
	}}
}

func validateTaskRatio(t *Team) []Warning {
	divisor := len(t.Members)
	if divisor == 0 {
		divisor = 1
	}
	ratio := len(t.Tasks) / divisor
	if len(t.Tasks) == 0 || (ratio >= 2 && ratio <= 8) {
		return nil
	}
	return []Warning{{
		Team:    t.Name,
		Message: fmt.Sprintf("task/member ratio is %d (recommended: 2-8)", ratio),
	}}
}

func detectCycles(teams []Team) error {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int)
	for i := range teams {
		color[teams[i].Name] = white
	}

	deps := make(map[string][]string)
	for i := range teams {
		deps[teams[i].Name] = teams[i].DependsOn
	}

	var visit func(name string) error
	visit = func(name string) error {
		color[name] = gray
		for _, dep := range deps[name] {
			if color[dep] == gray {
				return fmt.Errorf("dependency cycle detected involving team %q", dep)
			}
			if color[dep] == white {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[name] = black
		return nil
	}

	for i := range teams {
		if color[teams[i].Name] == white {
			if err := visit(teams[i].Name); err != nil {
				return err
			}
		}
	}
	return nil
}
