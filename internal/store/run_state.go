package store

import (
	"encoding/json"
	"time"
)

// RunState is the active run document stored under a workspace.
type RunState struct {
	Project       string                `json:"project"`
	Backend       string                `json:"backend,omitempty"`
	RunID         string                `json:"run_id,omitempty"`
	StartedAt     time.Time             `json:"started_at,omitempty"`
	EnvironmentID string                `json:"environment_id,omitempty"`
	Agents        map[string]AgentState `json:"agents"`

	// Phase is the v3 recipe-runtime phase the run is currently in. Empty
	// for non-recipe runs.
	Phase string `json:"phase,omitempty"`
	// PhaseIters tracks recipe-phase re-entry counts.
	PhaseIters map[string]int `json:"phase_iters,omitempty"`
	// LastError carries the most recent fatal error captured at the run
	// level (e.g. orchestrator-side spawner failures that don't attach to
	// a single agent).
	LastError string `json:"last_error,omitempty"`
}

// UnmarshalJSON accepts the legacy `teams` key from v2 state.json so
// orchestra upgrades that land mid-run can keep reading workspace state.
func (s *RunState) UnmarshalJSON(data []byte) error {
	type rawRunState struct {
		Project       string                `json:"project"`
		Backend       string                `json:"backend,omitempty"`
		RunID         string                `json:"run_id,omitempty"`
		StartedAt     time.Time             `json:"started_at,omitempty"`
		EnvironmentID string                `json:"environment_id,omitempty"`
		Agents        map[string]AgentState `json:"agents"`
		Teams         map[string]AgentState `json:"teams"`
		Phase         string                `json:"phase,omitempty"`
		PhaseIters    map[string]int        `json:"phase_iters,omitempty"`
		LastError     string                `json:"last_error,omitempty"`
	}
	var raw rawRunState
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Project = raw.Project
	s.Backend = raw.Backend
	s.RunID = raw.RunID
	s.StartedAt = raw.StartedAt
	s.EnvironmentID = raw.EnvironmentID
	s.Agents = raw.Agents
	if len(s.Agents) == 0 && len(raw.Teams) > 0 {
		s.Agents = raw.Teams
	}
	s.Phase = raw.Phase
	s.PhaseIters = raw.PhaseIters
	s.LastError = raw.LastError
	return nil
}

// AgentState captures the persisted execution state for one agent.
type AgentState struct {
	Status     string    `json:"status"`
	Tier       *int      `json:"tier,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	DurationMs int64     `json:"duration_ms,omitempty"`

	SessionID     string `json:"session_id,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	ResultSummary string `json:"result_summary,omitempty"`

	PID int `json:"pid,omitempty"`

	AgentID             string               `json:"agent_id,omitempty"`
	AgentVersion        int                  `json:"agent_version,omitempty"`
	LastEventID         string               `json:"last_event_id,omitempty"`
	LastEventAt         time.Time            `json:"last_event_at,omitempty"`
	LastTool            string               `json:"last_tool,omitempty"`
	RepositoryArtifacts []RepositoryArtifact `json:"repository_artifacts,omitempty"`

	CostUSD                  float64 `json:"cost_usd,omitempty"`
	InputTokens              int64   `json:"input_tokens,omitempty"`
	OutputTokens             int64   `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens,omitempty"`
	NumTurns                 int     `json:"num_turns,omitempty"`

	// Signal* are the host-recorded outcome of the agent's signal_completion
	// custom tool call. SignalStatus is "" until the agent calls the tool;
	// "done" or "blocked" thereafter. The remaining fields carry whatever
	// metadata the agent attached. Idempotent: a second call with the same
	// agent is a no-op (the first signal wins) so a confused agent calling
	// twice does not erase the original outcome.
	SignalStatus  string    `json:"signal_status,omitempty"`
	SignalSummary string    `json:"signal_summary,omitempty"`
	SignalPRURL   string    `json:"signal_pr_url,omitempty"`
	SignalReason  string    `json:"signal_reason,omitempty"`
	SignalAt      time.Time `json:"signal_at,omitempty"`

	Artifacts []string `json:"artifacts,omitempty"`
}

// RepositoryArtifact records repository output produced by a managed agent.
type RepositoryArtifact struct {
	URL            string    `json:"url"`
	Branch         string    `json:"branch"`
	BaseSHA        string    `json:"base_sha"`
	CommitSHA      string    `json:"commit_sha"`
	PullRequestURL string    `json:"pull_request_url,omitempty"`
	ResolvedAt     time.Time `json:"resolved_at,omitempty"`
}
