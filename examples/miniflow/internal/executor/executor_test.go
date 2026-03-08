package executor

import (
	"fmt"
	"testing"
	"time"

	"miniflow/internal/model"
)

// mockStore is an in-memory implementation of RunStore for testing.
type mockStore struct {
	runs         map[string]*model.WorkflowRun
	workflows    map[string]*model.Workflow
	activityRuns map[string][]model.ActivityRun // keyed by workflow run ID
	events       []model.Event
}

func newMockStore() *mockStore {
	return &mockStore{
		runs:         make(map[string]*model.WorkflowRun),
		workflows:    make(map[string]*model.Workflow),
		activityRuns: make(map[string][]model.ActivityRun),
	}
}

func (m *mockStore) GetRun(id string) (*model.WorkflowRun, error) {
	r, ok := m.runs[id]
	if !ok {
		return nil, fmt.Errorf("run %q not found", id)
	}
	// Return a copy so mutations in the executor are only persisted via UpdateRun.
	cp := *r
	return &cp, nil
}

func (m *mockStore) UpdateRun(r *model.WorkflowRun) error {
	cp := *r
	m.runs[r.ID] = &cp
	return nil
}

func (m *mockStore) GetWorkflow(id string) (*model.Workflow, error) {
	w, ok := m.workflows[id]
	if !ok {
		return nil, fmt.Errorf("workflow %q not found", id)
	}
	return w, nil
}

func (m *mockStore) CreateActivityRun(ar *model.ActivityRun) error {
	cp := *ar
	m.activityRuns[ar.WorkflowRunID] = append(m.activityRuns[ar.WorkflowRunID], cp)
	return nil
}

func (m *mockStore) GetActivityRunsByRunID(runID string) ([]model.ActivityRun, error) {
	return m.activityRuns[runID], nil
}

func (m *mockStore) AppendEvent(e *model.Event) error {
	m.events = append(m.events, *e)
	return nil
}

