package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_ValidYAML(t *testing.T) {
	yaml := `
name: test-project
defaults:
  model: opus
  max_turns: 100
teams:
  - name: backend
    lead:
      role: "Backend Lead"
      model: sonnet
    context: |
      Go 1.22, Chi router
    tasks:
      - summary: "Build API"
        details: "REST endpoints"
        deliverables: ["src/api/"]
        verify: "go build ./..."
      - summary: "Build DB"
        details: "Postgres schema"
        deliverables: ["migrations/"]
        verify: "go test ./..."
  - name: frontend
    depends_on: [backend]
    lead:
      role: "Frontend Lead"
    tasks:
      - summary: "Build UI"
        details: "React components"
        deliverables: ["src/components/"]
        verify: "npm run build"
      - summary: "Build routing"
        details: "React router"
        verify: "npm run build"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Valid() {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	// Legacy `teams:` key produces one deprecation warning; that's the
	// only warning expected.
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Message, "deprecated") {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	cfg := res.Config
	if cfg == nil {
		t.Fatal("Config is nil on a valid result")
	}
	if cfg.Name != "test-project" {
		t.Fatalf("expected test-project, got %s", cfg.Name)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(cfg.Agents))
	}
	if !cfg.LegacyTeamsKey {
		t.Fatal("YAML used `teams:`, expected LegacyTeamsKey=true")
	}
	if cfg.Agents[0].Lead.Model != "sonnet" {
		t.Fatalf("expected sonnet override, got %s", cfg.Agents[0].Lead.Model)
	}
	// Frontend should have inherited the default model (opus)
	if cfg.Agents[1].Lead.Model != "opus" {
		t.Fatalf("expected opus default, got %s", cfg.Agents[1].Lead.Model)
	}
	if cfg.Agents[0].Tasks[0].Summary != "Build API" {
		t.Fatalf("expected Build API, got %s", cfg.Agents[0].Tasks[0].Summary)
	}
	if len(cfg.Agents[0].Tasks[0].Deliverables) != 1 {
		t.Fatalf("expected 1 deliverable, got %d", len(cfg.Agents[0].Tasks[0].Deliverables))
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(":::invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	if res != nil {
		t.Fatalf("expected nil result on parse error, got %+v", res)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	res, err := Load("/nonexistent/file.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if res != nil {
		t.Fatalf("expected nil result on I/O error, got %+v", res)
	}
}

// TestLoad_RelativeFilePathsResolveAgainstConfigDir locks in the codex/copilot
// PR #36 fix: relative `files.path` entries must canonicalize against the
// directory containing the YAML file, not the process CWD. Without this, an
// `orchestra run /path/to/orchestra.yaml` invocation from a different working
// directory either uploads the wrong file or fails with "no such file".
func TestLoad_RelativeFilePathsResolveAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()

	// A real file the relative path will resolve to. We don't actually
	// open it during Load — Load just rewrites the path string — but
	// having it on disk asserts the resolved path is meaningful.
	specPath := filepath.Join(dir, "spec.md")
	if err := os.WriteFile(specPath, []byte("the spec"), 0o600); err != nil {
		t.Fatalf("seed spec: %v", err)
	}

	yaml := `
name: test
agents:
  - name: designer
    lead:
      role: "Designer"
    tasks:
      - summary: "Design"
        details: "from spec"
        verify: "true"
    files:
      - path: ./spec.md
        mount: /workspace/spec.md
      - path: nested/data.csv
      - path: ` + filepath.ToSlash(filepath.Join(dir, "absolute.json")) + `
`
	path := filepath.Join(dir, "orchestra.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Load from a different CWD to make sure config-dir resolution is what
	// matters, not the process's working directory.
	otherDir := t.TempDir()
	prevCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(otherDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevCWD) })

	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config == nil {
		t.Fatalf("Config nil; errors: %+v", res.Errors)
	}

	got := res.Config.Agents[0].Files
	if len(got) != 3 {
		t.Fatalf("expected 3 files, got %d", len(got))
	}

	wantSpec := filepath.Join(dir, "spec.md")
	if got[0].Path != wantSpec {
		t.Errorf("relative ./spec.md: got %q, want %q", got[0].Path, wantSpec)
	}
	wantNested := filepath.Join(dir, "nested", "data.csv")
	if got[1].Path != wantNested {
		t.Errorf("relative nested/data.csv: got %q, want %q", got[1].Path, wantNested)
	}
	wantAbs := filepath.Join(dir, "absolute.json")
	if got[2].Path != wantAbs {
		t.Errorf("absolute path should round-trip unchanged: got %q, want %q", got[2].Path, wantAbs)
	}
}

// TestLoad_FilesValidation exercises the structural file-mount checks
// added to validateResourceShape — empty path, non-absolute container
// mount, and the local-backend warning. Mirrors how skills /
// custom_tools are validated.
func TestLoad_FilesValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Run("empty path → error", func(t *testing.T) {
		assertFilesValidationErr(t, dir, "empty-path.yaml", filesYAMLEmptyPath, "empty path")
	})
	t.Run("non-absolute mount → error", func(t *testing.T) {
		assertFilesValidationErr(t, dir, "rel-mount.yaml", filesYAMLRelMount, "absolute container path")
	})
	t.Run("local backend → warning", func(t *testing.T) { assertFilesValidationWarn(t, dir, "local-warn.yaml", filesYAMLLocal) })
}

const filesYAMLEmptyPath = `
name: t
backend:
  kind: managed_agents
agents:
  - name: a
    lead: {role: A}
    tasks:
      - {summary: x, details: d, verify: v}
    files:
      - mount: /workspace/x
`

const filesYAMLRelMount = `
name: t
backend:
  kind: managed_agents
agents:
  - name: a
    lead: {role: A}
    tasks:
      - {summary: x, details: d, verify: v}
    files:
      - {path: ./spec.md, mount: workspace/spec.md}
`

const filesYAMLLocal = `
name: t
backend:
  kind: local
agents:
  - name: a
    lead: {role: A}
    tasks:
      - {summary: x, details: d, verify: v}
    files:
      - {path: ./spec.md}
`

func assertFilesValidationErr(t *testing.T, dir, name, yaml, wantSubstr string) {
	t.Helper()
	res, err := Load(writeYAML(t, dir, name, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Valid() {
		t.Fatalf("expected validation error containing %q", wantSubstr)
	}
	if !strings.Contains(res.Err().Error(), wantSubstr) {
		t.Fatalf("err = %v, want %q", res.Err(), wantSubstr)
	}
}

func assertFilesValidationWarn(t *testing.T, dir, name, yaml string) {
	t.Helper()
	res, err := Load(writeYAML(t, dir, name, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !res.Valid() {
		t.Fatalf("local-backend file mounts should warn, not error: %v", res.Errors)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "files are not supported under backend.kind=local") {
			return
		}
	}
	t.Fatalf("expected local-backend file warning; got %+v", res.Warnings)
}

func writeYAML(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoad_ValidationError(t *testing.T) {
	yaml := `
name: ""
teams: []
`
	dir := t.TempDir()
	path := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned an error for a structural validation failure (validation issues should live in res.Errors, not error): %v", err)
	}
	if res.Valid() {
		t.Fatal("expected validation errors")
	}
	if !errors.Is(res.Err(), ErrInvalidConfig) {
		t.Fatalf("res.Err() does not wrap ErrInvalidConfig: %v", res.Err())
	}
	if !strings.Contains(res.Err().Error(), "project name") {
		t.Fatalf("unexpected err: %v", res.Err())
	}
	if res.Config != nil {
		t.Fatalf("Config should be nil on invalid result, got %+v", res.Config)
	}
}
