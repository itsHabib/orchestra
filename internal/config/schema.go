package config

import (
	"fmt"
	"strings"
)

// Config is the top-level orchestra configuration.
type Config struct {
	Name        string      `yaml:"name"`
	Defaults    Defaults    `yaml:"defaults"`
	Coordinator Coordinator `yaml:"coordinator,omitempty"`
	Teams       []Team      `yaml:"teams"`
}

// Coordinator configures the top-level coordinator agent.
type Coordinator struct {
	Enabled  bool   `yaml:"enabled"`
	Model    string `yaml:"model,omitempty"`
	MaxTurns int    `yaml:"max_turns,omitempty"`
}

// Defaults holds default values applied to all teams unless overridden.
type Defaults struct {
	Model          string `yaml:"model" json:"model"`
	MaxTurns       int    `yaml:"max_turns" json:"max_turns"`
	PermissionMode string `yaml:"permission_mode" json:"permission_mode"`
	TimeoutMinutes int    `yaml:"timeout_minutes" json:"timeout_minutes"`
}

// Team represents a single team or solo agent in the orchestration.
type Team struct {
	Name      string   `yaml:"name"`
	Lead      Lead     `yaml:"lead"`
	Members   []Member `yaml:"members"`
	Tasks     []Task   `yaml:"tasks"`
	DependsOn []string `yaml:"depends_on"`
	Context   string   `yaml:"context"`
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
}

// Warning represents a non-fatal validation issue.
type Warning struct {
	Team    string
	Message string
}

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
	var errs []string

	if c.Name == "" {
		errs = append(errs, "project name is required")
	}
	if len(c.Teams) == 0 {
		errs = append(errs, "at least one team is required")
	}

	// Check unique team names
	seen := make(map[string]bool)
	for _, t := range c.Teams {
		if t.Name == "" {
			errs = append(errs, "team name cannot be empty")
			continue
		}
		if seen[t.Name] {
			errs = append(errs, fmt.Sprintf("duplicate team name: %q", t.Name))
		}
		seen[t.Name] = true
	}

	// Check each team
	for _, t := range c.Teams {
		if t.Name == "" {
			continue
		}
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

		// Check depends_on references
		for _, dep := range t.DependsOn {
			if dep == t.Name {
				errs = append(errs, fmt.Sprintf("team %q: cannot depend on itself", t.Name))
			}
			if !seen[dep] {
				errs = append(errs, fmt.Sprintf("team %q: depends on unknown team %q", t.Name, dep))
			}
		}

		// Team size warning
		if len(t.Members) > 5 {
			warnings = append(warnings, Warning{
				Team:    t.Name,
				Message: fmt.Sprintf("has %d members (recommended: 3-5); consider splitting into smaller teams", len(t.Members)),
			})
		}

		// Task ratio warning
		divisor := len(t.Members)
		if divisor == 0 {
			divisor = 1
		}
		ratio := len(t.Tasks) / divisor
		if len(t.Tasks) > 0 && (ratio < 2 || ratio > 8) {
			warnings = append(warnings, Warning{
				Team:    t.Name,
				Message: fmt.Sprintf("task/member ratio is %d (recommended: 2-8)", ratio),
			})
		}
	}

	// Check for cycles using DFS
	if err := detectCycles(c.Teams); err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return warnings, fmt.Errorf("validation errors:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return warnings, nil
}

func detectCycles(teams []Team) error {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int)
	for _, t := range teams {
		color[t.Name] = white
	}

	deps := make(map[string][]string)
	for _, t := range teams {
		deps[t.Name] = t.DependsOn
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

	for _, t := range teams {
		if color[t.Name] == white {
			if err := visit(t.Name); err != nil {
				return err
			}
		}
	}
	return nil
}
