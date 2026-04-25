package spawner

import "time"

// EventID is the backend event identifier.
type EventID string

// EventType is the backend event type tag.
type EventType string

const (
	// EventTypeAgentMessage is emitted when the agent sends message content.
	EventTypeAgentMessage EventType = "agent.message"
	// EventTypeAgentThinking is emitted when the agent sends thinking content.
	EventTypeAgentThinking EventType = "agent.thinking"
	// EventTypeAgentToolUse is emitted when the agent invokes a tool.
	EventTypeAgentToolUse EventType = "agent.tool_use"
	// EventTypeAgentToolResult is emitted when a tool result is available.
	EventTypeAgentToolResult EventType = "agent.tool_result"
	// EventTypeAgentMCPToolUse is emitted when the agent invokes an MCP tool.
	EventTypeAgentMCPToolUse EventType = "agent.mcp_tool_use"
	// EventTypeAgentMCPToolResult is emitted when an MCP tool result is available.
	EventTypeAgentMCPToolResult EventType = "agent.mcp_tool_result"
	// EventTypeAgentCustomToolUse is emitted when the agent invokes a custom tool.
	EventTypeAgentCustomToolUse EventType = "agent.custom_tool_use"
	// EventTypeAgentThreadContextCompacted is emitted after context compaction.
	EventTypeAgentThreadContextCompacted EventType = "agent.thread_context_compacted"
	// EventTypeSessionStatusRunning reports a running session.
	EventTypeSessionStatusRunning EventType = "session.status_running"
	// EventTypeSessionStatusIdle reports an idle session.
	EventTypeSessionStatusIdle EventType = "session.status_idle"
	// EventTypeSessionStatusRescheduled reports a rescheduled session.
	EventTypeSessionStatusRescheduled EventType = "session.status_rescheduled"
	// EventTypeSessionStatusTerminated reports a terminated session.
	EventTypeSessionStatusTerminated EventType = "session.status_terminated"
	// EventTypeSessionError reports a session error.
	EventTypeSessionError EventType = "session.error"
	// EventTypeSpanModelRequestStart reports the start of a model request span.
	EventTypeSpanModelRequestStart EventType = "span.model_request_start"
	// EventTypeSpanModelRequestEnd reports the end of a model request span.
	EventTypeSpanModelRequestEnd EventType = "span.model_request_end"
	// EventTypeUserMessage echoes a user.message event delivered into a session
	// (typically by the steering CLI). The translator maps these back through
	// the running orchestrator's event stream so that LastEventID / LastEventAt
	// advance and the run log shows the human's input alongside agent output.
	EventTypeUserMessage EventType = "user.message"
	// EventTypeUserInterrupt echoes a user.interrupt event delivered into a
	// session.
	EventTypeUserInterrupt EventType = "user.interrupt"
	// EventTypeUnknown preserves events the orchestrator does not yet understand.
	EventTypeUnknown EventType = "unknown"
)

// Event is the tagged event union shared by spawner backends.
type Event interface {
	EventID() EventID
	EventType() EventType
	EventProcessedAt() time.Time
}

// BaseEvent holds fields common to all backend events.
type BaseEvent struct {
	ID          EventID
	Type        EventType
	ProcessedAt time.Time
	Metadata    map[string]string
}

// EventID returns the backend event identifier.
func (e BaseEvent) EventID() EventID { return e.ID }

// EventType returns the backend event type tag.
func (e BaseEvent) EventType() EventType { return e.Type }

// EventProcessedAt returns the backend processing timestamp.
func (e BaseEvent) EventProcessedAt() time.Time { return e.ProcessedAt }

// ContentBlock is a text or tool-related block inside an agent message.
type ContentBlock struct {
	Type      string
	Text      string
	Name      string
	ID        string
	Input     any
	ToolUseID string
}

// AgentMessageEvent represents an agent.message event.
type AgentMessageEvent struct {
	BaseEvent
	Role    string
	Content []ContentBlock
	Text    string
}

// AgentThinkingEvent represents an agent.thinking event.
type AgentThinkingEvent struct {
	BaseEvent
	Text string
}

// ToolUse describes an agent tool invocation.
type ToolUse struct {
	ID    string
	Name  string
	Input any
}

// ToolResult describes a tool result emitted back to the agent.
type ToolResult struct {
	ToolUseID string
	Content   any
	Error     string
}

