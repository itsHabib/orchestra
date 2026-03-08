package activity

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"miniflow/internal/model"
)

// mockStore implements ActivityStore for testing.
type mockStore struct {
	mu          sync.Mutex
	activities  []*model.ActivityRun
	events      []*model.Event
	claimed     map[string]bool
	reclaimable bool // if true, activities set back to pending can be re-claimed
}

func newMockStore() *mockStore {
	return &mockStore{
		claimed: make(map[string]bool),
	}
}

func (m *mockStore) AddActivity(ar *model.ActivityRun) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activities = append(m.activities, ar)
}

func (m *mockStore) ClaimNextActivity() (*model.ActivityRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ar := range m.activities {
		if ar.Status == model.StatusPending && !m.claimed[ar.ID] {
			m.claimed[ar.ID] = true
			return ar, nil
		}
	}
	return nil, nil
}

func (m *mockStore) UpdateActivityRun(ar *model.ActivityRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// If set back to pending and reclaimable, allow re-claiming.
	if ar.Status == model.StatusPending && m.reclaimable {
		delete(m.claimed, ar.ID)
	}
	return nil
}

func (m *mockStore) AppendEvent(e *model.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

func (m *mockStore) getActivity(id string) *model.ActivityRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ar := range m.activities {
		if ar.ID == id {
			return ar
		}
	}
	return nil
}

func (m *mockStore) getEvents() []*model.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]*model.Event, len(m.events))
	copy(cp, m.events)
	return cp
}

// --- Registry tests ---

func TestRegistry_ExecuteUnknown(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(context.Background(), "nonexistent", "")
	if err == nil {
		t.Fatal("expected error for unregistered activity, got nil")
	}
}

// --- Builtin tests ---

func TestBuiltinNoop(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)
	out, err := r.Execute(context.Background(), "noop", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello" {
		t.Fatalf("expected 'hello', got %q", out)
	}
}

func TestBuiltinSleep(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)
	out, err := r.Execute(context.Background(), "sleep", `{"duration_ms": 10}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result struct {
		Slept int `json:"slept"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid output json: %v", err)
	}
	if result.Slept != 10 {
		t.Fatalf("expected slept=10, got %d", result.Slept)
	}
}

func TestBuiltinLog(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)
	out, err := r.Execute(context.Background(), "log", `{"message": "test msg"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result struct {
		Logged bool `json:"logged"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid output json: %v", err)
	}
	if !result.Logged {
		t.Fatal("expected logged=true")
	}
}

func TestBuiltinFail(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)
	_, err := r.Execute(context.Background(), "fail", "")
	if err == nil {
		t.Fatal("expected error from fail handler, got nil")
	}
	if err.Error() != "activity failed" {
		t.Fatalf("expected 'activity failed', got %q", err.Error())
	}
}

// --- Worker tests ---

func TestWorkerSuccessfulExecution(t *testing.T) {
	store := newMockStore()
	reg := NewRegistry()
	RegisterBuiltins(reg)

	ar := &model.ActivityRun{
		ID:             "ar-1",
		WorkflowRunID:  "wr-1",
		ActivityName:   "step-1",
		ActivityType:   "noop",
		Status:         model.StatusPending,
		Input:          "test-input",
		MaxRetries:     3,
		TimeoutSeconds: 10,
		CreatedAt:      time.Now(),
	}
	store.AddActivity(ar)

	wp := NewWorkerPool(store, reg, 1, 10*time.Millisecond)
	wp.Start()
	time.Sleep(200 * time.Millisecond)
	wp.Stop()

	act := store.getActivity("ar-1")
	if act.Status != model.StatusCompleted {
		t.Fatalf("expected status completed, got %s", act.Status)
	}
	if act.Output != "test-input" {
		t.Fatalf("expected output 'test-input', got %q", act.Output)
	}
	if act.Attempts != 1 {
		t.Fatalf("expected attempts=1, got %d", act.Attempts)
	}

	events := store.getEvents()
	hasStarted := false
	hasCompleted := false
	for _, e := range events {
		if e.EventType == model.EventActivityStarted {
			hasStarted = true
		}
		if e.EventType == model.EventActivityCompleted {
			hasCompleted = true
		}
	}
	if !hasStarted {
		t.Fatal("expected activity_started event")
	}
	if !hasCompleted {
		t.Fatal("expected activity_completed event")
	}
}

func TestWorkerFailureAndRetry(t *testing.T) {
	store := newMockStore()
	reg := NewRegistry()
	RegisterBuiltins(reg)

	ar := &model.ActivityRun{
		ID:             "ar-2",
		WorkflowRunID:  "wr-2",
		ActivityName:   "step-2",
		ActivityType:   "fail",
		Status:         model.StatusPending,
		Input:          "",
		Attempts:       0,
		MaxRetries:     3,
		TimeoutSeconds: 10,
		CreatedAt:      time.Now(),
	}
	store.AddActivity(ar)

	wp := NewWorkerPool(store, reg, 1, 10*time.Millisecond)
	wp.Start()
	time.Sleep(200 * time.Millisecond)
	wp.Stop()

	act := store.getActivity("ar-2")
	if act.Status != model.StatusPending {
		t.Fatalf("expected status pending (for retry), got %s", act.Status)
	}
	if act.Attempts != 1 {
		t.Fatalf("expected attempts=1 after first failure, got %d", act.Attempts)
	}

	events := store.getEvents()
	hasRetried := false
	for _, e := range events {
		if e.EventType == model.EventActivityRetried {
			hasRetried = true
		}
	}
	if !hasRetried {
		t.Fatal("expected activity_retried event")
	}
}

func TestWorkerMaxRetriesExhausted(t *testing.T) {
	store := newMockStore()
	reg := NewRegistry()
	RegisterBuiltins(reg)

	ar := &model.ActivityRun{
		ID:             "ar-3",
		WorkflowRunID:  "wr-3",
		ActivityName:   "step-3",
		ActivityType:   "fail",
		Status:         model.StatusPending,
		Input:          "",
		Attempts:       2,
		MaxRetries:     3,
		TimeoutSeconds: 10,
		CreatedAt:      time.Now(),
	}
	store.AddActivity(ar)

	wp := NewWorkerPool(store, reg, 1, 10*time.Millisecond)
	wp.Start()
	time.Sleep(200 * time.Millisecond)
	wp.Stop()

	act := store.getActivity("ar-3")
	if act.Status != model.StatusFailed {
		t.Fatalf("expected status failed, got %s", act.Status)
	}
	if act.Attempts != 3 {
		t.Fatalf("expected attempts=3, got %d", act.Attempts)
	}

	events := store.getEvents()
	hasFailed := false
	for _, e := range events {
		if e.EventType == model.EventActivityFailed {
			hasFailed = true
		}
	}
	if !hasFailed {
		t.Fatal("expected activity_failed event")
	}
}
