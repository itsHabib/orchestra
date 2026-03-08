package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"miniflow/internal/model"

	"github.com/google/uuid"
)

// newTestStore creates an in-memory Store for testing.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedWorkflow creates and inserts a workflow, returning it.
func seedWorkflow(t *testing.T, s *Store) *model.Workflow {
	t.Helper()
	w := &model.Workflow{
		ID:   uuid.NewString(),
		Name: "test-workflow-" + uuid.NewString()[:8],
		Definition: model.WorkflowDefinition{
			Activities: []model.ActivitySpec{
				{Name: "step1", Type: "http", InputExpr: "$.input", TimeoutSeconds: 30, MaxRetries: 3},
				{Name: "step2", Type: "grpc", InputExpr: "$.step1", TimeoutSeconds: 60, MaxRetries: 1},
			},
		},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.CreateWorkflow(w); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	return w
}

// seedRun creates and inserts a workflow run, returning it.
func seedRun(t *testing.T, s *Store, workflowID, status string) *model.WorkflowRun {
	t.Helper()
	r := &model.WorkflowRun{
		ID:         uuid.NewString(),
		WorkflowID: workflowID,
		Status:     status,
		Input:      `{"key":"value"}`,
		Output:     "",
		CreatedAt:  time.Now().UTC().Truncate(time.Second),
	}
	if err := s.CreateRun(r); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return r
}

// seedActivityRun creates and inserts an activity run, returning it.
func seedActivityRun(t *testing.T, s *Store, runID string, stepIndex int, status string) *model.ActivityRun {
	t.Helper()
	ar := &model.ActivityRun{
		ID:             uuid.NewString(),
		WorkflowRunID:  runID,
		ActivityName:   "activity-" + uuid.NewString()[:8],
		StepIndex:      stepIndex,
		Status:         status,
		Input:          `{"x":1}`,
		Output:         "",
		Attempts:       0,
		MaxRetries:     3,
		TimeoutSeconds: 30,
		CreatedAt:      time.Now().UTC().Truncate(time.Second),
	}
	if err := s.CreateActivityRun(ar); err != nil {
		t.Fatalf("CreateActivityRun: %v", err)
	}
	return ar
}

// -----------------------------------------------------------------------
// Workflow tests
// -----------------------------------------------------------------------

func TestCreateAndGetWorkflow(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)

	got, err := s.GetWorkflow(w.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}

	if got.ID != w.ID || got.Name != w.Name {
		t.Errorf("got ID=%s Name=%s, want ID=%s Name=%s", got.ID, got.Name, w.ID, w.Name)
	}
	if len(got.Definition.Activities) != 2 {
		t.Fatalf("expected 2 activities, got %d", len(got.Definition.Activities))
	}
	if got.Definition.Activities[0].Name != "step1" {
		t.Errorf("activity[0].Name = %s, want step1", got.Definition.Activities[0].Name)
	}

	// Also test GetWorkflowByName
	byName, err := s.GetWorkflowByName(w.Name)
	if err != nil {
		t.Fatalf("GetWorkflowByName: %v", err)
	}
	if byName.ID != w.ID {
		t.Errorf("GetWorkflowByName returned ID=%s, want %s", byName.ID, w.ID)
	}
}

func TestListWorkflows(t *testing.T) {
	s := newTestStore(t)
	seedWorkflow(t, s)
	seedWorkflow(t, s)

	wfs, err := s.ListWorkflows()
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(wfs))
	}
}

func TestDuplicateWorkflow(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)

	dup := &model.Workflow{
		ID:         uuid.NewString(),
		Name:       w.Name, // same name
		Definition: w.Definition,
		CreatedAt:  time.Now().UTC(),
	}
	err := s.CreateWorkflow(dup)
	if !errors.Is(err, ErrDuplicateWorkflow) {
		t.Fatalf("expected ErrDuplicateWorkflow, got: %v", err)
	}
}

func TestGetWorkflowNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetWorkflow("nonexistent-id")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// -----------------------------------------------------------------------
// WorkflowRun tests
// -----------------------------------------------------------------------

