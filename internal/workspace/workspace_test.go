package workspace

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/itsHabib/orchestra/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Name: "test-project",
		Teams: []config.Team{
			{Name: "alpha", Lead: config.Lead{Role: "Lead A"}},
			{Name: "beta", Lead: config.Lead{Role: "Lead B"}},
		},
	}
}

func chdirTemp(t *testing.T) {
	t.Helper()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origDir); err != nil {
			t.Fatal(err)
		}
	})
}

func TestInit_CreatesStructure(t *testing.T) {
	chdirTemp(t)

	ws, err := Init(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Check dirs exist
	for _, sub := range []string{"", "results", "logs"} {
		p := filepath.Join(ws.Path, sub)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("missing dir %s: %v", p, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", p)
		}
	}

	// Check state.json
	state, err := ws.ReadState(context.Background())
	if err != nil {
		t.Fatalf("ReadState failed: %v", err)
	}
	if state.Project != "test-project" {
		t.Fatalf("expected test-project, got %s", state.Project)
	}
	if len(state.Teams) != 2 {
		t.Fatalf("expected 2 teams in state, got %d", len(state.Teams))
	}
	if state.Teams["alpha"].Status != "pending" {
		t.Fatalf("expected pending, got %s", state.Teams["alpha"].Status)
	}

	// Check registry.json
	reg, err := ws.ReadRegistry()
	if err != nil {
		t.Fatalf("ReadRegistry failed: %v", err)
	}
	if len(reg.Teams) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(reg.Teams))
	}
}

func TestState_RoundTrip(t *testing.T) {
	chdirTemp(t)

	ws, err := Init(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	state := &State{
		Project: "rtt",
		Teams: map[string]TeamState{
			"x": {Status: "done", ResultSummary: "built it", CostUSD: 1.5},
		},
	}
	if err := ws.WriteState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	got, err := ws.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Teams["x"].CostUSD != 1.5 {
		t.Fatalf("expected 1.5, got %f", got.Teams["x"].CostUSD)
	}
}

func TestResult_RoundTrip(t *testing.T) {
	chdirTemp(t)

	ws, err := Init(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := &TeamResult{
		Team:    "alpha",
		Status:  "success",
		Result:  "done",
		CostUSD: 2.5,
	}
	if err := ws.WriteResult(r); err != nil {
		t.Fatal(err)
	}
	got, err := ws.ReadResult("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.CostUSD != 2.5 {
		t.Fatalf("expected 2.5, got %f", got.CostUSD)
	}
}

func TestUpdateTeamState_Concurrent(t *testing.T) {
	chdirTemp(t)

	ws, err := Init(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := "alpha"
			if n%2 == 0 {
				name = "beta"
			}
			if err := ws.UpdateTeamState(context.Background(), name, func(ts *TeamState) {
				*ts = TeamState{Status: "done", CostUSD: float64(n)}
			}); err != nil {
				t.Errorf("UpdateTeamState failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	state, err := ws.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Teams["alpha"].Status != "done" || state.Teams["beta"].Status != "done" {
		t.Fatal("expected both teams done")
	}
}

func TestUpdateRegistryEntry(t *testing.T) {
	chdirTemp(t)

	ws, err := Init(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}

	err = ws.UpdateRegistryEntry("alpha", func(e *RegistryEntry) {
		e.Status = "running"
		e.SessionID = "abc-123"
	})
	if err != nil {
		t.Fatal(err)
	}

	reg, err := ws.ReadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range reg.Teams {
		if entry.Name == "alpha" {
			if entry.Status != "running" || entry.SessionID != "abc-123" {
				t.Fatalf("unexpected entry: %+v", entry)
			}
			return
		}
	}
	t.Fatal("alpha not found in registry")
}

func TestUpdateRegistryEntry_NotFound(t *testing.T) {
	chdirTemp(t)

	ws, err := Init(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	err = ws.UpdateRegistryEntry("nonexistent", func(_ *RegistryEntry) {})
	if err == nil {
		t.Fatal("expected error for unknown team")
	}
}

func TestOpen_NonExistent(t *testing.T) {
	_, err := Open("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestLogWriter(t *testing.T) {
	chdirTemp(t)

	ws, err := Init(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	w, err := ws.LogWriter("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("test log")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(ws.Path, "logs", "alpha.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "test log" {
		t.Fatalf("expected 'test log', got %q", string(data))
	}
}
