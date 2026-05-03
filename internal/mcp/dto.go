package mcp

import "time"

// Run-level status strings derived from each agent's state. The run is not a
// first-class persisted entity; this is a view over per-agent state.
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
//
// `agents` is the v3 canonical key for the per-agent slice; `teams` is the
// legacy v2 spelling kept on the wire (mirroring `agents`) so v2 clients keep
// reading runs through v3.0. The MCP server populates both. The legacy alias
// is scheduled for removal in v3.x.
type RunView struct {
	RunID        string      `json:"run_id"`
	Status       string      `json:"status"`
	StartedAt    time.Time   `json:"started_at"`
	WorkspaceDir string      `json:"workspace_dir"`
	RepoURL      string      `json:"repo_url,omitempty"`
	DocPaths     []string    `json:"doc_paths,omitempty"`
	PID          int         `json:"pid,omitempty"`
	StateError   string      `json:"state_error,omitempty"`
	Phase        string      `json:"phase,omitempty"`
	PhaseIters   map[string]int `json:"phase_iters,omitempty"`
	LastError    string      `json:"last_error,omitempty"`
	Agents       []AgentView `json:"agents"`
	// Teams mirrors Agents on the wire so v2 clients keep working through
	// v3.0. Populated by the MCP server alongside Agents; do not consume in
	// new code. Removed in v3.x.
	//
	// Deprecated: use Agents.
	Teams []AgentView `json:"teams,omitempty"`
}

// AgentView is the per-agent slice of a RunView. Carries the
// signal_completion outcome verbatim so the chat-side LLM can react without a
// second round-trip.
//
// Renamed from TeamView in v3.
type AgentView struct {
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	SignalStatus  string    `json:"signal_status,omitempty"`
	SignalSummary string    `json:"signal_summary,omitempty"`
	SignalPRURL   string    `json:"signal_pr_url,omitempty"`
	SignalReason  string    `json:"signal_reason,omitempty"`
	CostUSD       float64   `json:"cost_usd,omitempty"`
	LastTool      string    `json:"last_tool,omitempty"`
	LastEventAt   time.Time `json:"last_event_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	Tokens        TokenView `json:"tokens"`
	Artifacts     []string  `json:"artifacts,omitempty"`
	ResultSummary string    `json:"result_summary,omitempty"`
}

// TokenView reports the token totals captured for one agent. Mirrors the
// counters tracked in [store.AgentState] so the chat-side LLM can render a
// cost breakdown without dipping into NDJSON logs.
type TokenView struct {
	InputTokens              int64 `json:"input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
}

// TeamView is the v2 alias for [AgentView], retained so internal callers
// keep building during the v3 migration window.
//
// Deprecated: use [AgentView].
type TeamView = AgentView
