package orchestra_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// TestLoadConfig_AliasFidelity asserts that loading a YAML produces a
// fully-resolved Config with the alias-driven Backend.UnmarshalYAML
// honoring the scalar-string spelling (the older syntax).
func TestLoadConfig_AliasFidelity(t *testing.T) {
	dir := t.TempDir()
	yaml := `name: alias-fidelity

backend: local

defaults:
  model: sonnet

teams:
  - name: solo
    lead:
      role: Solo Lead
    tasks:
      - summary: do work
        details: detail
        verify: "true"
`
	path := filepath.Join(dir, "orchestra.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := orchestra.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Backend.Kind != orchestra.BackendLocal {
		t.Fatalf("Backend.Kind=%q, want %q", cfg.Backend.Kind, orchestra.BackendLocal)
	}
	if cfg.Defaults.Model != "sonnet" {
		t.Fatalf("Defaults.Model=%q, want sonnet", cfg.Defaults.Model)
	}
	if len(cfg.Teams) != 1 || cfg.Teams[0].Name != "solo" {
		t.Fatalf("unexpected teams: %+v", cfg.Teams)
	}
}

// TestRun_LocalBackend_PopulatesResult runs a one-team orchestra against
// a mock-claude script using the local backend. It proves that Result
// carries everything printSummary needs without disk reads.
func TestRun_LocalBackend_PopulatesResult(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)

	cfg, _, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	withPath(t, binDir)
	chdir(t, workDir)

	res, err := orchestra.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("Run returned nil result")
	}
	team, ok := res.Teams["solo"]
	if !ok {
		t.Fatalf("solo team missing from Result.Teams: %+v", res.Teams)
	}
	if team.Status != "done" {
		t.Errorf("Status=%q, want done", team.Status)
	}
	if team.NumTurns <= 0 {
		t.Errorf("NumTurns=%d, want > 0", team.NumTurns)
	}
	if team.InputTokens <= 0 {
		t.Errorf("InputTokens=%d, want > 0", team.InputTokens)
	}
	if team.CostUSD <= 0 {
		t.Errorf("CostUSD=%v, want > 0", team.CostUSD)
	}
	if len(res.Tiers) == 0 {
		t.Errorf("Tiers empty, want at least one")
	}
	if res.Project != "minimal-sdk" {
		t.Errorf("Project=%q, want minimal-sdk", res.Project)
	}
}

// TestRun_ConcurrentSameWorkspaceReturnsErrRunInProgress proves that
// concurrent invocations against the same workspace within the process
// surface a typed sentinel rather than racing for the workspace lock.
func TestRun_ConcurrentSameWorkspaceReturnsErrRunInProgress(t *testing.T) {
	binDir := t.TempDir()
	// Mock-claude that takes a moment so the second Run definitely overlaps.
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 200)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)

	cfg, _, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	withPath(t, binDir)
	chdir(t, workDir)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cloned := orchestra.CloneConfig(cfg)
			_, err := orchestra.Run(context.Background(), cloned)
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	var firstErr, secondErr error
	for _, e := range errs {
		if errors.Is(e, orchestra.ErrRunInProgress) {
			secondErr = e
			continue
		}
		firstErr = e
	}
	if firstErr != nil {
		t.Fatalf("non-ErrRunInProgress invocation failed: %v", firstErr)
	}
	if secondErr == nil {
		t.Fatal("expected one of the concurrent invocations to return ErrRunInProgress, got none")
	}
}

// TestRun_ConcurrentDifferentWorkspacesIndependent proves that
// different WithWorkspaceDir values run independently in the same
// process.
func TestRun_ConcurrentDifferentWorkspacesIndependent(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 100)

	rootA := t.TempDir()
	rootB := t.TempDir()
	configPathA := writeOneTeamConfig(t, rootA)
	configPathB := writeOneTeamConfig(t, rootB)

	cfgA, _, err := orchestra.LoadConfig(configPathA)
	if err != nil {
		t.Fatal(err)
	}
	cfgB, _, err := orchestra.LoadConfig(configPathB)
	if err != nil {
		t.Fatal(err)
	}

	withPath(t, binDir)
	chdir(t, rootA)

	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errA = orchestra.Run(context.Background(), cfgA, orchestra.WithWorkspaceDir(filepath.Join(rootA, ".orchestra")))
	}()
	go func() {
		defer wg.Done()
		_, errB = orchestra.Run(context.Background(), cfgB, orchestra.WithWorkspaceDir(filepath.Join(rootB, ".orchestra")))
	}()
	wg.Wait()

	if errA != nil {
		t.Errorf("Run(A): %v", errA)
	}
	if errB != nil {
		t.Errorf("Run(B): %v", errB)
	}
}

// TestRun_TierZeroFailureReturnsResultAndError proves that on early-tier
// failure, Run returns both an error AND a Result reflecting the
// failed-team state. The structural defer in Run also guarantees no
// orchestra-spawned subprocesses survive past the return — exercised
// implicitly by the test completing in bounded time.
func TestRun_TierZeroFailureReturnsResultAndError(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, nil, []string{"mock failure"}, 1, 0)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)

	cfg, _, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	withPath(t, binDir)
	chdir(t, workDir)

	res, err := orchestra.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run: expected error, got nil")
	}
	if res == nil {
		t.Fatal("Run: expected partial result on error, got nil")
	}
	team, ok := res.Teams["solo"]
	if !ok {
		t.Fatalf("solo team missing from Result.Teams: %+v", res.Teams)
	}
	if team.Status != "failed" {
		t.Errorf("Status=%q, want failed", team.Status)
	}
}

