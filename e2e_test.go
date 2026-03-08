package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2E_OrchestraRun runs a full orchestra run with a mock claude script.
// It validates: config parsing, DAG ordering, spawner with progress output,
// workspace state, results, and logs are all written correctly.
func TestE2E_OrchestraRun(t *testing.T) {
	// Build the orchestra binary
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "orchestra")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("failed to build orchestra: %v\n%s", err, out)
	}

	// Create a mock claude script that emits stream-json
	mockClaude := filepath.Join(binDir, "claude")
	mockScript := `#!/bin/bash
# Ignore all flags, just emit stream-json in the real claude format
sleep 0.1
echo '{"type":"system","subtype":"init","session_id":"sess-'$$'"}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"planning..."}]}}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Starting work on the task..."}]}}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/test.go"}}]}}'
echo '{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"go build ./..."}}]}}'
echo '{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"All tasks completed successfully."}]}}'
echo '{"type":"result","subtype":"success","result":"Completed all tasks.","total_cost_usd":0.25,"num_turns":3,"duration_ms":1500,"session_id":"sess-'$$'"}'
`
	if err := os.WriteFile(mockClaude, []byte(mockScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a minimal orchestra.yaml with a 2-tier DAG
	workDir := t.TempDir()
	configYAML := `name: "e2e-test"

defaults:
  model: sonnet
  max_turns: 10
  permission_mode: acceptEdits
  timeout_minutes: 5

teams:
  - name: backend
    lead:
      role: "Backend Lead"
    context: |
      Build a simple Go HTTP server.
    tasks:
      - summary: "Build HTTP server"
        details: "Create a basic HTTP server with health check endpoint."
        deliverables:
          - "main.go"
        verify: "go build ."

  - name: frontend
    lead:
      role: "Frontend Lead"
    context: |
      Build a simple React app.
    tasks:
      - summary: "Build React app"
        details: "Create a basic React app with routing."
        deliverables:
          - "src/App.tsx"
        verify: "npm run build"

  - name: integration
    depends_on: [backend, frontend]
    lead:
      role: "Integration Lead"
    context: |
      Write integration tests.
    tasks:
      - summary: "Write integration tests"
        details: "Test backend and frontend together."
        deliverables:
          - "tests/"
        verify: "go test ./..."
`
	configPath := filepath.Join(workDir, "orchestra.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run orchestra with the mock claude in PATH
	cmd := exec.Command(binPath, "run", configPath)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))

	out, err := cmd.CombinedOutput()
	output := string(out)
	t.Logf("orchestra output:\n%s", output)

	if err != nil {
		t.Fatalf("orchestra run failed: %v\n%s", err, output)
	}

	// ── Verify output contains expected progress messages ──
	checks := []string{
		"Workspace initialized",
		"DAG: 2 tiers",
		"Tier 0:",
		"backend",
		"frontend",
		"Tier 1:",
		"integration",
		"Starting Backend Lead",
		"Starting Frontend Lead",
		"Starting Integration Lead",
		"session started",
		"thinking",
		"Write →",
		"Bash →",
		"tool completed",
		"finished",
		"Done (turns:",
	}
	for _, check := range checks {
		if !strings.Contains(output, check) {
			t.Errorf("output missing expected string: %q", check)
		}
	}

	// ── Verify workspace files were created ──
	wsDir := filepath.Join(workDir, ".orchestra")
	for _, f := range []string{
		"state.json",
		"registry.json",
		"results/backend.json",
		"results/frontend.json",
		"results/integration.json",
		"logs/backend.log",
		"logs/frontend.log",
		"logs/integration.log",
	} {
		path := filepath.Join(wsDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected workspace file missing: %s", f)
		}
	}

	// ── Verify state.json shows all teams done ──
	stateData, err := os.ReadFile(filepath.Join(wsDir, "state.json"))
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}
	stateStr := string(stateData)
	if strings.Count(stateStr, `"done"`) != 3 {
		t.Errorf("expected 3 teams with status done, got:\n%s", stateStr)
	}

	// ── Verify log files are non-empty ──
	for _, team := range []string{"backend", "frontend", "integration"} {
		logData, err := os.ReadFile(filepath.Join(wsDir, "logs", team+".log"))
		if err != nil {
			t.Fatalf("reading %s.log: %v", team, err)
		}
		if len(logData) == 0 {
			t.Errorf("log file for %s is empty", team)
		}
	}
}

// TestE2E_OrchestraRun_TeamFailure verifies that a failed team stops the tier
// and reports the error.
func TestE2E_OrchestraRun_TeamFailure(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "orchestra")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("failed to build orchestra: %v\n%s", err, out)
	}

	// Mock claude that fails
	mockClaude := filepath.Join(binDir, "claude")
	mockScript := `#!/bin/bash
echo "something broke" >&2
exit 1
`
	if err := os.WriteFile(mockClaude, []byte(mockScript), 0o755); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	configYAML := `name: "fail-test"

defaults:
  model: sonnet
  max_turns: 5
  permission_mode: acceptEdits
  timeout_minutes: 1

teams:
  - name: doomed
    lead:
      role: "Doomed Lead"
    context: |
      This will fail.
    tasks:
      - summary: "Fail"
        details: "This task will fail."
        deliverables: ["nothing"]
        verify: "false"

  - name: never-runs
    depends_on: [doomed]
    lead:
      role: "Never Lead"
    context: |
      Should not run.
    tasks:
      - summary: "Nope"
        details: "Should not execute."
        deliverables: ["nothing"]
        verify: "false"
`
	configPath := filepath.Join(workDir, "orchestra.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binPath, "run", configPath)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))

	out, err := cmd.CombinedOutput()
	output := string(out)
	t.Logf("orchestra output:\n%s", output)

	if err == nil {
		t.Fatal("expected orchestra run to fail, but it succeeded")
	}

	if !strings.Contains(output, "FAILED") {
		t.Error("output should contain FAILED")
	}
	if !strings.Contains(output, "doomed") {
		t.Error("output should mention the doomed team")
	}
	// Tier 1 team should never start
	if strings.Contains(output, "Never Lead") {
		t.Error("tier 1 team should not have started")
	}
}
