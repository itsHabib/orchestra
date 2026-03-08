package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ---------- helpers ----------

// newTestServer creates an httptest.Server that routes requests to the given handler map.
// The handler map keys are "METHOD /path" strings.
func newTestServer(t *testing.T, routes map[string]http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, handler := range routes {
		mux.HandleFunc(pattern, handler)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, NewClient(ts.URL)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ---------- RegisterWorkflow ----------

func TestRegisterWorkflow(t *testing.T) {
	now := time.Now().Truncate(time.Second)

	var gotMethod, gotPath, gotContentType string
	var gotBody createWorkflowRequest

	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"POST /api/workflows": func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotContentType = r.Header.Get("Content-Type")
			json.NewDecoder(r.Body).Decode(&gotBody)
			writeJSON(w, http.StatusCreated, Workflow{
				ID:   "wf-1",
				Name: "test-wf",
				Definition: WorkflowDefinition{
					Activities: []ActivitySpec{{Name: "step1", Type: "http"}},
				},
				CreatedAt: now,
			})
		},
	})

	def := WorkflowDefinition{
		Activities: []ActivitySpec{{Name: "step1", Type: "http"}},
	}
	wf, err := c.RegisterWorkflow("test-wf", def)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/api/workflows" {
		t.Errorf("expected path /api/workflows, got %s", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("expected content-type application/json, got %s", gotContentType)
	}
	if gotBody.Name != "test-wf" {
		t.Errorf("expected body name test-wf, got %s", gotBody.Name)
	}
	if wf.ID != "wf-1" {
		t.Errorf("expected ID wf-1, got %s", wf.ID)
	}
	if wf.Name != "test-wf" {
		t.Errorf("expected Name test-wf, got %s", wf.Name)
	}
	if len(wf.Definition.Activities) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(wf.Definition.Activities))
	}
}

// ---------- GetWorkflow ----------

func TestGetWorkflow(t *testing.T) {
	var gotPath string

	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/workflows/{name}": func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			writeJSON(w, http.StatusOK, Workflow{
				ID:   "wf-1",
				Name: "my-wf",
			})
		},
	})

	wf, err := c.GetWorkflow("my-wf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/workflows/my-wf" {
		t.Errorf("expected path /api/workflows/my-wf, got %s", gotPath)
	}
	if wf.Name != "my-wf" {
		t.Errorf("expected Name my-wf, got %s", wf.Name)
	}
}

// ---------- ListWorkflows ----------

func TestListWorkflows(t *testing.T) {
	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/workflows": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, []Workflow{
				{ID: "wf-1", Name: "a"},
				{ID: "wf-2", Name: "b"},
			})
		},
	})

	wfs, err := c.ListWorkflows()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wfs) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(wfs))
	}
	if wfs[0].Name != "a" || wfs[1].Name != "b" {
		t.Errorf("unexpected workflow names: %s, %s", wfs[0].Name, wfs[1].Name)
	}
}

// ---------- StartRun ----------

func TestStartRun(t *testing.T) {
	var gotPath string
	var gotBody startRunRequest

	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"POST /api/workflows/{name}/run": func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&gotBody)
			writeJSON(w, http.StatusCreated, WorkflowRun{
				ID:     "run-1",
				Status: "pending",
				Input:  "hello",
			})
		},
	})

	run, err := c.StartRun("my-wf", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/workflows/my-wf/run" {
		t.Errorf("expected path /api/workflows/my-wf/run, got %s", gotPath)
	}
	if gotBody.Input != "hello" {
		t.Errorf("expected input hello, got %s", gotBody.Input)
	}
	if run.ID != "run-1" {
		t.Errorf("expected run ID run-1, got %s", run.ID)
	}
	if run.Status != "pending" {
		t.Errorf("expected status pending, got %s", run.Status)
	}
}

// ---------- GetRun ----------

func TestGetRun(t *testing.T) {
	var gotPath string

	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/runs/{id}": func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			writeJSON(w, http.StatusOK, RunDetailResponse{
				WorkflowRun: WorkflowRun{
					ID:     "run-1",
					Status: "running",
				},
				Activities: []ActivityRun{
					{ID: "act-1", ActivityName: "step1", Status: "completed"},
				},
			})
		},
	})

	detail, err := c.GetRun("run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/runs/run-1" {
		t.Errorf("expected path /api/runs/run-1, got %s", gotPath)
	}
	if detail.ID != "run-1" {
		t.Errorf("expected run ID run-1, got %s", detail.ID)
	}
	if len(detail.Activities) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(detail.Activities))
	}
	if detail.Activities[0].ActivityName != "step1" {
		t.Errorf("expected activity name step1, got %s", detail.Activities[0].ActivityName)
	}
}

