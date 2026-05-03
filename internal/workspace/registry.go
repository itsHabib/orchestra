package workspace

import (
	"encoding/json"
	"errors"
	"time"
)

// Registry tracks all agents and their execution metadata.
type Registry struct {
	Project string          `json:"project"`
	Agents  []RegistryEntry `json:"agents"`
}

// UnmarshalJSON accepts the legacy `teams:` key from v2 registry.json so
// an orchestra upgrade that lands mid-run can keep reading the in-flight
// file. Setting both keys at the same time is rejected — registry.json is
// orchestra-written, never hand-edited, so a dual-key payload almost
// certainly means a writer bug; failing fast surfaces it instead of
// papering over it with a silent precedence rule.
func (r *Registry) UnmarshalJSON(data []byte) error {
	type rawRegistry struct {
		Project string          `json:"project"`
		Agents  []RegistryEntry `json:"agents"`
		Teams   []RegistryEntry `json:"teams"`
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	_, hasAgents := probe["agents"]
	_, hasTeams := probe["teams"]
	if hasAgents && hasTeams {
		return errors.New("registry: registry.json sets both `agents` and `teams`; orchestra writers should only produce one key")
	}
	var raw rawRegistry
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Project = raw.Project
	r.Agents = raw.Agents
	if hasTeams {
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
