package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level orchestra configuration.
type Config struct {
	Name        string      `yaml:"name"`
	Backend     Backend     `yaml:"backend,omitempty"`
	Defaults    Defaults    `yaml:"defaults"`
	Coordinator Coordinator `yaml:"coordinator,omitempty"`
	Agents      []Agent     `yaml:"agents"`

	// LegacyTeamsKey is set true by [Config.UnmarshalYAML] when the YAML
	// document used the deprecated `teams:` key instead of `agents:`. The
	// validator surfaces a one-shot deprecation warning when set so authors
	// learn the new spelling without breaking existing files. It is not a
	// YAML field itself.
	LegacyTeamsKey bool `yaml:"-" json:"-"`
}

// UnmarshalYAML accepts both the v3 spelling (`agents:`) and the v2
// spelling (`teams:`) for the agent list. Setting both keys at the same
// time is rejected — even when one of them is empty — so an accidental
// dual-key config during migration fails fast with a clear precedence
// error instead of silently treating the empty side as authoritative.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	keys := topLevelKeysPresent(value)
	hasAgents, hasTeams := keys.Agents, keys.Teams
	type rawConfig struct {
		Name        string      `yaml:"name"`
		Backend     Backend     `yaml:"backend,omitempty"`
		Defaults    Defaults    `yaml:"defaults"`
		Coordinator Coordinator `yaml:"coordinator,omitempty"`
		Agents      []Agent     `yaml:"agents"`
		Teams       []Agent     `yaml:"teams"`
	}
	var raw rawConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}
	if hasAgents && hasTeams {
		return errors.New("config: cannot set both `agents:` and `teams:` — use `agents:` (the v3 spelling)")
	}
	c.Name = raw.Name
	c.Backend = raw.Backend
	c.Defaults = raw.Defaults
	c.Coordinator = raw.Coordinator
	c.Agents = raw.Agents
	c.LegacyTeamsKey = false
	if hasTeams {
		c.Agents = raw.Teams
		c.LegacyTeamsKey = true
	}
	return nil
}

// agentListKeyPresence captures which top-level slice keys
// ([Config.UnmarshalYAML]) saw in the YAML document. Used by the dual-key
// guard to distinguish "missing key" from "empty list" — both decode to a
// zero-length slice, but only the former is allowed alongside its alias.
type agentListKeyPresence struct {
	Agents bool
	Teams  bool
}

