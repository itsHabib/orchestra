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
