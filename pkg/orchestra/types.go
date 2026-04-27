package orchestra

import (
	"errors"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/store"
)

// Config is the YAML schema for an orchestra run.
//
// Experimental: aliased from internal/config so that field additions there
// flow through transparently. Stability of the alias target is governed by
// internal/config until this surface is marked stable.
type Config = config.Config

// Defaults holds default values applied to all teams unless overridden.
//
// Experimental: aliased from internal/config.
type Defaults = config.Defaults

// Backend selects the runtime backend.
//
// Experimental: aliased from internal/config.
type Backend = config.Backend

// ManagedAgentsBackend captures managed-agents-specific backend settings.
// Aliased so callers can construct repository-backed configs without
// reaching into internal packages.
//
// Experimental: aliased from internal/config.
type ManagedAgentsBackend = config.ManagedAgentsBackend

// RepositorySpec describes a GitHub repository attached to managed-agents
// sessions.
//
// Experimental: aliased from internal/config.
type RepositorySpec = config.RepositorySpec

// EnvironmentOverride lets a single team substitute backend-level
// environment fields (currently just Repository) without touching others.
//
// Experimental: aliased from internal/config.
type EnvironmentOverride = config.EnvironmentOverride

// Coordinator configures the optional top-level coordinator agent.
//
// Experimental: aliased from internal/config.
type Coordinator = config.Coordinator

// Team represents a single team or solo agent in the orchestration.
//
// Experimental: aliased from internal/config.
type Team = config.Team

// Lead represents the team lead configuration.
//
// Experimental: aliased from internal/config.
type Lead = config.Lead

// Member represents a team member.
//
// Experimental: aliased from internal/config.
type Member = config.Member

// Task represents a unit of work assigned to a team.
//
// Experimental: aliased from internal/config.
type Task = config.Task

// SkillRef references a skill registered in the orchestra skills cache that
// should be attached to the team's MA agent. Type defaults to "custom"
// (skills the user uploaded via `orchestra skills upload`); "anthropic" is
// reserved for first-party skills.
//
// Experimental: aliased from internal/config.
type SkillRef = config.SkillRef

// CustomToolRef references a host-side custom tool by the name registered
// in the customtools package.
//
// Experimental: aliased from internal/config.
type CustomToolRef = config.CustomToolRef

// Warning represents a non-fatal validation issue surfaced by LoadConfig
// or Validate. It does not block execution. Field is the structured YAML
// path to the offending node (empty for project-level issues); Team is
// the denormalized team name when Field points into a team subtree.
//
// Experimental: aliased from internal/config.
type Warning = config.Warning

// ConfigError is a hard validation failure surfaced by [LoadConfig] or
// [Validate]. Same shape and semantics as [Warning]; only the severity
// differs. A non-empty Errors slice on a [ValidationResult] makes
// [ValidationResult.Valid] return false and [ValidationResult.Err]
// return non-nil.
//
// Experimental: aliased from internal/config.
type ConfigError = config.ConfigError

// ValidationResult is the aggregate output of [LoadConfig] and
// [Validate]: the parsed config (when valid), the structured warnings
// (soft issues), and the structured errors (hard validation failures).
// Use [ValidationResult.Valid] to gate further use of Config; use
// [ValidationResult.Err] for an error-shaped view of the validation
// failures suitable for `if err != nil` patterns.
//
// Experimental: aliased from internal/config; field set may grow.
type ValidationResult = config.Result

// ErrInvalidConfig is wrapped by [ValidationResult.Err] whenever the
// result has at least one entry in Errors. Callers can use errors.Is to
// recognize validation failures regardless of how the formatted message
// changes:
//
//	res, err := orchestra.LoadConfig("orchestra.yaml")
//	if err != nil {
//	    return err // I/O or parse failure
//	}
//	if errors.Is(res.Err(), orchestra.ErrInvalidConfig) { ... }
//
// Experimental: this sentinel is kept stable across breaking surface
// changes so callers can rely on errors.Is checks.
var ErrInvalidConfig = config.ErrInvalidConfig

// RunState is the persistent run document. Run-time observers can read it
// from the workspace via tools that already understand the schema.
//
// Experimental: aliased from internal/store.
type RunState = store.RunState

// TeamState is the persisted execution state for one team. After P2.0 it
// includes NumTurns alongside the existing token / cost counters, so the
// SDK can render a complete summary without dipping into the workspace.
//
// Experimental: aliased from internal/store.
type TeamState = store.TeamState

// RepositoryArtifact records repository output produced by a managed agent.
//
// Experimental: aliased from internal/store.
type RepositoryArtifact = store.RepositoryArtifact

// Backend kind constants. Use these instead of bare strings on
// [Config.Backend.Kind] to avoid typos.
const (
	// BackendLocal selects the local subprocess backend (claude -p).
	BackendLocal = "local"
	// BackendManagedAgents selects the Anthropic Managed Agents backend.
	BackendManagedAgents = "managed_agents"
)

// ErrRunInProgress is returned by [Run] or [Start] when another invocation
// against the same workspace within the same process is already in flight.
// Different workspace directories are independent. The cross-process case
// is handled by the workspace exclusive lock in the underlying store.
//
// Experimental: this sentinel is kept stable across breaking surface
// changes so callers can rely on errors.Is checks.
var ErrRunInProgress = errors.New("orchestra: run already in progress for workspace")

// Result is the SDK's view of a completed (or partially completed) run.
// All per-team data the CLI summary renderer needs lives here — callers do
// not need to read .orchestra/results/ off disk.
//
// Experimental: field set may grow as dogfood apps surface needs.
type Result struct {
	// Project is the configured project name.
	Project string
	// Teams maps team name to its final TeamResult.
	Teams map[string]TeamResult
	// Tiers is the tier-by-tier team-name layout, for ordered rendering.
	Tiers [][]string
	// DurationMs is the wall-clock duration of the run in milliseconds.
	DurationMs int64
}

// TeamResult is the SDK-shaped per-team view: it embeds [TeamState] so all
// status, cost, token, and turn counters are accessible directly. The
// wrapper exists so future SDK-only fields can be added without touching
// the persisted [TeamState].
//
// Experimental: this shape is stable in spirit, but additive growth is
// expected during the experimental phase.
type TeamResult struct {
	TeamState
}