// topLevelKeysPresent inspects the parsed YAML node and reports whether
// the document explicitly set `agents:` and `teams:`.
func topLevelKeysPresent(value *yaml.Node) agentListKeyPresence {
	var p agentListKeyPresence
	node := value
	if node != nil && node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node == nil || node.Kind != yaml.MappingNode {
		return p
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		switch key.Value {
		case "agents":
			p.Agents = true
		case "teams":
			p.Teams = true
		}
	}
	return p
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

// EnvironmentOverride lets a single agent substitute backend-level
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

// Defaults holds default values applied to all agents unless overridden.
type Defaults struct {
	Model                string `yaml:"model" json:"model"`
	MaxTurns             int    `yaml:"max_turns" json:"max_turns"`
	PermissionMode       string `yaml:"permission_mode" json:"permission_mode"`
	TimeoutMinutes       int    `yaml:"timeout_minutes" json:"timeout_minutes"`
	InboxPollInterval    string `yaml:"inbox_poll_interval" json:"inbox_poll_interval"`
	MAConcurrentSessions int    `yaml:"ma_concurrent_sessions,omitempty" json:"ma_concurrent_sessions,omitempty"`

	// RequiresCredentials lists the credential names every agent in this
	// run needs. Per-agent [Agent.RequiresCredentials] extends this list;
	// the engine resolves the union via internal/credentials at run start,
	// failing fast on any missing name.
	//
	// Backend coverage:
	//   - backend.kind=local: resolved values reach `claude -p` via cmd.Env.
	//     Verified end-to-end by internal/spawner.TestSpawn_EnvOverlayReachesChild.
	//   - backend.kind=managed_agents: the engine resolves the names but
	//     emits a one-shot warning at run start — the Anthropic Managed
	//     Agents SDK does not yet expose per-session env injection
	//     (BetaSessionNewParams has no Env field; Vault credentials only
	//     support mcp_oauth/static_bearer, not generic env vars). For
	//     GitHub specifically, the github_repository ResourceRef path
	//     (host PAT → AuthorizationToken on the resource) works
	//     end-to-end and is the recommended substitute. Other secrets
	//     are unreachable on MA today.
	//
	// Tracking: github.com/itsHabib/orchestra/issues/42 — closes when the
	// SDK gap closes. See docs/feedback-phase-a-dogfood.md §B2.
	RequiresCredentials []string `yaml:"requires_credentials,omitempty" json:"requires_credentials,omitempty"`
}

// DefaultMAConcurrentSessions caps how many managed-agents StartSession calls
// can be in flight at once. Bounds the create rate against MA's 60/min org
// limit. Override via Defaults.MAConcurrentSessions.
const DefaultMAConcurrentSessions = 20

// Agent represents one node in the orchestration DAG — a solo agent or a
// multi-member team led by [Lead]. Renamed from Team in v3.
type Agent struct {
	Name                string              `yaml:"name"`
	Lead                Lead                `yaml:"lead"`
	Members             []Member            `yaml:"members"`
	Tasks               []Task              `yaml:"tasks"`
	DependsOn           []string            `yaml:"depends_on"`
	Context             string              `yaml:"context"`
	EnvironmentOverride EnvironmentOverride `yaml:"environment_override,omitempty"`

	// Skills are attached to the agent's MA agent at agent-creation time.
	// Each entry is resolved against the orchestra skills cache
	// (~/.config/orchestra/skills.json) to find a registered skill_id.
	// Local backend ignores this field with a warning.
	Skills []SkillRef `yaml:"skills,omitempty" json:"skills,omitempty"`

	// CustomTools are host-side tools the agent's MA agent can invoke. Each
	// entry is resolved against the customtools registry; the engine wires
	// the agent's `agent.custom_tool_use` events through to the registered
	// handler. Local backend ignores this field with a warning.
	CustomTools []CustomToolRef `yaml:"custom_tools,omitempty" json:"custom_tools,omitempty"`

	// RequiresCredentials lists credential names this agent needs at run
	// start. Names resolve to environment variables on the agent's session
	// via internal/credentials. Combined with [Defaults.RequiresCredentials]
	// at run time — the agent sees the union of both lists.
	RequiresCredentials []string `yaml:"requires_credentials,omitempty" json:"requires_credentials,omitempty"`

	// Files declares host-side files to upload via the Anthropic Files API
	// and mount read-only inside the agent's MA container. Each entry's
	// Path is read at run time; the upload result is plumbed into
	// [spawner.ResourceRef]{Type:"file"} so the file lands at the
	// configured MountPath. Local backend ignores this field with a
	// warning. Phase A scope: managed_agents only.
	Files []FileMount `yaml:"files,omitempty" json:"files,omitempty"`
}

// FileMount declares one file an agent needs mounted in its MA container.
// Path is the host-side filesystem path (relative paths resolve from the
// orchestra.yaml's directory at validation time). MountPath is the absolute
// container path; an empty value defaults to /workspace/<basename(Path)>.
type FileMount struct {
	Path      string `yaml:"path" json:"path"`
	MountPath string `yaml:"mount,omitempty" json:"mount,omitempty"`
}

// RequiredCredentials returns the union of [Defaults.RequiresCredentials]
// and [Agent.RequiresCredentials], deduplicated and sorted. Empty when
// neither is set.
func (a *Agent) RequiredCredentials(d *Defaults) []string {
	if a == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(names []string) {
		for _, name := range names {
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	if d != nil {
		add(d.RequiresCredentials)
	}
	add(a.RequiresCredentials)
	sort.Strings(out)
	return out
}

// SkillRef references a skill registered with Anthropic via the orchestra
// skills cache. Type defaults to "custom" (skills the user uploaded via
// `orchestra skills upload`); "anthropic" is reserved for first-party skills
// once the cache learns to track them. Version pins a specific version of the
// skill; empty means "latest cached".
type SkillRef struct {
	Name    string `yaml:"name" json:"name"`
	Type    string `yaml:"type,omitempty" json:"type,omitempty"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
}

// CustomToolRef references a host-side custom tool by the name registered in
// the customtools package. The tool's input schema and description live with
// the registered handler; the config only carries the lookup key.
type CustomToolRef struct {
	Name string `yaml:"name" json:"name"`
}

// Task represents a unit of work assigned to an agent.
type Task struct {
	Summary      string   `yaml:"summary" json:"summary"`
	Details      string   `yaml:"details" json:"details,omitempty"`
	Deliverables []string `yaml:"deliverables" json:"deliverables,omitempty"`
	Verify       string   `yaml:"verify" json:"verify,omitempty"`
}

// Lead represents the lead role for an agent. For solo agents the Lead is
// the sole role; for multi-member teams the lead delegates to Members.
type Lead struct {
	Role  string `yaml:"role" json:"role"`
	Model string `yaml:"model" json:"model,omitempty"`
}

// Member represents a sub-role inside a multi-member team agent.
type Member struct {
	Role  string `yaml:"role" json:"role"`
	Focus string `yaml:"focus" json:"focus"`
}

// HasMembers reports whether the agent is a multi-member team rather than
// a solo agent.
func (a *Agent) HasMembers() bool {
	return len(a.Members) > 0
}

// AgentByName returns a pointer to the agent with the given name, or nil.
func (c *Config) AgentByName(name string) *Agent {
	for i := range c.Agents {
		if c.Agents[i].Name == name {
			return &c.Agents[i]
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
	for i := range c.Agents {
		if c.Agents[i].Lead.Model == "" {
			c.Agents[i].Lead.Model = c.Defaults.Model
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
	// {"agents", "0", "tasks", "2", "verify"} for a missing verify on
	// agent 0's third task. Empty for project-level issues.
	Field []string
	// Agent is the denormalized agent name when Field points into an
	// agent subtree; empty otherwise. Exists for ergonomic display so
	// String() can render `agent "foo": message` without walking Field
	// back into Config. Programmatic consumers should prefer Field.
	Agent string
	// Message is the human-readable description of the issue.
	Message string
}

// String returns the human-readable form: `agent "foo": message` when
// Agent is non-empty, else just Message.
func (w Warning) String() string {
	if w.Agent != "" {
		return fmt.Sprintf("agent %q: %s", w.Agent, w.Message)
	}
	return w.Message
}

// Validate runs the config validators and returns a [Result] aggregating
// the parsed config (when valid), structured warnings, and structured
// errors. Use [Result.Valid] to gate further use of the config; use
// [Result.Err] for an error-shaped view of validation failures suitable
// for `if err != nil` patterns.
//
// Validate never returns nil. When at least one entry exists in
// Result.Errors, Result.Config is nil so consumers cannot accidentally
// hand an invalid config to downstream code.
//
// pkg/orchestra re-exports the Result shape as orchestra.ValidationResult.
func (c *Config) Validate() *Result {
	if c == nil {
		return &Result{
			Errors: []ConfigError{{Message: "nil config"}},
		}
	}

	var warnings []Warning
	if c.LegacyTeamsKey {
		warnings = append(warnings, Warning{
			Field:   []string{"teams"},
			Message: "`teams:` is deprecated in v3; rename to `agents:`",
		})
	}
	errs := c.validateTopLevel()

	nameValidation := c.validateAgentNames()
	errs = append(errs, nameValidation.errs...)

	agentValidation := c.validateAgents(nameValidation.seen)
	warnings = append(warnings, agentValidation.warnings...)
	errs = append(errs, agentValidation.errs...)
	warnings = append(warnings, c.validateBackendWarnings()...)
	warnings = append(warnings, c.validateRepositoryWarnings()...)
	errs = append(errs, c.validateRepositoryHard()...)

	resourceShape := c.validateResourceShape()
	warnings = append(warnings, resourceShape.warnings...)
	errs = append(errs, resourceShape.errs...)

	// Check for cycles using DFS
	if cycleErr := detectCycles(c.Agents); cycleErr != nil {
		errs = append(errs, *cycleErr)
	}

	res := &Result{
		Warnings: warnings,
		Errors:   errs,
	}
	if res.Valid() {
		res.Config = c
	}
	return res
}

func (c *Config) validateTopLevel() []ConfigError {
	var errs []ConfigError
	if c.Name == "" {
		// Project-level: empty Field. The doc's §5.3 convention is
		// that the missing project-name field is the canonical
		// "empty Field" example so consumers can distinguish
		// project-wide issues from nested ones without inspecting
		// Message text.
		errs = append(errs, ConfigError{
			Message: "project name is required",
		})
	}
	backend := c.Backend.Kind
	if backend == "" {
		backend = "local"
	}
	switch backend {
	case "local", "managed_agents":
	default:
		errs = append(errs, ConfigError{
			Field:   []string{"backend", "kind"},
			Message: fmt.Sprintf("backend.kind must be one of: local, managed_agents (got %q)", c.Backend.Kind),
		})
	}
	if len(c.Agents) == 0 {
		errs = append(errs, ConfigError{
			Field:   []string{"agents"},
			Message: "at least one agent is required",
		})
	}
	if c.Defaults.MAConcurrentSessions < 0 {
		errs = append(errs, ConfigError{
			Field:   []string{"defaults", "ma_concurrent_sessions"},
			Message: fmt.Sprintf("defaults.ma_concurrent_sessions must be >= 0 (got %d)", c.Defaults.MAConcurrentSessions),
		})
	}
	return errs
}

func (c *Config) validateBackendWarnings() []Warning {
	if c.Backend.Kind != "managed_agents" {
		return nil
	}
	var warnings []Warning
	if c.Coordinator.Enabled {
		warnings = append(warnings, Warning{
			Field:   []string{"coordinator", "enabled"},
			Message: "coordinator is not supported under backend.kind=managed_agents",
		})
	}
	for i := range c.Agents {
		if c.Agents[i].HasMembers() {
			warnings = append(warnings, Warning{
				Field:   []string{"agents", strconv.Itoa(i), "members"},
				Agent:   c.Agents[i].Name,
				Message: "members are not supported under backend.kind=managed_agents",
			})
		}
	}
	return warnings
}

type agentNameValidation struct {
	seen map[string]bool
	errs []ConfigError
}

type validationResult struct {
	warnings []Warning
	errs     []ConfigError
}

func (c *Config) validateAgentNames() agentNameValidation {
	seen := make(map[string]bool)
	var errs []ConfigError
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.Name == "" {
			errs = append(errs, ConfigError{
				Field:   []string{"agents", strconv.Itoa(i), "name"},
				Message: "agent name cannot be empty",
			})
			continue
		}
		if seen[a.Name] {
			errs = append(errs, ConfigError{
				Field:   []string{"agents", strconv.Itoa(i), "name"},
				Agent:   a.Name,
				Message: fmt.Sprintf("duplicate agent name: %q", a.Name),
			})
		}
		seen[a.Name] = true
	}
	return agentNameValidation{seen: seen, errs: errs}
}

func (c *Config) validateAgents(seen map[string]bool) validationResult {
	var warnings []Warning
	var errs []ConfigError
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.Name == "" {
			continue
		}
		taskValidation := validateTasks(a, i)
		warnings = append(warnings, taskValidation.warnings...)
		errs = append(errs, taskValidation.errs...)
		errs = append(errs, validateDependencies(a, i, seen)...)
		warnings = append(warnings, validateAgentSize(a, i)...)
		warnings = append(warnings, validateTaskRatio(a, i)...)
	}
	return validationResult{warnings: warnings, errs: errs}
}

func validateTasks(a *Agent, agentIdx int) validationResult {
	var warnings []Warning
	var errs []ConfigError
	agentFieldPrefix := []string{"agents", strconv.Itoa(agentIdx)}
	if len(a.Tasks) == 0 {
		errs = append(errs, ConfigError{
			Field:   append(append([]string{}, agentFieldPrefix...), "tasks"),
			Agent:   a.Name,
			Message: "at least one task is required",
		})
	}
	for i, task := range a.Tasks {
		taskFieldPrefix := append(append([]string{}, agentFieldPrefix...), "tasks", strconv.Itoa(i))
		if task.Summary == "" {
			errs = append(errs, ConfigError{
				Field:   append(append([]string{}, taskFieldPrefix...), "summary"),
				Agent:   a.Name,
				Message: fmt.Sprintf("task %d has empty summary", i+1),
			})
		}
		if task.Details == "" {
			warnings = append(warnings, Warning{
				Field:   append(append([]string{}, taskFieldPrefix...), "details"),
				Agent:   a.Name,
				Message: fmt.Sprintf("task %d (%q) has empty details", i+1, task.Summary),
			})
		}
		if task.Verify == "" {
			warnings = append(warnings, Warning{
				Field:   append(append([]string{}, taskFieldPrefix...), "verify"),
				Agent:   a.Name,
				Message: fmt.Sprintf("task %d (%q) has empty verify command", i+1, task.Summary),
			})
		}
	}
	return validationResult{warnings: warnings, errs: errs}
}

func validateDependencies(a *Agent, agentIdx int, seen map[string]bool) []ConfigError {
	var errs []ConfigError
	field := []string{"agents", strconv.Itoa(agentIdx), "depends_on"}
	for _, dep := range a.DependsOn {
		if dep == a.Name {
			errs = append(errs, ConfigError{
				Field:   field,
				Agent:   a.Name,
				Message: "cannot depend on itself",
			})
		}
		if !seen[dep] {
			errs = append(errs, ConfigError{
				Field:   field,
				Agent:   a.Name,
				Message: fmt.Sprintf("depends on unknown agent %q", dep),
			})
		}
	}
	return errs
}

func validateAgentSize(a *Agent, agentIdx int) []Warning {
	if len(a.Members) <= 5 {
		return nil
	}
	return []Warning{{
		Field:   []string{"agents", strconv.Itoa(agentIdx), "members"},
		Agent:   a.Name,
		Message: fmt.Sprintf("has %d members (recommended: 3-5); consider splitting into smaller teams", len(a.Members)),
	}}
}

func validateTaskRatio(a *Agent, agentIdx int) []Warning {
	divisor := len(a.Members)
	if divisor == 0 {
		divisor = 1
	}
	ratio := len(a.Tasks) / divisor
	if len(a.Tasks) == 0 || (ratio >= 2 && ratio <= 8) {
		return nil
	}
	return []Warning{{
		Field:   []string{"agents", strconv.Itoa(agentIdx), "tasks"},
		Agent:   a.Name,
		Message: fmt.Sprintf("task/member ratio is %d (recommended: 2-8)", ratio),
	}}
}

func detectCycles(agents []Agent) *ConfigError {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int)
	for i := range agents {
		color[agents[i].Name] = white
	}

	deps := make(map[string][]string)
	for i := range agents {
		deps[agents[i].Name] = agents[i].DependsOn
	}

	var found *ConfigError
	var visit func(name string) bool
	visit = func(name string) bool {
		color[name] = gray
		for _, dep := range deps[name] {
			if color[dep] == gray {
				found = &ConfigError{
					Field:   []string{"agents"},
					Agent:   dep,
					Message: fmt.Sprintf("dependency cycle detected involving agent %q", dep),
				}
				return true
			}
			if color[dep] == white {
				if visit(dep) {
					return true
				}
			}
		}
		color[name] = black
		return false
	}

	for i := range agents {
		if color[agents[i].Name] == white {
			if visit(agents[i].Name) {
				return found
			}
		}
	}
	return nil
}

// validateResourceShape covers the per-agent Skills and CustomTools fields
// without consulting any external registry. Resolution-against-registries
// happens in [ValidateResourceReferences] because that needs the live skills
// cache and customtools registry passed in by the engine.
func (c *Config) validateResourceShape() validationResult {
	var out validationResult
	localBackend := c.Backend.Kind == "local" || c.Backend.Kind == ""
	for i := range c.Agents {
		a := &c.Agents[i]
		agentPrefix := []string{"agents", strconv.Itoa(i)}
		out.errs = append(out.errs, validateSkillRefs(a, agentPrefix)...)
		out.errs = append(out.errs, validateCustomToolRefs(a, agentPrefix)...)
		out.errs = append(out.errs, validateFileMounts(a, agentPrefix)...)
		if localBackend {
			out.warnings = append(out.warnings, localBackendResourceWarnings(a, agentPrefix)...)
		}
	}
	return out
}

func validateSkillRefs(a *Agent, agentPrefix []string) []ConfigError {
	var errs []ConfigError
	for j, sk := range a.Skills {
		fieldPrefix := append(append([]string{}, agentPrefix...), "skills", strconv.Itoa(j))
		if sk.Name == "" {
			errs = append(errs, ConfigError{
				Field:   append(append([]string{}, fieldPrefix...), "name"),
				Agent:   a.Name,
				Message: fmt.Sprintf("skill %d has empty name", j+1),
			})
		}
		switch sk.Type {
		case "", "custom":
			// "" defaults to "custom" downstream; both supported.
		case "anthropic":
			// The schema retains the field for forward-compat — the SDK's
			// skillParams already routes Metadata["type"] = "anthropic" to
			// BetaManagedAgentsAnthropicSkillParams — but the orchestra
			// skills cache only stores skill_ids returned by Beta.Skills.New
			// (custom skills the user uploaded). There's no resolution path
			// for an Anthropic first-party skill_id today, so accepting
			// type=anthropic here would surface as either a "skill not
			// registered" validator error or a 30-second-delayed MA API
			// rejection. Either is worse than failing here with a clear
			// message; remove this case once first-party skills are wired
			// in.
			errs = append(errs, ConfigError{
				Field:   append(append([]string{}, fieldPrefix...), "type"),
				Agent:   a.Name,
				Message: fmt.Sprintf("skill %q: type=anthropic is not yet supported (use type: custom with `orchestra skills upload`)", sk.Name),
			})
		default:
			// "anthropic" has its own case above (rejected with a more
			// specific message). Listing it here would mislead users
			// who typo the type into thinking the typo is the problem.
			errs = append(errs, ConfigError{
				Field:   append(append([]string{}, fieldPrefix...), "type"),
				Agent:   a.Name,
				Message: fmt.Sprintf("skill %q: type must be \"custom\" (got %q)", sk.Name, sk.Type),
			})
		}
	}
	return errs
}

func validateCustomToolRefs(a *Agent, agentPrefix []string) []ConfigError {
	var errs []ConfigError
	for j, ct := range a.CustomTools {
		fieldPrefix := append(append([]string{}, agentPrefix...), "custom_tools", strconv.Itoa(j))
		if ct.Name == "" {
			errs = append(errs, ConfigError{
				Field:   append(append([]string{}, fieldPrefix...), "name"),
				Agent:   a.Name,
				Message: fmt.Sprintf("custom_tool %d has empty name", j+1),
			})
		}
	}
	return errs
}

// validateFileMounts catches the obvious shape issues at `orchestra
// validate` time so a misconfigured `files:` entry surfaces alongside the
// equivalent skills/custom_tools checks rather than only at run time. The
// path-existence check still happens at upload time inside
// internal/files/files.go — validation here is purely structural.
func validateFileMounts(a *Agent, agentPrefix []string) []ConfigError {
	var errs []ConfigError
	for j, fm := range a.Files {
		fieldPrefix := append(append([]string{}, agentPrefix...), "files", strconv.Itoa(j))
		if fm.Path == "" {
			errs = append(errs, ConfigError{
				Field:   append(append([]string{}, fieldPrefix...), "path"),
				Agent:   a.Name,
				Message: fmt.Sprintf("file mount %d has empty path", j+1),
			})
		}
		// MountPath is the *container-side* path; the MA container is
		// Linux, so the absolute check is "starts with /", not
		// filepath.IsAbs (which on Windows requires a drive letter and
		// would reject /workspace/...).
		if fm.MountPath != "" && !strings.HasPrefix(fm.MountPath, "/") {
			errs = append(errs, ConfigError{
				Field:   append(append([]string{}, fieldPrefix...), "mount"),
				Agent:   a.Name,
				Message: fmt.Sprintf("file mount %d: mount %q must be an absolute container path (e.g. /workspace/spec.md)", j+1, fm.MountPath),
			})
		}
	}
	return errs
}

// localBackendResourceWarnings surfaces a once-per-agent warning when Skills
// or CustomTools are set under backend.kind=local — parity with the
// members/coordinator-under-MA pattern in validateBackendWarnings.
func localBackendResourceWarnings(a *Agent, agentPrefix []string) []Warning {
	var warnings []Warning
	if len(a.Skills) > 0 {
		warnings = append(warnings, Warning{
			Field:   append(append([]string{}, agentPrefix...), "skills"),
			Agent:   a.Name,
			Message: "skills are not supported under backend.kind=local",
		})
	}
	if len(a.CustomTools) > 0 {
		warnings = append(warnings, Warning{
			Field:   append(append([]string{}, agentPrefix...), "custom_tools"),
			Agent:   a.Name,
			Message: "custom_tools are not supported under backend.kind=local",
		})
	}
	if len(a.Files) > 0 {
		warnings = append(warnings, Warning{
			Field:   append(append([]string{}, agentPrefix...), "files"),
			Agent:   a.Name,
			Message: "files are not supported under backend.kind=local; agents on local already have direct host filesystem access",
		})
	}
	return warnings
}

// ValidateResourceReferences runs the additional validation that depends on
// external registries: that agent.Skills entries resolve to skills the user
// has registered (per the orchestra skills cache) and that agent.CustomTools
// entries resolve to handlers wired into the customtools registry.
//
// Under backend.kind=managed_agents an unknown reference is a hard error.
// Under backend.kind=local — including the empty-string default before
// [Config.ResolveDefaults] runs — it's a warning, parity with how
// members/coordinator behave under MA. Callers merge the returned [Result]
// with the result of [Config.Validate] before deciding whether the config
// is good to start a run.
//
// Ordering: callers that want the local-default behavior to apply (i.e. an
// empty Backend.Kind treated as local) should call [Config.ResolveDefaults]
// before this method or simply rely on the empty-as-local fallback below.
// The engine's [pkg/orchestra.Run] applies defaults before constructing the
// run, so the in-engine call is unaffected; pure callers of this method
// should be aware.
//
// skillNames and customToolNames are passed in so unit tests can drive any
// combination without touching the real cache or registry.
func (c *Config) ValidateResourceReferences(skillNames, customToolNames map[string]bool) *Result {
	if c == nil {
		return &Result{Errors: []ConfigError{{Message: "nil config"}}}
	}
	res := &Result{}
	maBackend := c.Backend.Kind == "managed_agents"
	for i := range c.Agents {
		a := &c.Agents[i]
		agentPrefix := []string{"agents", strconv.Itoa(i)}
		appendUnknownSkillFindings(res, a, agentPrefix, skillNames, maBackend)
		appendUnknownToolFindings(res, a, agentPrefix, customToolNames, maBackend)
	}
	if res.Valid() {
		res.Config = c
	}
	return res
}

func appendUnknownSkillFindings(res *Result, a *Agent, agentPrefix []string, skillNames map[string]bool, hardError bool) {
	for j, sk := range a.Skills {
		if sk.Name == "" || skillNames[sk.Name] {
			continue
		}
		field := append(append([]string{}, agentPrefix...), "skills", strconv.Itoa(j), "name")
		msg := fmt.Sprintf("skill %q is not registered (run `orchestra skills upload %s`)", sk.Name, sk.Name)
		if hardError {
			res.Errors = append(res.Errors, ConfigError{Field: field, Agent: a.Name, Message: msg})
		} else {
			res.Warnings = append(res.Warnings, Warning{Field: field, Agent: a.Name, Message: msg})
		}
	}
}

func appendUnknownToolFindings(res *Result, a *Agent, agentPrefix []string, toolNames map[string]bool, hardError bool) {
	for j, ct := range a.CustomTools {
		if ct.Name == "" || toolNames[ct.Name] {
			continue
		}
		field := append(append([]string{}, agentPrefix...), "custom_tools", strconv.Itoa(j), "name")
		msg := fmt.Sprintf("custom_tool %q has no registered handler", ct.Name)
		if hardError {
			res.Errors = append(res.Errors, ConfigError{Field: field, Agent: a.Name, Message: msg})
		} else {
			res.Warnings = append(res.Warnings, Warning{Field: field, Agent: a.Name, Message: msg})
		}
	}
}
