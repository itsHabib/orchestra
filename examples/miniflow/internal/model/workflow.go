package model

import "time"

// ActivitySpec defines a single activity within a workflow definition.
type ActivitySpec struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	InputExpr      string `json:"input_expr"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxRetries     int    `json:"max_retries"`
}

// WorkflowDefinition is the JSON structure stored in workflows.definition.
type WorkflowDefinition struct {
	Activities []ActivitySpec `json:"activities"`
}

// Workflow represents a registered workflow template.
type Workflow struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Definition WorkflowDefinition `json:"definition"`
	CreatedAt  time.Time          `json:"created_at"`
}