func TestCreateAndGetRun(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)
	r := seedRun(t, s, w.ID, model.StatusPending)

	got, err := s.GetRun(r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != r.ID || got.WorkflowID != w.ID || got.Status != model.StatusPending {
		t.Errorf("unexpected run: %+v", got)
	}
	if got.Input != `{"key":"value"}` {
		t.Errorf("got Input=%s, want {\"key\":\"value\"}", got.Input)
	}
	if got.StartedAt != nil {
		t.Errorf("expected StartedAt nil, got %v", got.StartedAt)
	}
}

func TestListRuns(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)
	seedRun(t, s, w.ID, model.StatusPending)
	seedRun(t, s, w.ID, model.StatusRunning)
	seedRun(t, s, w.ID, model.StatusCompleted)

	// All runs
	all, err := s.ListRuns("", 0)
	if err != nil {
		t.Fatalf("ListRuns all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(all))
	}

	// Filter by status
	pending, err := s.ListRuns(model.StatusPending, 0)
	if err != nil {
		t.Fatalf("ListRuns pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending run, got %d", len(pending))
	}

	// Test limit
	limited, err := s.ListRuns("", 2)
	if err != nil {
		t.Fatalf("ListRuns limit: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("expected 2 runs with limit, got %d", len(limited))
	}
}

func TestUpdateRun(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)
	r := seedRun(t, s, w.ID, model.StatusPending)

	now := time.Now().UTC().Truncate(time.Second)
	r.Status = model.StatusRunning
	r.StartedAt = &now
	r.CurrentStep = 1

	if err := s.UpdateRun(r); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := s.GetRun(r.ID)
	if err != nil {
		t.Fatalf("GetRun after update: %v", err)
	}
	if got.Status != model.StatusRunning {
		t.Errorf("status = %s, want running", got.Status)
	}
	if got.CurrentStep != 1 {
		t.Errorf("current_step = %d, want 1", got.CurrentStep)
	}
	if got.StartedAt == nil {
		t.Fatal("StartedAt should not be nil after update")
	}
}

// -----------------------------------------------------------------------
// ActivityRun tests
// -----------------------------------------------------------------------

func TestCreateAndGetActivityRun(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)
	r := seedRun(t, s, w.ID, model.StatusRunning)
	ar := seedActivityRun(t, s, r.ID, 0, model.StatusPending)

	got, err := s.GetActivityRun(ar.ID)
	if err != nil {
		t.Fatalf("GetActivityRun: %v", err)
	}
	if got.ID != ar.ID || got.WorkflowRunID != r.ID {
		t.Errorf("unexpected activity run: %+v", got)
	}
	if got.StepIndex != 0 || got.Status != model.StatusPending {
		t.Errorf("step_index=%d status=%s", got.StepIndex, got.Status)
	}

	// Also test GetActivityRunsByRunID
	ars, err := s.GetActivityRunsByRunID(r.ID)
	if err != nil {
		t.Fatalf("GetActivityRunsByRunID: %v", err)
	}
	if len(ars) != 1 {
		t.Fatalf("expected 1 activity run, got %d", len(ars))
	}
}

func TestUpdateActivityRun(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)
	r := seedRun(t, s, w.ID, model.StatusRunning)
	ar := seedActivityRun(t, s, r.ID, 0, model.StatusPending)

	now := time.Now().UTC().Truncate(time.Second)
	ar.Status = model.StatusCompleted
	ar.Output = `{"result":"ok"}`
	ar.Attempts = 1
	ar.StartedAt = &now
	ar.CompletedAt = &now

	if err := s.UpdateActivityRun(ar); err != nil {
		t.Fatalf("UpdateActivityRun: %v", err)
	}

	got, err := s.GetActivityRun(ar.ID)
	if err != nil {
		t.Fatalf("GetActivityRun after update: %v", err)
	}
	if got.Status != model.StatusCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
	if got.Output != `{"result":"ok"}` {
		t.Errorf("output = %s", got.Output)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
}

// -----------------------------------------------------------------------
// ClaimNextActivity tests
// -----------------------------------------------------------------------

