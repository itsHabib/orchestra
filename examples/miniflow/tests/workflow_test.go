package tests

import (
	"strings"
	"testing"
	"time"

	"miniflow/internal/client"
)

func TestRegisterAndListWorkflows(t *testing.T) {
	env := newTestEnv(t)

	registerTestWorkflow(t, env.client, "workflow-a", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))
	registerTestWorkflow(t, env.client, "workflow-b", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
		client.ActivitySpec{Name: "log", Type: "log"},
	))

	workflows, err := env.client.ListWorkflows()
	if err != nil {
		t.Fatalf("ListWorkflows failed: %v", err)
	}
	if len(workflows) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(workflows))
	}

	names := map[string]bool{}
	for _, wf := range workflows {
		names[wf.Name] = true
	}
	if !names["workflow-a"] {
		t.Error("workflow-a not found in list")
	}
	if !names["workflow-b"] {
		t.Error("workflow-b not found in list")
	}
}

func TestSimpleWorkflowExecution(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "simple-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	run, err := env.client.StartRun("simple-wf", "{}")
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	waitForRunStatus(t, env.client, run.ID, "completed", 5*time.Second)

	detail, err := env.client.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if len(detail.Activities) != 2 {
		t.Fatalf("expected 2 activities, got %d", len(detail.Activities))
	}
	for i, act := range detail.Activities {
		if act.Status != "completed" {
			t.Errorf("activity %d status = %q, want %q", i, act.Status, "completed")
		}
	}
}

func TestWorkflowWithDataPassing(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "data-passing-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop", InputExpr: "$.input"},
		client.ActivitySpec{Name: "noop", Type: "noop", InputExpr: "$.steps[0].output"},
	))

	run, err := env.client.StartRun("data-passing-wf", `{"value":"hello"}`)
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	waitForRunStatus(t, env.client, run.ID, "completed", 5*time.Second)

	detail, err := env.client.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if len(detail.Activities) != 2 {
		t.Fatalf("expected 2 activities, got %d", len(detail.Activities))
	}
	// noop passes input through as output, so step2's output should contain "hello"
	if !strings.Contains(detail.Activities[1].Output, "hello") {
		t.Errorf("step2 output %q does not contain %q", detail.Activities[1].Output, "hello")
	}
}

func TestMultiStepWorkflow(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "multi-step-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
		client.ActivitySpec{Name: "sleep", Type: "sleep"},
		client.ActivitySpec{Name: "log", Type: "log"},
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	// Provide input that satisfies sleep (duration_ms) and log (message)
	run, err := env.client.StartRun("multi-step-wf", `{"duration_ms":10,"message":"test"}`)
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	waitForRunStatus(t, env.client, run.ID, "completed", 10*time.Second)

	detail, err := env.client.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if len(detail.Activities) != 4 {
		t.Fatalf("expected 4 activities, got %d", len(detail.Activities))
	}
	for i, act := range detail.Activities {
		if act.Status != "completed" {
			t.Errorf("activity %d (%s) status = %q, want %q", i, act.ActivityName, act.Status, "completed")
		}
	}
}

func TestRunWithInput(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "input-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	customInput := `{"key":"custom_value"}`
	run, err := env.client.StartRun("input-wf", customInput)
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	waitForRunStatus(t, env.client, run.ID, "completed", 5*time.Second)

	detail, err := env.client.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if !strings.Contains(detail.Input, "custom_value") {
		t.Errorf("run input %q does not contain %q", detail.Input, "custom_value")
	}
}

func TestListRunsFiltered(t *testing.T) {
	env := newTestEnvWithWorkers(t, 2)

	registerTestWorkflow(t, env.client, "list-runs-wf", sampleWorkflow(
		client.ActivitySpec{Name: "noop", Type: "noop"},
	))

	var runIDs []string
	for i := 0; i < 3; i++ {
		run, err := env.client.StartRun("list-runs-wf", "{}")
		if err != nil {
			t.Fatalf("StartRun %d failed: %v", i, err)
		}
		runIDs = append(runIDs, run.ID)
	}

	for _, id := range runIDs {
		waitForRunStatus(t, env.client, id, "completed", 5*time.Second)
	}

	// List all runs
	allRuns, err := env.client.ListRuns("", 0)
	if err != nil {
		t.Fatalf("ListRuns (all) failed: %v", err)
	}
	if len(allRuns) < 3 {
		t.Errorf("expected at least 3 runs, got %d", len(allRuns))
	}

	// List completed runs
	completedRuns, err := env.client.ListRuns("completed", 0)
	if err != nil {
		t.Fatalf("ListRuns (completed) failed: %v", err)
	}
	if len(completedRuns) < 3 {
		t.Errorf("expected at least 3 completed runs, got %d", len(completedRuns))
	}

	// List pending runs (should be 0 since all completed)
	pendingRuns, err := env.client.ListRuns("pending", 0)
	if err != nil {
		t.Fatalf("ListRuns (pending) failed: %v", err)
	}
	if len(pendingRuns) != 0 {
		t.Errorf("expected 0 pending runs, got %d", len(pendingRuns))
	}
}
