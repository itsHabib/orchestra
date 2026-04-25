// Package masteering contains the live-MA delivery test for `orchestra msg`.
//
// The test is opt-in: it runs only when ORCHESTRA_MA_INTEGRATION=1 and
// ANTHROPIC_API_KEY (or ~/.config/orchestra/config.json:api_key) is set. It
// builds the orchestra binary, drives a real run against Managed Agents,
// steers it with `orchestra msg`, and asserts the event landed in the
// per-team event log. It does NOT assert the agent obeyed the steering —
// that is model-variance noise.
//
// The directory keeps its `ma_steering` name to match the `make
// e2e-ma-steering` target and the sibling `ma_single_team` /
// `ma_multi_team` fixtures; the in-file package identifier drops the
// underscore to satisfy revive's package-name rule.
package masteering

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/store"
)

const sentinelMessage = "use the existing JSON store, not a database"

func TestSteeringDelivery_LiveMA(t *testing.T) {
	if os.Getenv("ORCHESTRA_MA_INTEGRATION") != "1" {
		t.Skip("set ORCHESTRA_MA_INTEGRATION=1 to enable")
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		// machost.NewClient also accepts ~/.config/orchestra/config.json,
		// but skipping cleanly when neither is set saves a confusing
		// "auth missing" error from the helper subprocess.
		if _, err := os.Stat(orchestraConfigPath()); err != nil {
			t.Skip("set ANTHROPIC_API_KEY (or populate ~/.config/orchestra/config.json) to enable")
		}
	}

	repoRoot := findRepoRoot(t)
	binDir := t.TempDir()
	bin := buildOrchestraBin(t, repoRoot, binDir)

	workDir := t.TempDir()
	configPath := filepath.Join(repoRoot, "test", "integration", "ma_steering", "orchestra.yaml")

	runCtx, cancelRun := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancelRun()
	runCmd := exec.CommandContext(runCtx, bin, "run", configPath)
	runCmd.Dir = workDir
	runCmd.Env = append(os.Environ(), "ORCHESTRA_MA_INTEGRATION=1")
	runCmd.Stdout = os.Stdout
	runCmd.Stderr = os.Stderr
	if err := runCmd.Start(); err != nil {
		t.Fatalf("starting orchestra run: %v", err)
	}
	t.Cleanup(func() {
		if runCmd.ProcessState == nil {
			_ = runCmd.Process.Kill()
		}
	})

	statePath := filepath.Join(workDir, ".orchestra", "state.json")
	if err := waitForRunningTeam(t, statePath, "intro", 90*time.Second); err != nil {
		t.Fatalf("team never reached running: %v", err)
	}

	msgCtx, cancelMsg := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelMsg()
	msg := exec.CommandContext(msgCtx, bin,
		"msg",
		"--workspace", filepath.Join(workDir, ".orchestra"),
		"--team", "intro",
		"--message", sentinelMessage,
	)
	msg.Env = os.Environ()
	out, err := msg.CombinedOutput()
	if err != nil {
		t.Fatalf("orchestra msg failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "ok" {
		t.Logf("orchestra msg stdout: %q", got)
	}

	if err := runCmd.Wait(); err != nil {
		t.Fatalf("orchestra run failed: %v", err)
	}

	logPath := filepath.Join(workDir, ".orchestra", "logs", "intro.ndjson")
	steerEvent, ok := findUserMessageEvent(t, logPath, sentinelMessage)
	if !ok {
		t.Fatalf("no user.message event with sentinel text in %s", logPath)
	}
	if steerEvent.ID == "" {
		t.Fatal("steering event landed without an id")
	}

	finalState := loadState(t, statePath)
	intro := finalState.Teams["intro"]
	if intro.LastEventID == "" {
		t.Fatal("LastEventID never advanced")
	}
}

func waitForRunningTeam(t *testing.T, statePath, team string, deadline time.Duration) error {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		st, err := tryLoadState(statePath)
		if err == nil {
			ts, ok := st.Teams[team]
			if ok && ts.Status == "running" && ts.SessionID != "" {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s to be running with a session id", team)
}

type loggedEvent struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Content []eventContent `json:"content"`
}

type eventContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func findUserMessageEvent(t *testing.T, path, sentinel string) (loggedEvent, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening log: %v", err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ev loggedEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type != "user.message" {
			continue
		}
		for _, c := range ev.Content {
			if c.Text == sentinel {
				return ev, true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning log: %v", err)
	}
	return loggedEvent{}, false
}

func tryLoadState(path string) (*store.RunState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st store.RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func loadState(t *testing.T, path string) *store.RunState {
	t.Helper()
	st, err := tryLoadState(path)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st
}

func buildOrchestraBin(t *testing.T, repoRoot, binDir string) string {
	t.Helper()
	name := "orchestra"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binPath := filepath.Join(binDir, name)
	buildCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(buildCtx, goExe(), "build", "-o", binPath, ".")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building orchestra: %v\n%s", err, out)
	}
	return binPath
}

func goExe() string {
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return name
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod ancestor of %s", wd)
		}
		dir = parent
	}
}

func orchestraConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "orchestra", "config.json")
}
