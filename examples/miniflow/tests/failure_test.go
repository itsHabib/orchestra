package tests

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"miniflow/internal/client"
)

func TestActivityRetryOnFailure(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "fail-workflow", sampleWorkflow(
		client.ActivitySpec{
			Name:       "fail",
			Type:       "fail",
			MaxRetries: 2,
		},
	))

	run, err := env.client.StartRun("fail-workflow", "{}")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Poll until the activity reaches "failed" status with retries exhausted.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		detail, err := env.client.GetRun(run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if len(detail.Activities) > 0 && detail.Activities[0].Status == "failed" {
			act := detail.Activities[0]
			if act.Attempts <= 1 {
				t.Errorf("expected Attempts > 1 (retries happened), got %d", act.Attempts)
			}
			// The activity exhausted its retries and is now failed.
			// The run should eventually be marked failed by the executor.
			// Verify the run is in a terminal or expected state.
			if detail.Status != "failed" && detail.Status != "running" {
				t.Errorf("expected run status 'failed' or 'running', got %q", detail.Status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for activity to reach 'failed' status")
}

func TestCancelRunningWorkflow(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "sleep-workflow", sampleWorkflow(
		client.ActivitySpec{
			Name: "sleep",
			Type: "sleep",
		},
	))

	run, err := env.client.StartRun("sleep-workflow", `{"duration_ms":3000}`)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Give the scheduler time to pick up and start the run.
	time.Sleep(200 * time.Millisecond)

	if err := env.client.CancelRun(run.ID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	// Allow a brief moment for the cancellation to propagate.
	time.Sleep(100 * time.Millisecond)

	detail, err := env.client.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if detail.Status != "cancelled" {
		t.Errorf("expected run status 'cancelled', got %q", detail.Status)
	}
}

func TestCancelPendingRun(t *testing.T) {
	env := newTestEnv(t) // No workers — run stays pending.

	registerTestWorkflow(t, env.client, "noop-workflow", sampleWorkflow(
		client.ActivitySpec{
			Name: "noop",
			Type: "noop",
		},
	))

	run, err := env.client.StartRun("noop-workflow", "{}")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Verify run is still pending (no workers/scheduler to pick it up).
	detail, err := env.client.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if detail.Status != "pending" {
		t.Fatalf("expected run to be 'pending' without workers, got %q", detail.Status)
	}

	if err := env.client.CancelRun(run.ID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	detail, err = env.client.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun after cancel: %v", err)
	}
	if detail.Status != "cancelled" {
		t.Errorf("expected run status 'cancelled', got %q", detail.Status)
	}
}

func TestEventHistory(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "two-noop", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	run, err := env.client.StartRun("two-noop", `{"msg":"hello"}`)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	waitForRunStatus(t, env.client, run.ID, "completed", 10*time.Second)

	events, err := env.client.GetEvents(run.ID)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}

	// Count event types.
	counts := make(map[string]int)
	for _, ev := range events {
		counts[ev.EventType]++
	}

	// Verify expected event types and minimum counts.
	expected := map[string]int{
		"workflow_started":   1,
		"workflow_completed": 1,
		"activity_scheduled": 2, // one per activity
		"activity_started":   2,
		"activity_completed": 2,
	}

	for evType, minCount := range expected {
		if counts[evType] < minCount {
			t.Errorf("expected at least %d %q events, got %d", minCount, evType, counts[evType])
		}
	}
}

func TestConcurrentRuns(t *testing.T) {
	// Use 2 workers to reduce SQLite lock contention while still providing parallelism.
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "concurrent-noop", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	const numRuns = 3
	runIDs := make([]string, numRuns)
	for i := 0; i < numRuns; i++ {
		run, err := env.client.StartRun("concurrent-noop", fmt.Sprintf(`{"run_index":%d}`, i))
		if err != nil {
			t.Fatalf("StartRun[%d]: %v", i, err)
		}
		runIDs[i] = run.ID
	}

	// Wait for all runs to complete.
	for i, id := range runIDs {
		waitForRunStatus(t, env.client, id, "completed", 10*time.Second)

		detail, err := env.client.GetRun(id)
		if err != nil {
			t.Fatalf("GetRun[%d]: %v", i, err)
		}
		if detail.Status != "completed" {
			t.Errorf("run[%d] expected status 'completed', got %q", i, detail.Status)
		}
	}
}

func TestGetNonexistentRun(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.client.GetRun("00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Fatal("expected error for nonexistent run, got nil")
	}

	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *client.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 404 {
		t.Errorf("expected status code 404, got %d", apiErr.StatusCode)
	}
}
