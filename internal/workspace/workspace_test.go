package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/itsHabib/orchestra/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Name: "test-project",
		Agents: []config.Agent{
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

func TestEnsure_CreatesStructure(t *testing.T) {
	chdirTemp(t)

	ws, err := Ensure(".orchestra")
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
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
}

func TestSeedRegistry(t *testing.T) {
	chdirTemp(t)

	ws, err := Ensure(".orchestra")
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if err := ws.SeedRegistry(testConfig()); err != nil {
		t.Fatalf("SeedRegistry failed: %v", err)
	}

	// Check registry.json
	reg, err := ws.ReadRegistry()
	if err != nil {
		t.Fatalf("ReadRegistry failed: %v", err)
	}
	if len(reg.Agents) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(reg.Agents))
	}
}

func TestResult_RoundTrip(t *testing.T) {
	chdirTemp(t)

	ws, err := Ensure(".orchestra")
	if err != nil {
		t.Fatal(err)
	}
	r := &AgentResult{
		Agent:   "alpha",
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

func TestResult_LegacyTeamFieldOnRead(t *testing.T) {
	chdirTemp(t)

	ws, err := Ensure(".orchestra")
	if err != nil {
		t.Fatal(err)
	}
	// Drop a v2-shaped results file directly so the parser sees the legacy
	// `team` key without a corresponding `agent`.
	legacy := []byte(`{"team":"legacy","status":"success","result":"ok"}`)
	if err := os.MkdirAll(filepath.Join(ws.Path, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "results", "legacy.json"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ws.ReadResult("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "legacy" {
		t.Fatalf("Agent = %q, want legacy", got.Agent)
	}
}

func TestRegistry_LegacyTeamsKeyOnRead(t *testing.T) {
	chdirTemp(t)

	ws, err := Ensure(".orchestra")
	if err != nil {
		t.Fatal(err)
	}
	// v2-shaped registry.json with `teams` instead of `agents`.
	legacy := []byte(`{"project":"p","teams":[{"name":"alpha","status":"pending"}]}`)
	if err := os.WriteFile(filepath.Join(ws.Path, "registry.json"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ws.ReadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Agents) != 1 || reg.Agents[0].Name != "alpha" {
		t.Fatalf("Agents = %+v, want one entry named alpha", reg.Agents)
	}
}

func TestUpdateRegistryEntry(t *testing.T) {
	chdirTemp(t)

	ws, err := Ensure(".orchestra")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.SeedRegistry(testConfig()); err != nil {
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
	for _, entry := range reg.Agents {
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

	ws, err := Ensure(".orchestra")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.SeedRegistry(testConfig()); err != nil {
		t.Fatal(err)
	}
	err = ws.UpdateRegistryEntry("nonexistent", func(_ *RegistryEntry) {})
	if err == nil {
		t.Fatal("expected error for unknown agent")
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

	ws, err := Ensure(".orchestra")
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
