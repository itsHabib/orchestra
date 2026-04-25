package store

import "time"

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
