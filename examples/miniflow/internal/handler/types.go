package handler

import "miniflow/internal/model"

// CreateWorkflowRequest is the body for POST /api/workflows.
type CreateWorkflowRequest struct {
	Name       string                   `json:"name"`
	Definition model.WorkflowDefinition `json:"definition"`
}

// StartRunRequest is the body for POST /api/workflows/{name}/run.
type StartRunRequest struct {
	Input string `json:"input"`
}

// RunDetailResponse extends WorkflowRun with activity run details.
type RunDetailResponse struct {
	model.WorkflowRun
	Activities []model.ActivityRun `json:"activities"`
}

// StatsResponse is the response for GET /api/stats.
type StatsResponse struct {
	Counts map[string]int `json:"counts"`
}

// ErrorResponse is a standard error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
}
