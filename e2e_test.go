package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func goCommand() string {
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if goroot := runtime.GOROOT(); goroot != "" {
		path := filepath.Join(goroot, "bin", name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "go"
}

func testExecutableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func buildOrchestraBinary(t *testing.T, binDir string) string {
	t.Helper()
	binPath := filepath.Join(binDir, testExecutableName("orchestra"))
	build := exec.Command(goCommand(), "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("failed to build orchestra: %v\n%s", err, out)
	}
	return binPath
}

func writeMockClaudeCommand(t *testing.T, dir string, stdout, stderr []string, exitCode, sleepMillis int) string {
	t.Helper()
	path := filepath.Join(dir, testExecutableName("claude"))
	srcPath := filepath.Join(dir, "mock_claude.go")
	src := fmt.Sprintf(`package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if %d > 0 {
		time.Sleep(time.Duration(%d) * time.Millisecond)
	}
	for _, line := range %#v {
		fmt.Println(line)
	}
	for _, line := range %#v {
		fmt.Fprintln(os.Stderr, line)
	}
	os.Exit(%d)
}
`, sleepMillis, sleepMillis, stdout, stderr, exitCode)
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command(goCommand(), "build", "-o", path, srcPath)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building mock claude: %v\n%s", err, out)
	}
	return path
}

func pathWithPrepended(dir string) string {
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// TestE2E_OrchestraRun runs a full orchestra run with a mock claude script.
// It validates: config parsing, DAG ordering, spawner with progress output,
// workspace state, results, and logs are all written correctly.
func TestE2E_OrchestraRun(t *testing.T) {
	// Build the orchestra binary
	binDir := t.TempDir()
	binPath := buildOrchestraBinary(t, binDir)

	// Create a mock claude script that emits stream-json
	writeMockClaudeCommand(t, binDir, []string{
		`{"type":"system","subtype":"init","session_id":"sess-e2e"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"planning..."}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Starting work on the task..."}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/test.go"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"go build ./..."}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"All tasks completed successfully."}]}}`,
		`{"type":"result","subtype":"success","result":"Completed all tasks.","total_cost_usd":0.25,"num_turns":3,"duration_ms":1500,"session_id":"sess-e2e"}`,
	}, nil, 0, 100)

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
	cmd.Env = append(os.Environ(), pathWithPrepended(binDir))

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
	binPath := buildOrchestraBinary(t, binDir)

	// Mock claude that fails
	writeMockClaudeCommand(t, binDir, nil, []string{"something broke"}, 1, 0)

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
	cmd.Env = append(os.Environ(), pathWithPrepended(binDir))

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