// TestRun_TakesOwnershipOfConfig documents the ownership contract: Run
// may call ResolveDefaults on the caller's pointer, so a Config with
// zero-value Defaults.Model becomes "sonnet" after the call.
func TestRun_TakesOwnershipOfConfig(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)

	workDir := t.TempDir()
	cfg := &orchestra.Config{
		Name: "ownership",
		Teams: []orchestra.Team{
			{
				Name: "solo",
				Lead: orchestra.Lead{Role: "Lead"},
				Tasks: []orchestra.Task{
					{Summary: "do work", Details: "detail", Verify: "true"},
				},
			},
		},
	}
	if cfg.Defaults.Model != "" {
		t.Fatal("test setup: Defaults.Model should be empty before Run")
	}

	withPath(t, binDir)
	chdir(t, workDir)

	if _, err := orchestra.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Defaults.Model != "sonnet" {
		t.Errorf("Defaults.Model=%q, want sonnet (ResolveDefaults ran on caller's pointer)", cfg.Defaults.Model)
	}
}

// TestValidate_StandaloneCallerBuiltConfig exercises Validate on a
// programmatically-built config — the path consumers without a YAML
// file would take.
func TestValidate_StandaloneCallerBuiltConfig(t *testing.T) {
	cfg := &orchestra.Config{
		Name: "programmatic",
		Teams: []orchestra.Team{
			{
				Name: "solo",
				Lead: orchestra.Lead{Role: "Lead"},
				Tasks: []orchestra.Task{
					{Summary: "x", Details: "y", Verify: "true"},
				},
			},
		},
	}
	if _, err := orchestra.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Defaults.Model != "sonnet" {
		t.Errorf("Defaults.Model=%q, want sonnet", cfg.Defaults.Model)
	}
}

// TestCloneConfig_DeepCopy proves CloneConfig isolates mutations from
// the source so callers can run Run concurrently.
func TestCloneConfig_DeepCopy(t *testing.T) {
	src := &orchestra.Config{
		Name: "src",
		Teams: []orchestra.Team{
			{Name: "a", Tasks: []orchestra.Task{{Summary: "s1", Deliverables: []string{"x"}}}, DependsOn: []string{"upstream"}},
		},
	}
	clone := orchestra.CloneConfig(src)
	if clone == src {
		t.Fatal("CloneConfig returned the same pointer")
	}
	clone.Name = "clone"
	clone.Teams[0].Name = "b"
	clone.Teams[0].Tasks[0].Summary = "s2"
	clone.Teams[0].Tasks[0].Deliverables[0] = "y"
	clone.Teams[0].DependsOn[0] = "other"

	if src.Name != "src" {
		t.Errorf("src.Name mutated: %q", src.Name)
	}
	if src.Teams[0].Name != "a" {
		t.Errorf("src team name mutated: %q", src.Teams[0].Name)
	}
	if src.Teams[0].Tasks[0].Summary != "s1" {
		t.Errorf("src task summary mutated: %q", src.Teams[0].Tasks[0].Summary)
	}
	if src.Teams[0].Tasks[0].Deliverables[0] != "x" {
		t.Errorf("src deliverable mutated: %q", src.Teams[0].Tasks[0].Deliverables[0])
	}
	if src.Teams[0].DependsOn[0] != "upstream" {
		t.Errorf("src DependsOn mutated: %q", src.Teams[0].DependsOn[0])
	}
}

// --- helpers --------------------------------------------------------------

func writeOneTeamConfig(t *testing.T, workDir string) string {
	t.Helper()
	yaml := `name: minimal-sdk

defaults:
  model: sonnet
  max_turns: 5
  permission_mode: acceptEdits
  timeout_minutes: 5

teams:
  - name: solo
    lead:
      role: Solo Lead
    context: |
      Minimal SDK exercise.
    tasks:
      - summary: "Do work"
        details: "Run the mock task."
        deliverables:
          - "out"
        verify: "true"
`
	path := filepath.Join(workDir, "orchestra.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mockSuccessStream() []string {
	return []string{
		`{"type":"system","subtype":"init","session_id":"sess-sdk"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}`,
		`{"type":"result","subtype":"success","result":"done","total_cost_usd":0.42,"num_turns":2,"duration_ms":900,"session_id":"sess-sdk","usage":{"input_tokens":111,"output_tokens":22}}`,
	}
}

func writeMockClaude(t *testing.T, dir string, stdout, stderr []string, exitCode, sleepMillis int) {
	t.Helper()
	binPath := filepath.Join(dir, executableName("claude"))
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
	build := exec.CommandContext(context.Background(), goCommand(), "build", "-o", binPath, srcPath)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building mock claude: %v\n%s", err, out)
	}
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func goCommand() string {
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	if runtime.GOOS == "windows" {
		path := `C:\Program Files\Go\bin\` + name
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	path := "/usr/local/go/bin/" + name
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return "go"
}

func withPath(t *testing.T, dir string) {
	t.Helper()
	prev := os.Getenv("PATH")
	combined := dir + string(os.PathListSeparator) + prev
	if err := os.Setenv("PATH", combined); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("PATH", prev)
	})
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prev)
	})
}
