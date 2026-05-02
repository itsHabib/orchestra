package mcp

import "time"

// Run-level status strings derived from each team's state. The run is not a
// first-class persisted entity; this is a view over per-team state.
const (
	RunStatusRunning = "running"
	RunStatusBlocked = "blocked"
	RunStatusFailed  = "failed"
	RunStatusDone    = "done"
)

// RunView is one entry in list_runs / the body of get_run. Combines the MCP-
// side registry data with the per-run state.json snapshot. Returned over MCP;
// do not return internal/store or pkg/orchestra types directly — the JSON
// shape here is the public contract.
type RunView struct {
	RunID        string     `json:"run_id"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	WorkspaceDir string     `json:"workspace_dir"`
	RepoURL      string     `json:"repo_url,omitempty"`
	DocPaths     []string   `json:"doc_paths,omitempty"`
	PID          int        `json:"pid,omitempty"`
	StateError   string     `json:"state_error,omitempty"`
	Teams        []TeamView `json:"teams"`
}

// TeamView is the per-team slice of a RunView. Carries the signal_completion
// outcome verbatim so the chat-side LLM can react without a second round-trip.
type TeamView struct {
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	SignalStatus  string  `json:"signal_status,omitempty"`
	SignalSummary string  `json:"signal_summary,omitempty"`
	SignalPRURL   string  `json:"signal_pr_url,omitempty"`
	SignalReason  string  `json:"signal_reason,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
}
