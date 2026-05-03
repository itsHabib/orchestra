package workspace

import (
	"encoding/json"
	"time"
)

// Registry tracks all agents and their execution metadata.
type Registry struct {
	Project string          `json:"project"`
	Agents  []RegistryEntry `json:"agents"`
}

// UnmarshalJSON accepts the legacy `teams:` key from v2 registry.json so an
// orchestra upgrade that lands mid-run can keep reading the in-flight file.
// Mixed `agents`+`teams` keys are not supported (callers should not be
// producing both).
func (r *Registry) UnmarshalJSON(data []byte) error {
	type rawRegistry struct {
		Project string          `json:"project"`
		Agents  []RegistryEntry `json:"agents"`
		Teams   []RegistryEntry `json:"teams"`
	}
	var raw rawRegistry
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Project = raw.Project
	r.Agents = raw.Agents
	if len(r.Agents) == 0 && len(raw.Teams) > 0 {
		r.Agents = raw.Teams
	}
	return nil
}

// RegistryEntry tracks the execution state of a single agent.
type RegistryEntry struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	SessionID string    `json:"session_id"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}