// ---------- ListRuns ----------

func TestListRuns(t *testing.T) {
	var gotQuery string

	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/runs": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			writeJSON(w, http.StatusOK, []WorkflowRun{
				{ID: "run-1", Status: "completed"},
			})
		},
	})

	runs, err := c.ListRuns("completed", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	// Check query params (order may vary so check both)
	if gotQuery != "limit=10&status=completed" && gotQuery != "status=completed&limit=10" {
		t.Errorf("unexpected query string: %s", gotQuery)
	}
}

func TestListRunsNoFilters(t *testing.T) {
	var gotQuery string

	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/runs": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			writeJSON(w, http.StatusOK, []WorkflowRun{})
		},
	})

	_, err := c.ListRuns("", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("expected no query params, got %s", gotQuery)
	}
}

// ---------- GetEvents ----------

func TestGetEvents(t *testing.T) {
	var gotPath string

	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/runs/{id}/events": func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			writeJSON(w, http.StatusOK, []Event{
				{ID: 1, EventType: "workflow_started", WorkflowRunID: "run-1"},
				{ID: 2, EventType: "activity_started", WorkflowRunID: "run-1"},
			})
		},
	})

	events, err := c.GetEvents("run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/runs/run-1/events" {
		t.Errorf("expected path /api/runs/run-1/events, got %s", gotPath)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].EventType != "workflow_started" {
		t.Errorf("expected event type workflow_started, got %s", events[0].EventType)
	}
}

// ---------- CancelRun ----------

func TestCancelRun(t *testing.T) {
	var gotMethod, gotPath string

	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"POST /api/runs/{id}/cancel": func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		},
	})

	err := c.CancelRun("run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/api/runs/run-1/cancel" {
		t.Errorf("expected path /api/runs/run-1/cancel, got %s", gotPath)
	}
}

// ---------- Stats ----------

func TestStats(t *testing.T) {
	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/stats": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, StatsResponse{
				Counts: map[string]int{
					"pending":   3,
					"running":   1,
					"completed": 10,
				},
			})
		},
	})

	stats, err := c.Stats()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Counts["pending"] != 3 {
		t.Errorf("expected pending=3, got %d", stats.Counts["pending"])
	}
	if stats.Counts["completed"] != 10 {
		t.Errorf("expected completed=10, got %d", stats.Counts["completed"])
	}
}

// ---------- Health ----------

func TestHealth(t *testing.T) {
	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /health": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	err := c.Health()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------- Error scenarios ----------

func TestAPIError500(t *testing.T) {
	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/workflows": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, struct {
				Error string `json:"error"`
			}{Error: "database error"})
		},
	})

	_, err := c.ListWorkflows()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 500 {
		t.Errorf("expected status 500, got %d", apiErr.StatusCode)
	}
	if apiErr.Message != "database error" {
		t.Errorf("expected message 'database error', got %q", apiErr.Message)
	}
}

func TestAPIError404(t *testing.T) {
	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/workflows/{name}": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, struct {
				Error string `json:"error"`
			}{Error: "workflow not found"})
		},
	})

	_, err := c.GetWorkflow("nonexistent")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 404 {
		t.Errorf("expected status 404, got %d", apiErr.StatusCode)
	}
	if apiErr.Message != "workflow not found" {
		t.Errorf("expected message 'workflow not found', got %q", apiErr.Message)
	}
}

func TestAPIErrorPlainText(t *testing.T) {
	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/stats": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("bad gateway"))
		},
	})

	_, err := c.Stats()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 502 {
		t.Errorf("expected status 502, got %d", apiErr.StatusCode)
	}
	if apiErr.Message != "bad gateway" {
		t.Errorf("expected message 'bad gateway', got %q", apiErr.Message)
	}
}

func TestMalformedJSON(t *testing.T) {
	_, c := newTestServer(t, map[string]http.HandlerFunc{
		"GET /api/workflows": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{invalid json"))
		},
	})

	_, err := c.ListWorkflows()
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	// Should NOT be an APIError — it's a decode error
	if _, ok := err.(*APIError); ok {
		t.Error("expected non-APIError for malformed JSON")
	}
}

// ---------- NewClient default ----------

func TestNewClientDefault(t *testing.T) {
	c := NewClient("")
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("expected default baseURL http://localhost:8080, got %s", c.baseURL)
	}
}

func TestNewClientCustomURL(t *testing.T) {
	c := NewClient("http://example.com:9090")
	if c.baseURL != "http://example.com:9090" {
		t.Errorf("expected baseURL http://example.com:9090, got %s", c.baseURL)
	}
}
