//go:build smoke

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSmoke_RealClaude runs a real 4-team orchestra against actual claude.
// Tag: smoke (skipped in normal test runs).
// Run: go test -tags smoke -timeout 15m -v -run TestSmoke_RealClaude
func TestSmoke_RealClaude(t *testing.T) {
	// Ensure orchestra binary is built
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "orchestra")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// Set up work dir with go module
	workDir := t.TempDir()
	configSrc := filepath.Join("testdata", "smoke", "orchestra.yaml")
	configData, err := os.ReadFile(configSrc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "orchestra.yaml"), configData, 0o644); err != nil {
		t.Fatal(err)
	}

	// Init go module so teams can build
	goModInit := exec.Command("go", "mod", "init", "fortune-api")
	goModInit.Dir = workDir
	if out, err := goModInit.CombinedOutput(); err != nil {
		t.Fatalf("go mod init failed: %v\n%s", err, out)
	}

	// Run orchestra
	t.Logf("Running orchestra in %s", workDir)
	start := time.Now()

	cmd := exec.Command(binPath, "run", filepath.Join(workDir, "orchestra.yaml"))
	cmd.Dir = workDir

	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	t.Logf("Orchestra completed in %s", elapsed.Round(time.Second))
	t.Logf("Output:\n%s", string(out))

	if err != nil {
		t.Fatalf("orchestra run failed: %v", err)
	}

	// ── Assertions ──

	wsDir := filepath.Join(workDir, ".orchestra")

	// 1. Workspace files exist
	for _, f := range []string{
		"state.json", "registry.json",
		"results/backend.json", "results/cli.json",
		"results/tests.json", "results/docker.json",
		"logs/backend.log", "logs/cli.log",
		"logs/tests.log", "logs/docker.log",
	} {
		path := filepath.Join(wsDir, f)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			t.Errorf("missing workspace file: %s", f)
			continue
		}
		if strings.HasSuffix(f, ".log") && info.Size() == 0 {
			t.Errorf("empty log file: %s", f)
		}
	}

	// 2. All teams done in state.json
	stateData, err := os.ReadFile(filepath.Join(wsDir, "state.json"))
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}

	var state struct {
		Project string `json:"project"`
		Teams   map[string]struct {
			Status        string `json:"status"`
			ResultSummary string `json:"result_summary"`
			DurationMs    int64  `json:"duration_ms"`
			InputTokens   int64  `json:"input_tokens"`
			OutputTokens  int64  `json:"output_tokens"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("parsing state.json: %v", err)
	}

	if state.Project != "fortune-api" {
		t.Errorf("project name: got %q, want %q", state.Project, "fortune-api")
	}

	for name, ts := range state.Teams {
		if ts.Status != "done" {
			t.Errorf("team %s: status=%q, want %q", name, ts.Status, "done")
		}
		if ts.ResultSummary == "" {
			t.Errorf("team %s: empty result summary", name)
		}
	}

	// 3. Registry has session IDs
	regData, err := os.ReadFile(filepath.Join(wsDir, "registry.json"))
	if err != nil {
		t.Fatalf("reading registry.json: %v", err)
	}

	var reg struct {
		Teams []struct {
			Name      string `json:"name"`
			Status    string `json:"status"`
			SessionID string `json:"session_id"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(regData, &reg); err != nil {
		t.Fatalf("parsing registry.json: %v", err)
	}

	for _, entry := range reg.Teams {
		if entry.Name == "coordinator" {
			continue
		}
		if entry.SessionID == "" {
			t.Errorf("registry: team %s has empty session_id", entry.Name)
		}
		if entry.Status != "done" {
			t.Errorf("registry: team %s status=%q, want done", entry.Name, entry.Status)
		}
	}

	// 4. Results have turns and cost
	for _, team := range []string{"backend", "cli", "tests", "docker"} {
		resData, err := os.ReadFile(filepath.Join(wsDir, "results", team+".json"))
		if err != nil {
			t.Errorf("missing result for %s: %v", team, err)
			continue
		}
		var res struct {
			Status   string `json:"status"`
			NumTurns int    `json:"num_turns"`
		}
		if err := json.Unmarshal(resData, &res); err != nil {
			t.Errorf("parsing %s result: %v", team, err)
			continue
		}
		if res.Status != "success" {
			t.Errorf("%s result status=%q, want success", team, res.Status)
		}
		if res.NumTurns == 0 {
			t.Errorf("%s result has 0 turns", team)
		}
	}

	// 5. Code was actually written
	goFiles := 0
	filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.Contains(path, ".orchestra") {
			goFiles++
		}
		return nil
	})
	if goFiles < 4 {
		t.Errorf("expected at least 4 .go files, found %d", goFiles)
	}

	// 6. Code compiles
	goBuild := exec.Command("go", "build", "./...")
	goBuild.Dir = workDir
	if out, err := goBuild.CombinedOutput(); err != nil {
		t.Errorf("go build failed: %v\n%s", err, string(out))
	}

	// 7. Tests pass
	goTest := exec.Command("go", "test", "./...")
	goTest.Dir = workDir
	if out, err := goTest.CombinedOutput(); err != nil {
		t.Errorf("go test failed: %v\n%s", err, string(out))
	}

	// Print token summary
	var totalIn, totalOut int64
	for name, ts := range state.Teams {
		totalIn += ts.InputTokens
		totalOut += ts.OutputTokens
		t.Logf("  %s: %dK in / %dK out (%dms)", name, ts.InputTokens/1000, ts.OutputTokens/1000, ts.DurationMs)
	}
	t.Logf("  TOTAL: %dK in / %dK out", totalIn/1000, totalOut/1000)
}
