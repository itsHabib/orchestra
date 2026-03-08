package tests

import (
	"testing"
	"time"

	"miniflow/internal/client"
)

func TestClientRegisterWorkflow(t *testing.T) {
	env := newTestEnv(t)

	wf := registerTestWorkflow(t, env.client, "client-reg-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	if wf.Name != "client-reg-wf" {
		t.Errorf("workflow name = %q, want %q", wf.Name, "client-reg-wf")
	}
	if wf.ID == "" {
		t.Error("workflow ID is empty")
	}
}

func TestClientStartAndGetRun(t *testing.T) {
	env := newTestEnv(t)

	wf := registerTestWorkflow(t, env.client, "client-run-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	run, err := env.client.StartRun("client-run-wf", "{}")
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	detail, err := env.client.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}

	if detail.WorkflowID != wf.ID {
		t.Errorf("run workflow_id = %q, want %q", detail.WorkflowID, wf.ID)
	}
	if detail.Status != "pending" && detail.Status != "running" {
		t.Errorf("run status = %q, want pending or running", detail.Status)
	}
}

func TestClientListRuns(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "client-list-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	var runIDs []string
	for i := 0; i < 3; i++ {
		run, err := env.client.StartRun("client-list-wf", "{}")
		if err != nil {
			t.Fatalf("StartRun %d failed: %v", i, err)
		}
		runIDs = append(runIDs, run.ID)
	}

	for _, id := range runIDs {
		waitForRunStatus(t, env.client, id, "completed", 5*time.Second)
	}

	runs, err := env.client.ListRuns("completed", 0)
	if err != nil {
		t.Fatalf("ListRuns failed: %v", err)
	}
	if len(runs) < 3 {
		t.Errorf("expected at least 3 completed runs, got %d", len(runs))
	}
}

func TestClientGetEvents(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "client-events-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	run, err := env.client.StartRun("client-events-wf", "{}")
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	waitForRunStatus(t, env.client, run.ID, "completed", 5*time.Second)

	events, err := env.client.GetEvents(run.ID)
	if err != nil {
		t.Fatalf("GetEvents failed: %v", err)
	}

	hasStarted := false
	hasCompleted := false
	for _, ev := range events {
		if ev.EventType == "workflow_started" {
			hasStarted = true
		}
		if ev.EventType == "workflow_completed" {
			hasCompleted = true
		}
	}
	if !hasStarted {
		t.Error("events do not include workflow_started")
	}
	if !hasCompleted {
		t.Error("events do not include workflow_completed")
	}
}

func TestClientCancelRun(t *testing.T) {
	env := newTestEnv(t) // no workers, run stays pending

	registerTestWorkflow(t, env.client, "client-cancel-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	run, err := env.client.StartRun("client-cancel-wf", "{}")
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	err = env.client.CancelRun(run.ID)
	if err != nil {
		t.Fatalf("CancelRun failed: %v", err)
	}

	detail, err := env.client.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if detail.Status != "cancelled" {
		t.Errorf("run status = %q, want %q", detail.Status, "cancelled")
	}
}

func TestClientStats(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "client-stats-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	run, err := env.client.StartRun("client-stats-wf", "{}")
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	waitForRunStatus(t, env.client, run.ID, "completed", 5*time.Second)

	stats, err := env.client.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if stats.Counts["completed"] < 1 {
		t.Errorf("expected completed count >= 1, got %d", stats.Counts["completed"])
	}
}
