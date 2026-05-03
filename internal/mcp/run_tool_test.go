package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/itsHabib/orchestra/internal/config"
)

func TestHandleRun_RejectsBothInlineAndPath(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, _, err := srv.handleRun(context.Background(), nil, RunArgs{
		InlineDAG:  &InlineDAG{Agents: []InlineAgent{{Name: "a", Role: "r", Prompt: "p"}}},
		ConfigPath: "/tmp/foo.yaml",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError when both inline_dag and config_path are set")
	}
}

func TestHandleRun_RejectsNeither(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, _, err := srv.handleRun(context.Background(), nil, RunArgs{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError when neither set")
	}
}

func TestHandleRun_InlineDAG_HappyPath(t *testing.T) {
	t.Parallel()

	sp := &stubSpawner{}
	srv := newTestServer(t, sp, stateReaderFn(nil))

	res, out, err := srv.handleRun(context.Background(), nil, RunArgs{
		InlineDAG: &InlineDAG{
			ProjectName: "demo",
			Backend:     "local",
			Agents: []InlineAgent{
				{Name: "design", Role: "designer", Prompt: "spec out X"},
				{Name: "build", Role: "engineer", Prompt: "build X", Deps: []string{"design"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if out.RunID == "" || out.WorkspaceDir == "" {
		t.Fatalf("RunResult missing fields: %+v", out)
	}
	if len(sp.calls) != 1 {
		t.Fatalf("spawner calls: got %d, want 1", len(sp.calls))
	}
	yamlPath := sp.calls[0].YAMLPath
	if _, err := os.Stat(yamlPath); err != nil {
		t.Fatalf("yaml not written: %v", err)
	}
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	var loaded config.Config
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	if loaded.Name != "demo" {
		t.Fatalf("name: got %q, want %q", loaded.Name, "demo")
	}
	if len(loaded.Agents) != 2 {
		t.Fatalf("teams: %d", len(loaded.Agents))
	}
	if loaded.Agents[1].DependsOn[0] != "design" {
		t.Fatalf("deps not preserved: %+v", loaded.Agents[1])
	}
	all, err := srv.Registry().List(context.Background())
	if err != nil {
		t.Fatalf("Registry.List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("registry size: got %d, want 1", len(all))
	}
}

func TestHandleRun_InlineDAG_RejectsEmptyTeams(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, _, err := srv.handleRun(context.Background(), nil, RunArgs{
		InlineDAG: &InlineDAG{ProjectName: "x"},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on empty teams")
	}
}

func TestHandleRun_InlineDAG_MissingFieldRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		team InlineAgent
		want string
	}{
		{"missing name", InlineAgent{Role: "r", Prompt: "p"}, "name"},
		{"missing role", InlineAgent{Name: "a", Prompt: "p"}, "role"},
		{"missing prompt", InlineAgent{Name: "a", Role: "r"}, "prompt"},
	}
	for _, tc := range cases {
		srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
		res, _, err := srv.handleRun(context.Background(), nil, RunArgs{
			InlineDAG: &InlineDAG{Agents: []InlineAgent{tc.team}},
		})
		if err != nil {
			t.Fatalf("%s: handler error: %v", tc.name, err)
		}
		if !res.IsError {
			t.Fatalf("%s: expected IsError", tc.name)
		}
		if !strings.Contains(resultText(res), tc.want) {
			t.Fatalf("%s: error text %q missing %q", tc.name, resultText(res), tc.want)
		}
	}
}

// TestInlineDAG_AcceptsLegacyTeamsKey verifies the v2 → v3 alias on the
// MCP wire: clients on the legacy schema keep producing valid runs.
func TestInlineDAG_AcceptsLegacyTeamsKey(t *testing.T) {
	payload := []byte(`{"project_name":"demo","backend":"local","teams":[{"name":"a","role":"r","prompt":"p"}]}`)
	var dag InlineDAG
	if err := dag.UnmarshalJSON(payload); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if len(dag.Agents) != 1 || dag.Agents[0].Name != "a" {
		t.Fatalf("Agents = %+v", dag.Agents)
	}
}

// TestInlineDAG_RejectsBothKeysEvenWhenOneIsEmpty pins the strict dual-key
// guard on the MCP `run` tool. The previous implementation silently
// treated `agents: []` plus `teams: [...]` as legacy input — that masked
// migration bugs in clients. After v3 the parser fails fast.
func TestInlineDAG_RejectsBothKeysEvenWhenOneIsEmpty(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"agents":[],"teams":[{"name":"a","role":"r","prompt":"p"}]}`),
		[]byte(`{"agents":[{"name":"a","role":"r","prompt":"p"}],"teams":[]}`),
		[]byte(`{"agents":[],"teams":[]}`),
	}
	for i, p := range payloads {
		var dag InlineDAG
		if err := dag.UnmarshalJSON(p); err == nil {
			t.Errorf("case %d: expected error for dual-key payload %s", i, p)
		}
	}
}

func TestHandleRun_ConfigPath_LoadsExistingYAML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "orchestra.yaml")
	yamlBody := []byte(`
name: from-disk
backend: local
teams:
  - name: solo
    lead:
      role: builder
    tasks:
      - summary: do the thing
`)
	if err := os.WriteFile(cfgPath, yamlBody, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	sp := &stubSpawner{}
	srv := newTestServer(t, sp, stateReaderFn(nil))

	res, out, err := srv.handleRun(context.Background(), nil, RunArgs{
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if out.RunID == "" {
		t.Fatalf("missing run_id")
	}
	if len(sp.calls) != 1 {
		t.Fatalf("spawner calls: %d", len(sp.calls))
	}
}

func TestHandleRun_ConfigPath_RejectsRelative(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, _, err := srv.handleRun(context.Background(), nil, RunArgs{
		ConfigPath: "relative/path.yaml",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on relative path")
	}
	if !strings.Contains(resultText(res), "absolute") {
		t.Fatalf("error text: %q", resultText(res))
	}
}

func TestHandleRun_ConfigPath_RejectsMissingFile(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	missing := filepath.Join(t.TempDir(), "nope.yaml")
	res, _, err := srv.handleRun(context.Background(), nil, RunArgs{
		ConfigPath: missing,
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on missing file")
	}
}

func TestHandleRun_ProjectNameOverridesInlineDAG(t *testing.T) {
	t.Parallel()

	sp := &stubSpawner{}
	srv := newTestServer(t, sp, stateReaderFn(nil))

	_, _, err := srv.handleRun(context.Background(), nil, RunArgs{
		InlineDAG: &InlineDAG{
			ProjectName: "from-dag",
			Agents:      []InlineAgent{{Name: "a", Role: "r", Prompt: "p"}},
		},
		ProjectName: "override",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	data, err := os.ReadFile(sp.calls[0].YAMLPath)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	if cfg.Name != "override" {
		t.Fatalf("name: got %q, want %q", cfg.Name, "override")
	}
}
