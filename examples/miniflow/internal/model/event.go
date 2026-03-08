package model

import "time"

// Event type constants.
const (
	EventWorkflowStarted   = "workflow_started"
	EventWorkflowCompleted = "workflow_completed"
	EventWorkflowFailed    = "workflow_failed"
	EventWorkflowCancelled = "workflow_cancelled"

	EventActivityScheduled  = "activity_scheduled"
	EventActivityStarted    = "activity_started"
	EventActivityCompleted  = "activity_completed"
	EventActivityFailed     = "activity_failed"
	EventActivityRetried    = "activity_retried"
	EventActivityTimedOut   = "activity_timed_out"
	EventActivityHeartbeat  = "activity_heartbeat"
)

// Event is an immutable log entry tracking state changes (event sourcing).
type Event struct {
	ID            int64     `json:"id"`
	WorkflowRunID string    `json:"workflow_run_id"`
	ActivityRunID string    `json:"activity_run_id,omitempty"`
	EventType     string    `json:"event_type"`
	Payload       string    `json:"payload"`
	CreatedAt     time.Time `json:"created_at"`
}
