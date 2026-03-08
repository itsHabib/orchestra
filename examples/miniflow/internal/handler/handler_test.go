package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"miniflow/internal/executor"
	"miniflow/internal/handler"
	"miniflow/internal/model"
	"miniflow/internal/store"
)

func setup(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	s, err := store.NewStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ex := executor.NewExecutor(s)
	return handler.New(s, ex), s
}

func TestHealth(t *testing.T) {
	h, _ := setup(t)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Fatalf("expected ok, got %s", body["status"])
	}
}

func createWorkflow(t *testing.T, h http.Handler) model.Workflow {
	t.Helper()
	body := `{"name":"test-wf","definition":{"activities":[{"name":"step1","type":"noop","input_expr":"$.input","timeout_seconds":10,"max_retries":2}]}}`
	req := httptest.NewRequest("POST", "/api/workflows", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create workflow: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var wf model.Workflow
	json.NewDecoder(w.Body).Decode(&wf)
	return wf
}

func TestCreateAndGetWorkflow(t *testing.T) {
	h, _ := setup(t)
	wf := createWorkflow(t, h)
	if wf.Name != "test-wf" {
		t.Fatalf("expected test-wf, got %s", wf.Name)
	}

	// Get by name
	req := httptest.NewRequest("GET", "/api/workflows/test-wf", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("get workflow: expected 200, got %d", w.Code)
	}
}

func TestListWorkflows(t *testing.T) {
	h, _ := setup(t)
	createWorkflow(t, h)

	req := httptest.NewRequest("GET", "/api/workflows", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var wfs []model.Workflow
	json.NewDecoder(w.Body).Decode(&wfs)
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
}

func TestDuplicateWorkflow(t *testing.T) {
	h, _ := setup(t)
	createWorkflow(t, h)

	body := `{"name":"test-wf","definition":{"activities":[]}}`
	req := httptest.NewRequest("POST", "/api/workflows", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestWorkflowNotFound(t *testing.T) {
	h, _ := setup(t)
	req := httptest.NewRequest("GET", "/api/workflows/nonexistent", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestStartRun(t *testing.T) {
	h, _ := setup(t)
	createWorkflow(t, h)

	body := `{"input":"{\"key\":\"value\"}"}`
	req := httptest.NewRequest("POST", "/api/workflows/test-wf/run", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var run model.WorkflowRun
	json.NewDecoder(w.Body).Decode(&run)
	if run.Status != "pending" {
		t.Fatalf("expected pending, got %s", run.Status)
	}
}

func TestListRuns(t *testing.T) {
	h, _ := setup(t)
	createWorkflow(t, h)
	// Create a run
	req := httptest.NewRequest("POST", "/api/workflows/test-wf/run", bytes.NewBufferString(`{"input":"{}"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	req = httptest.NewRequest("GET", "/api/runs?status=pending&limit=10", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var runs []model.WorkflowRun
	json.NewDecoder(w.Body).Decode(&runs)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
}

func TestGetRun(t *testing.T) {
	h, _ := setup(t)
	createWorkflow(t, h)

	req := httptest.NewRequest("POST", "/api/workflows/test-wf/run", bytes.NewBufferString(`{"input":"{}"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var run model.WorkflowRun
	json.NewDecoder(w.Body).Decode(&run)

	req = httptest.NewRequest("GET", "/api/runs/"+run.ID, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetRunNotFound(t *testing.T) {
	h, _ := setup(t)
	req := httptest.NewRequest("GET", "/api/runs/nonexistent", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListEvents(t *testing.T) {
	h, _ := setup(t)
	createWorkflow(t, h)

	req := httptest.NewRequest("POST", "/api/workflows/test-wf/run", bytes.NewBufferString(`{"input":"{}"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var run model.WorkflowRun
	json.NewDecoder(w.Body).Decode(&run)

	req = httptest.NewRequest("GET", "/api/runs/"+run.ID+"/events", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestCancelRun(t *testing.T) {
	h, s := setup(t)
	createWorkflow(t, h)

	// Create and start a run
	req := httptest.NewRequest("POST", "/api/workflows/test-wf/run", bytes.NewBufferString(`{"input":"{}"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var run model.WorkflowRun
	json.NewDecoder(w.Body).Decode(&run)

	// Set to running so cancel works
	run.Status = model.StatusRunning
	s.UpdateRun(&run)

	req = httptest.NewRequest("POST", "/api/runs/"+run.ID+"/cancel", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestStats(t *testing.T) {
	h, _ := setup(t)
	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}
