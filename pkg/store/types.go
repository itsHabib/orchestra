package store

import "time"

// RunState is the active run document stored under a workspace.
type RunState struct {
	Project       string               `json:"project"`
	Backend       string               `json:"backend,omitempty"`
	RunID         string               `json:"run_id,omitempty"`
	StartedAt     time.Time            `json:"started_at,omitempty"`
	EnvironmentID string               `json:"environment_id,omitempty"`
	Teams         map[string]TeamState `json:"teams"`
}

// TeamState captures the persisted execution state for one team.
type TeamState struct {
	Status     string    `json:"status"`
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
	RepositoryArtifacts []RepositoryArtifact `json:"repository_artifacts,omitempty"`

	CostUSD                  float64 `json:"cost_usd,omitempty"`
	InputTokens              int64   `json:"input_tokens,omitempty"`
	OutputTokens             int64   `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens,omitempty"`

	Artifacts []string `json:"artifacts,omitempty"`
}

// RepositoryArtifact records repository output produced by a managed agent.
type RepositoryArtifact struct {
	URL            string `json:"url"`
	Branch         string `json:"branch"`
	BaseSHA        string `json:"base_sha"`
	CommitSHA      string `json:"commit_sha"`
	PullRequestURL string `json:"pull_request_url,omitempty"`
}

// AgentRecord is the user-scoped cache entry for a managed agent.
type AgentRecord struct {
	Key       string    `json:"key"`
	Project   string    `json:"project"`
	Role      string    `json:"role"`
	AgentID   string    `json:"agent_id"`
	Version   int       `json:"version"`
	SpecHash  string    `json:"spec_hash"`
	UpdatedAt time.Time `json:"updated_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

// EnvRecord is the user-scoped cache entry for a managed environment.
type EnvRecord struct {
	Key       string    `json:"key"`
	Project   string    `json:"project"`
	Name      string    `json:"name"`
	EnvID     string    `json:"env_id"`
	SpecHash  string    `json:"spec_hash"`
	UpdatedAt time.Time `json:"updated_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

// LockMode selects whether a run lock is exclusive or shared.
type LockMode int

const (
	// LockExclusive prevents any other exclusive or shared run lock from being acquired.
	LockExclusive LockMode = iota

	// LockShared allows other shared holders but blocks exclusive holders.
	LockShared
)
