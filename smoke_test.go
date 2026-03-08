//go:build smoke

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const smokeHistoryFile = "testdata/smoke/history.jsonl"

// smokeRun is a single smoke test result appended to history.
type smokeRun struct {
	Timestamp    string          `json:"ts"`
	WallClockMs  int64           `json:"wall_ms"`
	TotalIn      int64           `json:"total_in"`
	TotalOut     int64           `json:"total_out"`
	TotalTurns   int             `json:"total_turns"`
	Passed       bool            `json:"passed"`
	Teams        map[string]smokeTeam `json:"teams"`
}

type smokeTeam struct {
	DurationMs   int64 `json:"dur_ms"`
	InputTokens  int64 `json:"in"`
	OutputTokens int64 `json:"out"`
	Turns        int   `json:"turns"`
}

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

	// 4. Results have turns
	teamTurns := make(map[string]int)
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
		teamTurns[team] = res.NumTurns
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

	// ── Record to history ──
	run := smokeRun{
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		WallClockMs: elapsed.Milliseconds(),
		Passed:      !t.Failed(),
		Teams:       make(map[string]smokeTeam),
	}
	for name, ts := range state.Teams {
		run.TotalIn += ts.InputTokens
		run.TotalOut += ts.OutputTokens
		run.Teams[name] = smokeTeam{
			DurationMs:   ts.DurationMs,
			InputTokens:  ts.InputTokens,
			OutputTokens: ts.OutputTokens,
			Turns:        teamTurns[name],
		}
		run.TotalTurns += teamTurns[name]
	}

	if line, err := json.Marshal(run); err == nil {
		f, err := os.OpenFile(smokeHistoryFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(line)
			f.Write([]byte("\n"))
			f.Close()
			t.Logf("Recorded to %s", smokeHistoryFile)
		}
	}

	// Print this run
	t.Logf("── This Run ──")
	for name, ts := range state.Teams {
		t.Logf("  %s: %dK in / %dK out, %d turns (%dms)", name, ts.InputTokens/1000, ts.OutputTokens/1000, teamTurns[name], ts.DurationMs)
	}
	t.Logf("  TOTAL: %dK in / %dK out, %d turns, %s wall", run.TotalIn/1000, run.TotalOut/1000, run.TotalTurns, elapsed.Round(time.Second))

	// Print history stats if available
	printSmokeStats(t)
}

// printSmokeStats reads history.jsonl and prints min/max/avg stats.
func printSmokeStats(t *testing.T) {
	t.Helper()

	data, err := os.ReadFile(smokeHistoryFile)
	if err != nil {
		return
	}

	var runs []smokeRun
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r smokeRun
		if err := json.Unmarshal([]byte(line), &r); err == nil && r.Passed {
			runs = append(runs, r)
		}
	}

	if len(runs) < 2 {
		t.Logf("── History: %d run(s) — need 2+ for stats ──", len(runs))
		return
	}

	t.Logf("── History: %d passing runs ──", len(runs))

	// Wall clock stats
	walls := make([]float64, len(runs))
	for i, r := range runs {
		walls[i] = float64(r.WallClockMs) / 1000
	}
	min, max, avg, stddev := stats(walls)
	t.Logf("  Wall clock: min=%.0fs  max=%.0fs  avg=%.0fs  stddev=%.0fs", min, max, avg, stddev)

	// Total tokens
	ins := make([]float64, len(runs))
	outs := make([]float64, len(runs))
	turns := make([]float64, len(runs))
	for i, r := range runs {
		ins[i] = float64(r.TotalIn) / 1000
		outs[i] = float64(r.TotalOut) / 1000
		turns[i] = float64(r.TotalTurns)
	}
	minI, maxI, avgI, _ := stats(ins)
	minO, maxO, avgO, _ := stats(outs)
	minT, maxT, avgT, _ := stats(turns)
	t.Logf("  Input:      min=%.0fK  max=%.0fK  avg=%.0fK", minI, maxI, avgI)
	t.Logf("  Output:     min=%.0fK  max=%.0fK  avg=%.0fK", minO, maxO, avgO)
	t.Logf("  Turns:      min=%.0f   max=%.0f   avg=%.0f", minT, maxT, avgT)

	// Per-team wall clock
	teamNames := []string{"backend", "cli", "tests", "docker"}
	for _, name := range teamNames {
		vals := make([]float64, 0, len(runs))
		for _, r := range runs {
			if tm, ok := r.Teams[name]; ok {
				vals = append(vals, float64(tm.DurationMs)/1000)
			}
		}
		if len(vals) > 0 {
			min, max, avg, _ := stats(vals)
			t.Logf("  %-10s  min=%.0fs  max=%.0fs  avg=%.0fs", name+":", min, max, avg)
		}
	}
}

func stats(vals []float64) (min, max, avg, stddev float64) {
	if len(vals) == 0 {
		return
	}
	sort.Float64s(vals)
	min = vals[0]
	max = vals[len(vals)-1]
	var sum float64
	for _, v := range vals {
		sum += v
	}
	avg = sum / float64(len(vals))
	if len(vals) > 1 {
		var sqDiff float64
		for _, v := range vals {
			d := v - avg
			sqDiff += d * d
		}
		stddev = math.Sqrt(sqDiff / float64(len(vals)))
	}
	return
}

// TestSmoke_Stats prints stats from history without running a new smoke test.
// Run: go test -tags smoke -v -run TestSmoke_Stats
func TestSmoke_Stats(t *testing.T) {
	data, err := os.ReadFile(smokeHistoryFile)
	if err != nil {
		t.Skipf("No history file: %v", err)
	}

	var runs []smokeRun
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r smokeRun
		if err := json.Unmarshal([]byte(line), &r); err == nil {
			runs = append(runs, r)
		}
	}

	if len(runs) == 0 {
		t.Skip("No runs in history")
	}

	// Print recent runs
	t.Logf("── %d runs ──", len(runs))
	for i, r := range runs {
		status := "✅"
		if !r.Passed {
			status = "❌"
		}
		t.Logf("  %s #%d  %s  wall=%ds  tokens=%dK→%dK  turns=%d",
			status, i+1, r.Timestamp[:19],
			r.WallClockMs/1000,
			r.TotalIn/1000, r.TotalOut/1000,
			r.TotalTurns)
	}

	// Print aggregate stats (passing runs only)
	fmt.Println() // blank line before stats
	printSmokeStats(t)
}