func TestClaimNextActivity(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)
	r := seedRun(t, s, w.ID, model.StatusRunning)

	// No pending activities — should return nil, nil.
	claimed, err := s.ClaimNextActivity()
	if err != nil {
		t.Fatalf("ClaimNextActivity (empty): %v", err)
	}
	if claimed != nil {
		t.Fatal("expected nil when no pending activities")
	}

	// Create two pending activities with a slight time gap.
	ar1 := seedActivityRun(t, s, r.ID, 0, model.StatusPending)
	time.Sleep(10 * time.Millisecond) // ensure distinct created_at
	seedActivityRun(t, s, r.ID, 1, model.StatusPending)

	// Claim should return the oldest (ar1).
	claimed, err = s.ClaimNextActivity()
	if err != nil {
		t.Fatalf("ClaimNextActivity: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected a claimed activity run")
	}
	if claimed.ID != ar1.ID {
		t.Errorf("claimed ID=%s, want %s (oldest)", claimed.ID, ar1.ID)
	}
	if claimed.Status != model.StatusRunning {
		t.Errorf("claimed status=%s, want running", claimed.Status)
	}
	if claimed.StartedAt == nil {
		t.Error("claimed StartedAt should be set")
	}

	// Verify it was persisted.
	persisted, _ := s.GetActivityRun(ar1.ID)
	if persisted.Status != model.StatusRunning {
		t.Errorf("persisted status=%s, want running", persisted.Status)
	}
}

func TestClaimNextActivityConcurrency(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)
	r := seedRun(t, s, w.ID, model.StatusRunning)

	const n = 10
	// Create n pending activity runs.
	ids := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		ar := seedActivityRun(t, s, r.ID, i, model.StatusPending)
		ids[ar.ID] = true
		time.Sleep(2 * time.Millisecond) // ensure ordering
	}

	// Launch n goroutines that all try to claim.
	var mu sync.Mutex
	claimed := make(map[string]bool)
	var wg sync.WaitGroup

	for i := 0; i < n*2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ar, err := s.ClaimNextActivity()
			if err != nil {
				// Contention errors are acceptable; just skip.
				return
			}
			if ar == nil {
				return
			}
			mu.Lock()
			if claimed[ar.ID] {
				t.Errorf("double claim: %s", ar.ID)
			}
			claimed[ar.ID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Verify: every claimed ID should be a real activity run and no doubles.
	for id := range claimed {
		if !ids[id] {
			t.Errorf("claimed unknown ID: %s", id)
		}
	}
	// All n should have been claimed (no lost claims).
	if len(claimed) != n {
		t.Errorf("claimed %d activities, want %d", len(claimed), n)
	}
}

// -----------------------------------------------------------------------
// Events tests
// -----------------------------------------------------------------------

func TestAppendAndListEvents(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)
	r := seedRun(t, s, w.ID, model.StatusRunning)

	e1 := &model.Event{
		WorkflowRunID: r.ID,
		EventType:     model.EventWorkflowStarted,
		Payload:       `{"msg":"started"}`,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
	}
	e2 := &model.Event{
		WorkflowRunID: r.ID,
		ActivityRunID: "some-activity-id",
		EventType:     model.EventActivityStarted,
		Payload:       `{}`,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
	}
	if err := s.AppendEvent(e1); err != nil {
		t.Fatalf("AppendEvent e1: %v", err)
	}
	if e1.ID == 0 {
		t.Error("expected e1.ID to be set after insert")
	}
	if err := s.AppendEvent(e2); err != nil {
		t.Fatalf("AppendEvent e2: %v", err)
	}

	events, err := s.ListEvents(r.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].EventType != model.EventWorkflowStarted {
		t.Errorf("event[0] type=%s", events[0].EventType)
	}
	if events[1].ActivityRunID != "some-activity-id" {
		t.Errorf("event[1] activity_run_id=%s", events[1].ActivityRunID)
	}
}

// -----------------------------------------------------------------------
// Stats tests
// -----------------------------------------------------------------------

func TestStats(t *testing.T) {
	s := newTestStore(t)
	w := seedWorkflow(t, s)

	seedRun(t, s, w.ID, model.StatusPending)
	seedRun(t, s, w.ID, model.StatusPending)
	seedRun(t, s, w.ID, model.StatusRunning)
	seedRun(t, s, w.ID, model.StatusCompleted)

	stats, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats[model.StatusPending] != 2 {
		t.Errorf("pending=%d, want 2", stats[model.StatusPending])
	}
	if stats[model.StatusRunning] != 1 {
		t.Errorf("running=%d, want 1", stats[model.StatusRunning])
	}
	if stats[model.StatusCompleted] != 1 {
		t.Errorf("completed=%d, want 1", stats[model.StatusCompleted])
	}
}
