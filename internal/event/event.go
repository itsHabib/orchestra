// Package event defines the canonical structured event types emitted by
// the orchestra engine. The pkg/orchestra SDK type-aliases these so SDK
// callers depend on a stable shape; the engine and emitter implementations
// import this package directly to avoid an import cycle through
// pkg/orchestra.
package event

import "time"

// Kind enumerates the structured event types emitted during a run. Most
// kinds carry a (Tier, Team) pair; a handful are run-level and use Tier=-1
// with Team="" to signal "no team scope".
type Kind int

// Event kinds emitted by the engine. Each constant's godoc describes when
// the event fires and which Event fields are populated.
const (
	// KindTierStart fires once at the top of each tier, before any team in
	// the tier begins. Populates Tier, Team="", Message=team-list (joined),
	// At.
	KindTierStart Kind = iota
	// KindTeamStart fires once per team when its goroutine begins, after
	// RecordTeamStart succeeds. Populates Tier, Team, Message=role, At.
	KindTeamStart
	// KindTeamMessage carries the agent's natural-language output. Local
	// backend: emitted from the spawner ProgressFunc with the raw message.
	// Managed-agents backend: emitted on AgentMessageEvent /
	// UserMessageEchoEvent (with "human:" prefix). Populates Tier, Team,
	// Message, At.
	KindTeamMessage
	// KindToolCall fires when the agent invokes a tool. Today only the
	// managed-agents backend distinguishes tool calls from natural-language
	// output; the local backend collapses everything to KindTeamMessage.
	// Populates Tier, Team, Tool, Message=input-summary, At.
	KindToolCall
	// KindToolResult fires when a tool returns its result to the agent.
	// Today only the managed-agents backend emits this; the local backend
	// does not currently distinguish tool results. Populates Tier, Team,
	// Tool, Message=result-summary, At.
	KindToolResult
	// KindTeamComplete fires when a team finishes successfully. Populates
	// Tier, Team, Message=summary line, Cost, Turns, At.
	KindTeamComplete
	// KindTeamFailed fires when a team's run errors out. Populates Tier,
	// Team, Message=error string, At.
	KindTeamFailed
	// KindTierComplete fires once at the end of each tier, after every
	// team in the tier has settled. Populates Tier, Team="", At.
	KindTierComplete
	// KindRunComplete fires once when the engine has finished all tiers
	// and is about to transition to PhaseDone. Populates Tier=-1, Team="",
	// At.
	KindRunComplete
	// KindDropped is synthetic — the engine prepends it ahead of the next
	// real event when the consumer fell behind and one or more events were
	// dropped from the bounded buffer. Populates Tier=-1, Team="",
	// DropCount=number-of-dropped-events, At.
	KindDropped
	// KindInfo carries an engine-level informational message ("Workspace
	// initialized at ...", "DAG: N tiers"). Populates Tier=-1, Team="",
	// Message, At.
	KindInfo
	// KindWarn carries an engine-level non-fatal warning. Populates
	// Tier=-1, Team="", Message, At.
	KindWarn
	// KindError carries an engine-level error message. The run's actual
	// error is still surfaced by Wait()'s return value; this is for
	// human-visible output along the way. Populates Tier=-1, Team="",
	// Message, At.
	KindError
)

// Event is a structured observation of a run-time event. Field population
// is per-Kind; consult the doc on each Kind constant for which fields are
// non-zero.
type Event struct {
	// Kind discriminates the event — see the Kind constants for what fires
	// when and which fields are populated.
	Kind Kind
	// Tier is the tier index the event scopes to, or -1 for run-level
	// events (KindInfo, KindWarn, KindError, KindDropped, KindRunComplete).
	Tier int
	// Team is the team the event scopes to, or "" for run-level events
	// and tier-level events (KindTierStart, KindTierComplete).
	Team string
	// Message carries the natural-language content where applicable. See
	// the per-Kind doc for the exact shape.
	Message string
	// Tool is the tool name on KindToolCall / KindToolResult, otherwise "".
	Tool string
	// At is the wall-clock time the engine emitted the event.
	At time.Time
	// Cost is the running team cost (USD) when relevant — populated on
	// KindTeamComplete.
	Cost float64
	// Turns is the team's current turn count when relevant — populated on
	// KindTeamComplete.
	Turns int
	// DropCount is populated only on KindDropped: the number of events
	// dropped from the bounded buffer since the last KindDropped fire.
	DropCount int
}

// Emitter is the engine-side abstraction for delivering events to whatever
// per-run sink the SDK has constructed. The pkg/orchestra Handle implements
// this interface and routes events through a bounded channel plus an
// optional synchronous handler.
//
// Emit must be safe for concurrent use — multiple team goroutines call it
// in parallel within a tier.
type Emitter interface {
	Emit(ev Event)
}

// NoopEmitter discards every event. Useful as a default for code paths
// (tests, callers without a Handle) that don't need event delivery.
type NoopEmitter struct{}

// Emit implements Emitter by discarding ev.
func (NoopEmitter) Emit(Event) {}
