package tests

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"miniflow/internal/activity"
	"miniflow/internal/client"
	"miniflow/internal/executor"
	"miniflow/internal/handler"
	"miniflow/internal/store"
)

// testEnv holds all components needed for integration tests.
type testEnv struct {
	store      *store.Store
	handler    http.Handler
	server     *httptest.Server
	client     *client.Client
	executor   *executor.Executor
	scheduler  *executor.Scheduler
	workerPool *activity.WorkerPool
}

// newTestEnv creates a test environment with store, executor, handler, httptest
// server, and client. No workers or scheduler are started.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	ex := executor.NewExecutor(s)
	h := handler.New(s, ex)
	srv := httptest.NewServer(h)
	c := client.NewClient(srv.URL)

	env := &testEnv{
		store:    s,
		handler:  h,
		server:   srv,
		client:   c,
		executor: ex,
	}

	t.Cleanup(func() {
		srv.Close()
		s.Close()
	})

	return env
}

// newTestEnvWithWorkers creates a test environment with workers and scheduler running.
func newTestEnvWithWorkers(t *testing.T, count int) *testEnv {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	ex := executor.NewExecutor(s)
	h := handler.New(s, ex)
	srv := httptest.NewServer(h)
	c := client.NewClient(srv.URL)

	registry := activity.NewRegistry()
	activity.RegisterBuiltins(registry)

	wp := activity.NewWorkerPool(s, registry, count, 50*time.Millisecond)
	wp.Start()

	sched := executor.NewScheduler(ex, s, 50*time.Millisecond)
	sched.Start()

	env := &testEnv{
		store:      s,
		handler:    h,
		server:     srv,
		client:     c,
		executor:   ex,
		scheduler:  sched,
		workerPool: wp,
	}

	t.Cleanup(func() {
		sched.Stop()
		wp.Stop()
		srv.Close()
		s.Close()
	})

	return env
}

// registerTestWorkflow registers a workflow via the client, fataling on error.
func registerTestWorkflow(t *testing.T, c *client.Client, name string, activities []client.ActivitySpec) *client.Workflow {
	t.Helper()
	wf, err := c.RegisterWorkflow(name, client.WorkflowDefinition{
		Activities: activities,
	})
	if err != nil {
		t.Fatalf("failed to register workflow %q: %v", name, err)
	}
	return wf
}

// waitForRunStatus polls GetRun every 50ms until the run reaches the expected
// status or the timeout expires.
func waitForRunStatus(t *testing.T, c *client.Client, runID string, status string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		run, err := c.GetRun(runID)
		if err != nil {
			t.Fatalf("failed to get run %s: %v", runID, err)
		}
		if run.Status == status {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for run %s to reach status %q", runID, status)
}

// sampleWorkflow is a convenience function that returns the given activity specs as a slice.
func sampleWorkflow(specs ...client.ActivitySpec) []client.ActivitySpec {
	return specs
}
