// Package client provides an HTTP client library for the miniflow workflow engine API.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ---------- Types (mirroring server model types) ----------

// ActivitySpec describes a single activity within a workflow definition.
type ActivitySpec struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	InputExpr      string `json:"input_expr"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxRetries     int    `json:"max_retries"`
}

// WorkflowDefinition holds the ordered list of activities for a workflow.
type WorkflowDefinition struct {
	Activities []ActivitySpec `json:"activities"`
}

// Workflow represents a registered workflow.
type Workflow struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Definition WorkflowDefinition `json:"definition"`
	CreatedAt  time.Time          `json:"created_at"`
}

// WorkflowRun represents a single execution of a workflow.
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

// ActivityRun represents the execution of a single activity within a run.
type ActivityRun struct {
	ID             string     `json:"id"`
	WorkflowRunID  string     `json:"workflow_run_id"`
	ActivityName   string     `json:"activity_name"`
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

// Event represents a workflow event in the event history.
type Event struct {
	ID            int64     `json:"id"`
	WorkflowRunID string    `json:"workflow_run_id"`
	ActivityRunID string    `json:"activity_run_id,omitempty"`
	EventType     string    `json:"event_type"`
	Payload       string    `json:"payload"`
	CreatedAt     time.Time `json:"created_at"`
}

// RunDetailResponse is the response returned by GetRun, embedding the run
// together with its activity runs.
type RunDetailResponse struct {
	WorkflowRun
	Activities []ActivityRun `json:"activities"`
}

// StatsResponse contains run counts grouped by status.
type StatsResponse struct {
	Counts map[string]int `json:"counts"`
}

// ---------- Request types ----------

type createWorkflowRequest struct {
	Name       string             `json:"name"`
	Definition WorkflowDefinition `json:"definition"`
}

type startRunRequest struct {
	Input string `json:"input"`
}

// ---------- Error type ----------

// APIError is returned when the server responds with a non-2xx status code.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("miniflow API error (HTTP %d): %s", e.StatusCode, e.Message)
}

// ---------- Client ----------

// Client is an HTTP client for the miniflow workflow engine API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new Client targeting the given base URL.
// If baseURL is empty, it defaults to http://localhost:8080.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ---------- Public methods ----------

// RegisterWorkflow registers a new workflow definition with the server.
func (c *Client) RegisterWorkflow(name string, definition WorkflowDefinition) (*Workflow, error) {
	body := createWorkflowRequest{
		Name:       name,
		Definition: definition,
	}
	var wf Workflow
	if err := c.doJSON(http.MethodPost, "/api/workflows", body, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// GetWorkflow retrieves a workflow by name.
func (c *Client) GetWorkflow(name string) (*Workflow, error) {
	var wf Workflow
	if err := c.doJSON(http.MethodGet, "/api/workflows/"+url.PathEscape(name), nil, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// ListWorkflows returns all registered workflows.
func (c *Client) ListWorkflows() ([]Workflow, error) {
	var wfs []Workflow
	if err := c.doJSON(http.MethodGet, "/api/workflows", nil, &wfs); err != nil {
		return nil, err
	}
	return wfs, nil
}

// StartRun begins a new run of the named workflow with the given input.
func (c *Client) StartRun(workflowName string, input string) (*WorkflowRun, error) {
	body := startRunRequest{Input: input}
	var run WorkflowRun
	if err := c.doJSON(http.MethodPost, "/api/workflows/"+url.PathEscape(workflowName)+"/run", body, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// GetRun retrieves full details for a run, including its activity runs.
func (c *Client) GetRun(id string) (*RunDetailResponse, error) {
	var detail RunDetailResponse
	if err := c.doJSON(http.MethodGet, "/api/runs/"+url.PathEscape(id), nil, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// ListRuns lists workflow runs, optionally filtered by status and limited in count.
// Pass an empty string for status and 0 for limit to skip those filters.
func (c *Client) ListRuns(status string, limit int) ([]WorkflowRun, error) {
	path := "/api/runs"
	params := url.Values{}
	if status != "" {
		params.Set("status", status)
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var runs []WorkflowRun
	if err := c.doJSON(http.MethodGet, path, nil, &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

// GetEvents returns the event history for a run.
func (c *Client) GetEvents(runID string) ([]Event, error) {
	var events []Event
	if err := c.doJSON(http.MethodGet, "/api/runs/"+url.PathEscape(runID)+"/events", nil, &events); err != nil {
		return nil, err
	}
	return events, nil
}

// CancelRun cancels a running workflow run.
func (c *Client) CancelRun(id string) error {
	return c.doJSON(http.MethodPost, "/api/runs/"+url.PathEscape(id)+"/cancel", nil, nil)
}

// Stats returns aggregate run counts grouped by status.
func (c *Client) Stats() (*StatsResponse, error) {
	var stats StatsResponse
	if err := c.doJSON(http.MethodGet, "/api/stats", nil, &stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

// Health performs a health check against the server. Returns nil if healthy.
func (c *Client) Health() error {
	return c.doJSON(http.MethodGet, "/health", nil, nil)
}

// ---------- Internal helpers ----------

// doJSON performs an HTTP request, optionally encoding reqBody as JSON, and
// decoding the response into respBody (if non-nil). Non-2xx responses are
// returned as *APIError.
func (c *Client) doJSON(method, path string, reqBody interface{}, respBody interface{}) error {
	fullURL := c.baseURL + path

	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, fullURL, bodyReader)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := string(respData)
		// Try to extract a structured error message.
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respData, &errResp) == nil && errResp.Error != "" {
			msg = errResp.Error
		}
		return &APIError{
			StatusCode: resp.StatusCode,
			Message:    msg,
		}
	}

	if respBody != nil && len(respData) > 0 {
		if err := json.Unmarshal(respData, respBody); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}

	return nil
}
