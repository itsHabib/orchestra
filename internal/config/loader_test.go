package config

import (
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

	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if cfg.Name != "test-project" {
		t.Fatalf("expected test-project, got %s", cfg.Name)
	}
	if len(cfg.Teams) != 2 {
		t.Fatalf("expected 2 teams, got %d", len(cfg.Teams))
	}
	if cfg.Teams[0].Lead.Model != "sonnet" {
		t.Fatalf("expected sonnet override, got %s", cfg.Teams[0].Lead.Model)
	}
	// Frontend should have inherited the default model (opus)
	if cfg.Teams[1].Lead.Model != "opus" {
		t.Fatalf("expected opus default, got %s", cfg.Teams[1].Lead.Model)
	}
	if cfg.Teams[0].Tasks[0].Summary != "Build API" {
		t.Fatalf("expected Build API, got %s", cfg.Teams[0].Tasks[0].Summary)
	}
	if len(cfg.Teams[0].Tasks[0].Deliverables) != 1 {
		t.Fatalf("expected 1 deliverable, got %d", len(cfg.Teams[0].Tasks[0].Deliverables))
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(":::invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, _, err := Load("/nonexistent/file.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
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
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "project name") {
		t.Fatalf("unexpected error: %v", err)
	}
}
