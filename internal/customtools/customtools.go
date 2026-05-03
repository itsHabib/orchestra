// Package customtools is the host-side registry for custom tools an MA agent
// can invoke. The agent emits an `agent.custom_tool_use` event; the engine's
// run loop looks the tool up here, calls Handle to produce a JSON result, and
// relays the result back as a `user.custom_tool_result` event.
//
// The naming intentionally avoids mirroring the SDK's BetaManagedAgentsCustom*
// nouns — this package describes orchestra's host-side workflow primitives,
// not the wire format.
package customtools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/itsHabib/orchestra/internal/artifacts"
	"github.com/itsHabib/orchestra/internal/notify"
	"github.com/itsHabib/orchestra/internal/store"
)

// Definition is the host-side description of a custom tool. The fields map
// 1:1 to the SDK's BetaManagedAgentsCustomToolParams shape, but the engine
// translates Definition → SDK params at agent-creation time so handler code
// never imports the SDK.
type Definition struct {
	// Name is the tool identifier the agent emits in agent.custom_tool_use.
	// Must be unique across the registry.
	Name string

	// Description is shown to the agent. Should make the tool's purpose
	// unambiguous in one or two sentences.
	Description string

	// InputSchema is a JSON Schema fragment declaring the tool's input shape.
	// Map[string]any is enough for the validation surface MA actually
	// enforces; richer schema features (refs, allOf) can be added later.
	InputSchema map[string]any
}

// RunContext is the slice of engine state a Handler needs to act on a tool
// call. Construct one per dispatch — the values are cheap to copy and the
// per-call freshness lets future handlers see updated run-level state.
type RunContext struct {
	// Store is the run state writer the handler may mutate via
	// UpdateAgentState. Required for handlers that record outcomes.
	Store store.Store

	// Notifier is the host-side notification fan-out. Optional; nil disables
	// notification (e.g. unit tests, or runs that never want a system bell).
	Notifier notify.Notifier

	// RunID is the active run identifier, used as a label on notifications
	// and the NDJSON log records. Empty is acceptable when the engine has
	// not yet recorded a run id (very early in run startup).
	RunID string

	// Now is the engine clock. Defaults to time.Now().UTC() when nil.
	Now func() time.Time

	// Artifacts persists structured outputs the agent attaches to its
	// signal_completion call. Nil disables artifact persistence — the
	// signal_completion handler then ignores any artifacts payload rather
	// than erroring, so unit tests and the local backend (which doesn't
	// dispatch through customtools yet) keep working.
	Artifacts artifacts.Store

	// Phase is the recipe-runtime phase the agent is executing in. Snapshot
	// at RunContext construction time — UpdateAgentState's mutex serializes
	// per-team writes so a phase change mid-Handle is impossible. Empty for
	// non-recipe runs (the only shape that exists today; the recipe runtime
	// lands with Phase B).
	Phase string
}

// Time returns Now() in UTC, falling back to time.Now().UTC() when Now is
// nil. Handlers should use this rather than time.Now() directly so test
// clocks reach signal_at and notification timestamps.
func (c *RunContext) Time() time.Time {
	if c == nil || c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

// Handler executes one custom tool. Tool() returns the registration metadata
// (the engine consults it when populating the AgentSpec); Handle() is invoked
// per agent.custom_tool_use event.
//
// The result is JSON the engine relays back via user.custom_tool_result.
// A non-nil error is reported with is_error=true on the result event — the
// agent can decide whether to retry, escalate, or stop. Handlers should keep
// errors actionable (e.g. "missing required field 'status'") because the
// agent sees the message verbatim.
type Handler interface {
	Tool() Definition
	Handle(ctx context.Context, run *RunContext, team string, input json.RawMessage) (json.RawMessage, error)
}
