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

// TestInlineDAG_PlumbsRequiresCredentials covers the §B1 inline-DAG
// extension: top-level requires_credentials must reach Defaults so the
// engine resolves the union under each agent's RequiredCredentials.
func TestInlineDAG_PlumbsRequiresCredentials(t *testing.T) {
	t.Parallel()
	dag := &InlineDAG{
		ProjectName:         "demo",
		RequiresCredentials: []string{"github_token", "polygon_api_key"},
		Agents: []InlineAgent{{
			Name: "a", Role: "r", Prompt: "p",
			RequiresCredentials: []string{"openai_api_key"},
		}},
	}
	cfg, err := dag.toConfig("")
	if err != nil {
		t.Fatalf("toConfig: %v", err)
	}
	cfg.ResolveDefaults()
	if got := cfg.Defaults.RequiresCredentials; len(got) != 2 || got[0] != "github_token" || got[1] != "polygon_api_key" {
		t.Fatalf("Defaults.RequiresCredentials = %v", got)
	}
	if got := cfg.Agents[0].RequiresCredentials; len(got) != 1 || got[0] != "openai_api_key" {
		t.Fatalf("Agents[0].RequiresCredentials = %v", got)
	}
	// The engine consumes the union via Agent.RequiredCredentials(Defaults).
	got := cfg.Agents[0].RequiredCredentials(&cfg.Defaults)
	want := []string{"github_token", "openai_api_key", "polygon_api_key"}
	if len(got) != len(want) {
		t.Fatalf("union = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("union[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestInlineDAG_PlumbsFiles covers shared + per-agent file mounts. Top-level
// files fan out onto every agent (mirrors the chat-side LLM's "every agent
// reads these inputs" mental model) and per-agent files extend that list.
func TestInlineDAG_PlumbsFiles(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	shared := filepath.Join(tmp, "shared.md")
	perAgent := filepath.Join(tmp, "per-agent.md")
	for _, p := range []string{shared, perAgent} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	dag := &InlineDAG{
		Files: []config.FileMount{{Path: shared, MountPath: "/workspace/shared.md"}},
		Agents: []InlineAgent{{
			Name: "a", Role: "r", Prompt: "p",
			Files: []config.FileMount{{Path: perAgent}},
		}},
	}
	cfg, err := dag.toConfig("")
	if err != nil {
		t.Fatalf("toConfig: %v", err)
	}
	got := cfg.Agents[0].Files
	if len(got) != 2 {
		t.Fatalf("Files = %v, want 2 entries", got)
	}
	if got[0].Path != shared || got[0].MountPath != "/workspace/shared.md" {
		t.Errorf("shared mount = %+v", got[0])
	}
	if got[1].Path != perAgent {
		t.Errorf("per-agent mount = %+v", got[1])
	}
}

// TestInlineDAG_RejectsRelativeFilePath enforces the absolute-path rule for
// inline file mounts. Relative paths have no source-yaml directory to
// canonicalize against on the inline path, so they fail fast at toConfig
// rather than producing a confusing file-not-found at upload time.
func TestInlineDAG_RejectsRelativeFilePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dag  *InlineDAG
		want string
	}{
		{
			name: "top-level relative",
			dag: &InlineDAG{
				Files:  []config.FileMount{{Path: "relative/path.md"}},
				Agents: []InlineAgent{{Name: "a", Role: "r", Prompt: "p"}},
			},
			want: "inline_dag.files[0].path must be absolute",
		},
		{
			name: "per-agent relative",
			dag: &InlineDAG{
				Agents: []InlineAgent{{
					Name: "a", Role: "r", Prompt: "p",
					Files: []config.FileMount{{Path: "relative/path.md"}},
				}},
			},
			want: "inline_dag.agents[0].files[0].path must be absolute",
		},
		{
			name: "empty path",
			dag: &InlineDAG{
				Files:  []config.FileMount{{Path: ""}},
				Agents: []InlineAgent{{Name: "a", Role: "r", Prompt: "p"}},
			},
			want: "inline_dag.files[0].path is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.dag.toConfig(""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

// TestInlineDAG_PlumbsEnvironmentOverride covers the per-agent repo
// substitution case (e.g. one synthesizer pushes to a different repo than the
// rest of the run).
func TestInlineDAG_PlumbsEnvironmentOverride(t *testing.T) {
	t.Parallel()
	override := &config.EnvironmentOverride{
		Repository: &config.RepositorySpec{URL: "https://github.com/other/repo"},
	}
	dag := &InlineDAG{
		Agents: []InlineAgent{{
			Name: "a", Role: "r", Prompt: "p",
			EnvironmentOverride: override,
		}},
	}
	cfg, err := dag.toConfig("")
	if err != nil {
		t.Fatalf("toConfig: %v", err)
	}
	got := cfg.Agents[0].EnvironmentOverride.Repository
	if got == nil || got.URL != "https://github.com/other/repo" {
		t.Fatalf("EnvironmentOverride.Repository = %+v, want override URL", got)
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

// TestHandleRun_InlineDAG_ExtendedFieldsRoundTrip exercises the full handler
// path: an inline_dag carrying requires_credentials, files, and a per-agent
// environment_override is folded into a config.Config, written to YAML, and
// the spawner sees the resolved shape. Locks in the §B1 substrate fix
// end-to-end.
func TestHandleRun_InlineDAG_ExtendedFieldsRoundTrip(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mountSrc := filepath.Join(tmp, "design.md")
	if err := os.WriteFile(mountSrc, []byte("doc"), 0o644); err != nil {
		t.Fatalf("seed mount: %v", err)
	}
	sp, loaded := runExtendedInlineDAG(t, mountSrc)
	if len(sp.calls) != 1 {
		t.Fatalf("spawner calls: %d", len(sp.calls))
	}
	assertExtendedFieldsLanded(t, loaded, mountSrc)
}

func runExtendedInlineDAG(t *testing.T, mountSrc string) (*stubSpawner, *config.Config) {
	t.Helper()
	sp := &stubSpawner{}
	srv := newTestServer(t, sp, stateReaderFn(nil))
	res, _, err := srv.handleRun(context.Background(), nil, RunArgs{
		InlineDAG: &InlineDAG{
			ProjectName:         "extended",
			Backend:             "local",
			RequiresCredentials: []string{"github_token"},
			Files:               []config.FileMount{{Path: mountSrc, MountPath: "/workspace/design.md"}},
			Agents: []InlineAgent{{
				Name: "solo", Role: "engineer", Prompt: "do it",
				RequiresCredentials: []string{"openai_api_key"},
				EnvironmentOverride: &config.EnvironmentOverride{
					Repository: &config.RepositorySpec{URL: "https://github.com/other/repo"},
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError: %s", resultText(res))
	}
	if len(sp.calls) == 0 {
		t.Fatalf("spawner not called")
	}
	data, err := os.ReadFile(sp.calls[0].YAMLPath)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	var loaded config.Config
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	return sp, &loaded
}

func assertExtendedFieldsLanded(t *testing.T, loaded *config.Config, mountSrc string) {
	t.Helper()
	if got := loaded.Defaults.RequiresCredentials; len(got) != 1 || got[0] != "github_token" {
		t.Errorf("Defaults.RequiresCredentials = %v", got)
	}
	if len(loaded.Agents) != 1 {
		t.Fatalf("agents = %d", len(loaded.Agents))
	}
	a := loaded.Agents[0]
	if got := a.RequiresCredentials; len(got) != 1 || got[0] != "openai_api_key" {
		t.Errorf("Agents[0].RequiresCredentials = %v", got)
	}
	if len(a.Files) != 1 || a.Files[0].Path != mountSrc {
		t.Errorf("Agents[0].Files = %+v", a.Files)
	}
	if a.EnvironmentOverride.Repository == nil || a.EnvironmentOverride.Repository.URL != "https://github.com/other/repo" {
		t.Errorf("Agents[0].EnvironmentOverride = %+v", a.EnvironmentOverride)
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
