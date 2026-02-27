package workspace

import "time"

// Registry tracks all teams and their execution metadata.
type Registry struct {
	Project string          `json:"project"`
	Teams   []RegistryEntry `json:"teams"`
}

// RegistryEntry tracks the execution state of a single team.
type RegistryEntry struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	SessionID string    `json:"session_id"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}
