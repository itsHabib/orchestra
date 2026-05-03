package workspace

import "encoding/json"

// AgentResult holds the structured result from one agent's session
// (claude -p subprocess for local backend, MA session for managed_agents).
type AgentResult struct {
	Agent        string  `json:"agent"`
	Status       string  `json:"status"`
	Result       string  `json:"result"`
	CostUSD      float64 `json:"cost_usd"`
	NumTurns     int     `json:"num_turns"`
	DurationMs   int64   `json:"duration_ms"`
	SessionID    string  `json:"session_id"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// UnmarshalJSON accepts the legacy `team` field from v2 results files so an
// orchestra upgrade that lands mid-run can still read prior outputs.
func (r *AgentResult) UnmarshalJSON(data []byte) error {
	type rawResult struct {
		Agent        string  `json:"agent"`
		Team         string  `json:"team"`
		Status       string  `json:"status"`
		Result       string  `json:"result"`
		CostUSD      float64 `json:"cost_usd"`
		NumTurns     int     `json:"num_turns"`
		DurationMs   int64   `json:"duration_ms"`
		SessionID    string  `json:"session_id"`
		InputTokens  int64   `json:"input_tokens"`
		OutputTokens int64   `json:"output_tokens"`
	}
	var raw rawResult
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Agent = raw.Agent
	if r.Agent == "" {
		r.Agent = raw.Team
	}
	r.Status = raw.Status
	r.Result = raw.Result
	r.CostUSD = raw.CostUSD
	r.NumTurns = raw.NumTurns
	r.DurationMs = raw.DurationMs
	r.SessionID = raw.SessionID
	r.InputTokens = raw.InputTokens
	r.OutputTokens = raw.OutputTokens
	return nil
}
