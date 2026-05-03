package spawner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

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

func writeMockCommand(t *testing.T, stdout, stderr []string, exitCode, sleepMillis int) string {
	t.Helper()
	dir := t.TempDir()
	name := "mock-claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	srcPath := filepath.Join(dir, "main.go")
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
	build := exec.CommandContext(context.Background(), goCommand(), "build", "-o", path, srcPath)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building mock command: %v\n%s", err, out)
	}
	return path
}

func TestSpawn_SuccessResult(t *testing.T) {
	script := writeMockCommand(t, []string{
		`{"type":"system","subtype":"init","session_id":"sess-123"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Working on it..."}]}}`,
		`{"type":"result","subtype":"success","result":"All done!","total_cost_usd":0.42,"num_turns":5,"duration_ms":3000,"session_id":"sess-123"}`,
	}, nil, 0, 0)

	result, err := Spawn(context.Background(), &SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do something",
		Command:  script,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Agent != "test-team" {
		t.Fatalf("expected test-team, got %s", result.Agent)
	}
	if result.Status != "success" {
		t.Fatalf("expected success, got %s", result.Status)
	}
	if result.Result != "All done!" {
		t.Fatalf("expected 'All done!', got %q", result.Result)
	}
	if result.CostUSD != 0.42 {
		t.Fatalf("expected 0.42, got %f", result.CostUSD)
	}
	if result.NumTurns != 5 {
		t.Fatalf("expected 5, got %d", result.NumTurns)
	}
	if result.SessionID != "sess-123" {
		t.Fatalf("expected sess-123, got %s", result.SessionID)
	}
}

func TestSpawn_FallbackToAssistantText(t *testing.T) {
	script := writeMockCommand(t, []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Final answer here"}]}}`,
	}, nil, 0, 0)

	result, err := Spawn(context.Background(), &SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do something",
		Command:  script,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("expected success, got %s", result.Status)
	}
	if result.Result != "Final answer here" {
		t.Fatalf("expected fallback text, got %q", result.Result)
	}
}

func TestSpawn_ErrorExit(t *testing.T) {
	script := writeMockCommand(t, nil, []string{"something went wrong"}, 1, 0)

	_, err := Spawn(context.Background(), &SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do something",
		Command:  script,
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
}

func TestSpawn_Timeout(t *testing.T) {
	script := writeMockCommand(t, nil, nil, 0, 30000)

	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()

	_, err := Spawn(ctx, &SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do something",
		Command:  script,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSpawn_LogWriter(t *testing.T) {
	script := writeMockCommand(t, []string{
		`{"type":"result","subtype":"success","result":"done","total_cost_usd":0.1,"num_turns":1,"duration_ms":100}`,
	}, nil, 0, 0)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Spawn(context.Background(), &SpawnOpts{
		TeamName:  "test-team",
		Prompt:    "do something",
		Command:   script,
		LogWriter: logFile,
	})
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(logPath)
	if len(data) == 0 {
		t.Fatal("log file is empty")
	}
}

func TestSpawn_ProgressFunc(t *testing.T) {
	script := writeMockCommand(t, []string{
		`{"type":"system","subtype":"init","session_id":"sess-1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me think"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/foo.go"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done writing file"}]}}`,
		`{"type":"result","subtype":"success","result":"ok","total_cost_usd":0.5,"num_turns":3,"duration_ms":2000,"session_id":"sess-1"}`,
	}, nil, 0, 0)

	var messages []string
	_, err := Spawn(context.Background(), &SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do something",
		Command:  script,
		ProgressFunc: func(_ string, msg string) {
			messages = append(messages, msg)
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have: session started, thinking, tool_use Write, tool completed, text, finished
	if len(messages) < 5 {
		t.Fatalf("expected at least 5 progress messages, got %d: %v", len(messages), messages)
	}

	// Verify key messages are present
	found := map[string]bool{}
	for _, m := range messages {
		if contains(m, "session started") {
			found["session"] = true
		}
		if contains(m, "thinking") {
			found["thinking"] = true
		}
		if contains(m, "Write") {
			found["write"] = true
		}
		if contains(m, "tool completed") {
			found["tool_result"] = true
		}
		if contains(m, "finished") {
			found["finished"] = true
		}
	}
	for _, key := range []string{"session", "thinking", "write", "tool_result", "finished"} {
		if !found[key] {
			t.Errorf("missing progress message for %q. Got: %v", key, messages)
		}
	}
}

func TestSpawn_OnToolUse(t *testing.T) {
	script := writeMockCommand(t, []string{
		`{"type":"system","subtype":"init","session_id":"sess-tool"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/tmp/foo.go"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/bar.go"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
		`{"type":"result","subtype":"success","result":"ok","total_cost_usd":0,"num_turns":1,"duration_ms":1,"session_id":"sess-tool"}`,
	}, nil, 0, 0)

	var seen []string
	_, err := Spawn(context.Background(), &SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do",
		Command:  script,
		OnToolUse: func(name string, _ time.Time) {
			seen = append(seen, name)
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	want := []string{"Bash", "Edit", "Write"}
	if len(seen) != len(want) {
		t.Fatalf("OnToolUse calls = %v, want %v", seen, want)
	}
	for i, name := range want {
		if seen[i] != name {
			t.Errorf("OnToolUse[%d] = %q, want %q", i, seen[i], name)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || s != "" && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
