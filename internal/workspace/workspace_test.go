package workspace

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/michaelhabib/orchestra/internal/config"
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

func TestInit_CreatesStructure(t *testing.T) {
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ws, err := Init(testConfig())
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
	state, err := ws.ReadState()
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
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ws, _ := Init(testConfig())
	state := &State{
		Project: "rtt",
		Teams: map[string]TeamState{
			"x": {Status: "done", ResultSummary: "built it", CostUSD: 1.5},
		},
	}
	if err := ws.WriteState(state); err != nil {
		t.Fatal(err)
	}
	got, err := ws.ReadState()
	if err != nil {
		t.Fatal(err)
	}
	if got.Teams["x"].CostUSD != 1.5 {
		t.Fatalf("expected 1.5, got %f", got.Teams["x"].CostUSD)
	}
}

func TestResult_RoundTrip(t *testing.T) {
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ws, _ := Init(testConfig())
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
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ws, _ := Init(testConfig())

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := "alpha"
			if n%2 == 0 {
				name = "beta"
			}
			ws.UpdateTeamState(name, TeamState{Status: "done", CostUSD: float64(n)})
		}(i)
	}
	wg.Wait()

	state, err := ws.ReadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Teams["alpha"].Status != "done" || state.Teams["beta"].Status != "done" {
		t.Fatal("expected both teams done")
	}
}

func TestUpdateRegistryEntry(t *testing.T) {
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ws, _ := Init(testConfig())

	err := ws.UpdateRegistryEntry("alpha", func(e *RegistryEntry) {
		e.Status = "running"
		e.SessionID = "abc-123"
	})
	if err != nil {
		t.Fatal(err)
	}

	reg, _ := ws.ReadRegistry()
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
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ws, _ := Init(testConfig())
	err := ws.UpdateRegistryEntry("nonexistent", func(e *RegistryEntry) {})
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
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	ws, _ := Init(testConfig())
	w, err := ws.LogWriter("alpha")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("test log"))
	w.Close()

	data, err := os.ReadFile(filepath.Join(ws.Path, "logs", "alpha.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "test log" {
		t.Fatalf("expected 'test log', got %q", string(data))
	}
}
