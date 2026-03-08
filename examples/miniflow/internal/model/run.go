package model

import "time"

// Run status constants.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// WorkflowRun represents an instance of a workflow being executed.
type WorkflowRun struct {
	ID          string     `json:"id"`
	WorkflowID  string     `json:"workflow_id"`
	Status      string     `json:"status"`
	Input       string     `json:"input"`
	Output      string     `json:"output"`
	CurrentStep int        `json:"current_step"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// ActivityRun represents an instance of an activity within a workflow run.
type ActivityRun struct {
	ID             string     `json:"id"`
	WorkflowRunID  string     `json:"workflow_run_id"`
	ActivityName   string     `json:"activity_name"`
	ActivityType   string     `json:"activity_type"`
	StepIndex      int        `json:"step_index"`
	Status         string     `json:"status"`
	Input          string     `json:"input"`
	Output         string     `json:"output"`
	Attempts       int        `json:"attempts"`
	MaxRetries     int        `json:"max_retries"`
	TimeoutSeconds int        `json:"timeout_seconds"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	LastHeartbeat  *time.Time `json:"last_heartbeat,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}