func (m *mockStore) ListRuns(status string, limit int) ([]model.WorkflowRun, error) {
	var result []model.WorkflowRun
	for _, r := range m.runs {
		if r.Status == status {
			result = append(result, *r)
			if len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

// --- helpers ---

func twoStepWorkflow() *model.Workflow {
	return &model.Workflow{
		ID:   "wf-1",
		Name: "two-step",
		Definition: model.WorkflowDefinition{
			Activities: []model.ActivitySpec{
				{Name: "step-a", Type: "http", InputExpr: "$.input", TimeoutSeconds: 30, MaxRetries: 1},
				{Name: "step-b", Type: "http", InputExpr: "$.steps[0].output", TimeoutSeconds: 30, MaxRetries: 0},
			},
		},
		CreatedAt: time.Now().UTC(),
	}
}

func pendingRun() *model.WorkflowRun {
	return &model.WorkflowRun{
		ID:          "run-1",
		WorkflowID:  "wf-1",
		Status:      model.StatusRunning,
		Input:       `{"url":"https://example.com"}`,
		CurrentStep: 0,
		CreatedAt:   time.Now().UTC(),
	}
}

// --- tests ---

func TestAdvanceRun_SchedulesFirstActivity(t *testing.T) {
	store := newMockStore()
	store.workflows["wf-1"] = twoStepWorkflow()
	store.runs["run-1"] = pendingRun()

	exec := NewExecutor(store)
	if err := exec.AdvanceRun("run-1"); err != nil {
		t.Fatalf("AdvanceRun: %v", err)
	}

	ars := store.activityRuns["run-1"]
	if len(ars) != 1 {
		t.Fatalf("expected 1 activity run, got %d", len(ars))
	}
	ar := ars[0]
	if ar.ActivityName != "step-a" {
		t.Errorf("expected activity name %q, got %q", "step-a", ar.ActivityName)
	}
	if ar.StepIndex != 0 {
		t.Errorf("expected step index 0, got %d", ar.StepIndex)
	}
	if ar.Status != model.StatusPending {
		t.Errorf("expected status %q, got %q", model.StatusPending, ar.Status)
	}
	if ar.Input != `{"url":"https://example.com"}` {
		t.Errorf("expected input to be run input, got %q", ar.Input)
	}

	// Should have emitted an activity_scheduled event.
	found := false
	for _, ev := range store.events {
		if ev.EventType == model.EventActivityScheduled {
			found = true
		}
	}
	if !found {
		t.Error("expected activity_scheduled event")
	}
}

func TestAdvanceRun_AdvancesToNextStep(t *testing.T) {
	store := newMockStore()
	store.workflows["wf-1"] = twoStepWorkflow()
	run := pendingRun()
	run.CurrentStep = 0
	store.runs["run-1"] = run

	// Simulate first activity completed.
	store.activityRuns["run-1"] = []model.ActivityRun{
		{
			ID:            "ar-1",
			WorkflowRunID: "run-1",
			ActivityName:  "step-a",
			StepIndex:     0,
			Status:        model.StatusCompleted,
			Input:         `{"url":"https://example.com"}`,
			Output:        `{"body":"hello"}`,
			Attempts:      1,
		},
	}

	exec := NewExecutor(store)
	if err := exec.AdvanceRun("run-1"); err != nil {
		t.Fatalf("AdvanceRun: %v", err)
	}

	// Run should have advanced to step 1.
	updatedRun := store.runs["run-1"]
	if updatedRun.CurrentStep != 1 {
		t.Errorf("expected current_step=1, got %d", updatedRun.CurrentStep)
	}

	// Second activity should be scheduled.
	ars := store.activityRuns["run-1"]
	if len(ars) != 2 {
		t.Fatalf("expected 2 activity runs, got %d", len(ars))
	}
	ar := ars[1]
	if ar.ActivityName != "step-b" {
		t.Errorf("expected activity name %q, got %q", "step-b", ar.ActivityName)
	}
	if ar.StepIndex != 1 {
		t.Errorf("expected step index 1, got %d", ar.StepIndex)
	}
	// Input should be resolved from $.steps[0].output.
	if ar.Input != `{"body":"hello"}` {
		t.Errorf("expected input from step 0 output, got %q", ar.Input)
	}
}

func TestAdvanceRun_CompletesWorkflow(t *testing.T) {
	store := newMockStore()
	store.workflows["wf-1"] = twoStepWorkflow()
	run := pendingRun()
	run.CurrentStep = 1
	store.runs["run-1"] = run

	// Both activities completed.
	store.activityRuns["run-1"] = []model.ActivityRun{
		{
			ID:            "ar-1",
			WorkflowRunID: "run-1",
			ActivityName:  "step-a",
			StepIndex:     0,
			Status:        model.StatusCompleted,
			Output:        `{"body":"hello"}`,
			Attempts:      1,
		},
		{
			ID:            "ar-2",
			WorkflowRunID: "run-1",
			ActivityName:  "step-b",
			StepIndex:     1,
			Status:        model.StatusCompleted,
			Output:        `{"result":"done"}`,
			Attempts:      1,
		},
	}

	exec := NewExecutor(store)
	if err := exec.AdvanceRun("run-1"); err != nil {
		t.Fatalf("AdvanceRun: %v", err)
	}

	updatedRun := store.runs["run-1"]
	if updatedRun.Status != model.StatusCompleted {
		t.Errorf("expected status %q, got %q", model.StatusCompleted, updatedRun.Status)
	}
	if updatedRun.Output != `{"result":"done"}` {
		t.Errorf("expected output from last activity, got %q", updatedRun.Output)
	}
	if updatedRun.CompletedAt == nil {
		t.Error("expected completed_at to be set")
	}

	// Should have workflow_completed event.
	found := false
	for _, ev := range store.events {
		if ev.EventType == model.EventWorkflowCompleted {
			found = true
		}
	}
	if !found {
		t.Error("expected workflow_completed event")
	}
}

func TestAdvanceRun_FailsWorkflow(t *testing.T) {
	store := newMockStore()
	store.workflows["wf-1"] = twoStepWorkflow()
	run := pendingRun()
	run.CurrentStep = 0
	store.runs["run-1"] = run

	// First activity failed with max retries exhausted (MaxRetries=1, Attempts=2).
	store.activityRuns["run-1"] = []model.ActivityRun{
		{
			ID:            "ar-1",
			WorkflowRunID: "run-1",
			ActivityName:  "step-a",
			StepIndex:     0,
			Status:        model.StatusFailed,
			Attempts:      2,
			MaxRetries:    1,
		},
	}

	exec := NewExecutor(store)
	if err := exec.AdvanceRun("run-1"); err != nil {
		t.Fatalf("AdvanceRun: %v", err)
	}

	updatedRun := store.runs["run-1"]
	if updatedRun.Status != model.StatusFailed {
		t.Errorf("expected status %q, got %q", model.StatusFailed, updatedRun.Status)
	}
	if updatedRun.CompletedAt == nil {
		t.Error("expected completed_at to be set")
	}

	// Should have workflow_failed event.
	found := false
	for _, ev := range store.events {
		if ev.EventType == model.EventWorkflowFailed {
			found = true
		}
	}
	if !found {
		t.Error("expected workflow_failed event")
	}
}

func TestResolveInputExpr(t *testing.T) {
	run := &model.WorkflowRun{
		Input: `{"key":"value"}`,
	}
	activityRuns := []model.ActivityRun{
		{StepIndex: 0, Output: `{"out0":"a"}`},
		{StepIndex: 1, Output: `{"out1":"b"}`},
	}

	tests := []struct {
		name string
		expr string
		want string
	}{
		{"dollar input", "$.input", `{"key":"value"}`},
		{"step 0 output", "$.steps[0].output", `{"out0":"a"}`},
		{"step 1 output", "$.steps[1].output", `{"out1":"b"}`},
		{"missing step", "$.steps[99].output", ""},
		{"empty expr", "", `{"key":"value"}`},
		{"unknown expr", "something.else", `{"key":"value"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveInputExpr(tt.expr, run, activityRuns)
			if got != tt.want {
				t.Errorf("resolveInputExpr(%q) = %q, want %q", tt.expr, got, tt.want)
			}
		})
	}
}

func TestStartRun(t *testing.T) {
	store := newMockStore()
	store.workflows["wf-1"] = twoStepWorkflow()
	run := &model.WorkflowRun{
		ID:          "run-1",
		WorkflowID:  "wf-1",
		Status:      model.StatusPending,
		Input:       `{"url":"https://example.com"}`,
		CurrentStep: 0,
		CreatedAt:   time.Now().UTC(),
	}
	store.runs["run-1"] = run

	exec := NewExecutor(store)
	if err := exec.StartRun("run-1"); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	updatedRun := store.runs["run-1"]
	if updatedRun.Status != model.StatusRunning {
		t.Errorf("expected status %q, got %q", model.StatusRunning, updatedRun.Status)
	}
	if updatedRun.StartedAt == nil {
		t.Error("expected started_at to be set")
	}

	// Should have workflow_started event.
	foundStarted := false
	for _, ev := range store.events {
		if ev.EventType == model.EventWorkflowStarted {
			foundStarted = true
		}
	}
	if !foundStarted {
		t.Error("expected workflow_started event")
	}

	// StartRun should also call AdvanceRun, scheduling the first activity.
	ars := store.activityRuns["run-1"]
	if len(ars) != 1 {
		t.Fatalf("expected 1 activity run after StartRun, got %d", len(ars))
	}
	if ars[0].ActivityName != "step-a" {
		t.Errorf("expected first activity %q, got %q", "step-a", ars[0].ActivityName)
	}
}
