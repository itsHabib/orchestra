package workspace

// State holds the shared state that grows as teams complete.
type State struct {
	Project string                `json:"project"`
	Teams   map[string]TeamState  `json:"teams"`
}

// TeamState holds the result state of a completed team.
type TeamState struct {
	Status        string   `json:"status"`
	ResultSummary string   `json:"result_summary"`
	Artifacts     []string `json:"artifacts"`
	CostUSD       float64  `json:"cost_usd"`
	DurationMs    int64    `json:"duration_ms"`
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
}