// AgentToolUseEvent represents an agent.tool_use event.
type AgentToolUseEvent struct {
	BaseEvent
	ToolUse ToolUse
}

// AgentToolResultEvent represents an agent.tool_result event.
type AgentToolResultEvent struct {
	BaseEvent
	ToolResult ToolResult
}

// AgentMCPToolUseEvent represents an agent.mcp_tool_use event.
type AgentMCPToolUseEvent struct {
	BaseEvent
	ServerName string
	ToolUse    ToolUse
}

// AgentMCPToolResultEvent represents an agent.mcp_tool_result event.
type AgentMCPToolResultEvent struct {
	BaseEvent
	ServerName string
	ToolResult ToolResult
}

// AgentCustomToolUseEvent represents an agent.custom_tool_use event.
type AgentCustomToolUseEvent struct {
	BaseEvent
	ToolUse ToolUse
}

// AgentThreadContextCompactedEvent represents context compaction.
type AgentThreadContextCompactedEvent struct {
	BaseEvent
	Summary string
}

// SessionStatusRunningEvent represents session.status_running.
type SessionStatusRunningEvent struct {
	BaseEvent
	Status SessionStatus
}

// SessionStatusIdleEvent represents session.status_idle.
type SessionStatusIdleEvent struct {
	BaseEvent
	Status SessionStatus
}

// SessionStatusRescheduledEvent represents session.status_rescheduled.
type SessionStatusRescheduledEvent struct {
	BaseEvent
	Status SessionStatus
}

// SessionStatusTerminatedEvent represents session.status_terminated.
type SessionStatusTerminatedEvent struct {
	BaseEvent
	Status SessionStatus
}

// SessionErrorEvent represents session.error.
type SessionErrorEvent struct {
	BaseEvent
	Message string
	Code    string
}

// SpanModelRequestStartEvent represents span.model_request_start.
type SpanModelRequestStartEvent struct {
	BaseEvent
	Model string
}

// SpanModelRequestEndEvent represents span.model_request_end.
type SpanModelRequestEndEvent struct {
	BaseEvent
	Model string
	Usage Usage
}

// UserMessageEchoEvent represents MA's echo of a user.message event delivered
// into the session (e.g. by `orchestra msg`). The translator emits this so the
// running orchestrator can advance LastEventID / LastEventAt and surface the
// human's input in the run log; team status is not mutated.
type UserMessageEchoEvent struct {
	BaseEvent
	Text string
}

// UserInterruptEchoEvent represents MA's echo of a user.interrupt event
// delivered into the session (e.g. by `orchestra interrupt`).
type UserInterruptEchoEvent struct {
	BaseEvent
}

// UnknownEvent preserves events the orchestrator does not yet interpret.
type UnknownEvent struct {
	BaseEvent
	Payload any
}

// UserEventType is the type tag for events sent by a user/client.
type UserEventType string

const (
	// UserEventTypeMessage sends user text into a session.
	UserEventTypeMessage UserEventType = "user.message"
	// UserEventTypeInterrupt interrupts a running session.
	UserEventTypeInterrupt UserEventType = "user.interrupt"
	// UserEventTypeToolConfirmation answers a pending tool confirmation.
	UserEventTypeToolConfirmation UserEventType = "user.tool_confirmation"
	// UserEventTypeCustomToolResult answers a custom tool use.
	UserEventTypeCustomToolResult UserEventType = "user.custom_tool_result"
	// UserEventTypeDefineOutcome defines a user outcome.
	UserEventTypeDefineOutcome UserEventType = "user.define_outcome"
)

// UserEvent is an event sent into a session.
type UserEvent struct {
	Type             UserEventType
	Message          string
	InterruptReason  string
	ToolConfirmation *ToolConfirmation
	CustomToolResult *CustomToolResult
	Outcome          *Outcome
	Metadata         map[string]string
}

// ToolConfirmation answers a pending tool confirmation request.
type ToolConfirmation struct {
	ToolUseID EventID
	Allow     bool
	Message   string
}

// CustomToolResult responds to a backend custom tool invocation.
type CustomToolResult struct {
	ToolUseID EventID
	Result    any
	Error     string
}

// Outcome describes a user-defined session outcome.
type Outcome struct {
	Name        string
	Description string
	Metadata    map[string]string
}
