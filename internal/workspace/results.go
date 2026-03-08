package workspace

// TeamResult holds the structured result from a claude -p session.
type TeamResult struct {
	Team         string `json:"team"`
	Status       string `json:"status"`
	Result       string `json:"result"`
	CostUSD      float64 `json:"cost_usd"`
	NumTurns     int    `json:"num_turns"`
	DurationMs   int64  `json:"duration_ms"`
	SessionID    string `json:"session_id"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}
