package spawner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeMockScript(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSpawn_SuccessResult(t *testing.T) {
	script := writeMockScript(t, "mock-claude", `#!/bin/bash
echo '{"type":"system","subtype":"init","session_id":"sess-123"}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Working on it..."}]}}'
echo '{"type":"result","subtype":"success","result":"All done!","total_cost_usd":0.42,"num_turns":5,"duration_ms":3000,"session_id":"sess-123"}'
`)

	result, err := Spawn(context.Background(), SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do something",
		Command:  script,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Team != "test-team" {
		t.Fatalf("expected test-team, got %s", result.Team)
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
	script := writeMockScript(t, "mock-claude", `#!/bin/bash
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Final answer here"}]}}'
`)

	result, err := Spawn(context.Background(), SpawnOpts{
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
	script := writeMockScript(t, "mock-claude", `#!/bin/bash
echo "something went wrong" >&2
exit 1
`)

	_, err := Spawn(context.Background(), SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do something",
		Command:  script,
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
}

func TestSpawn_Timeout(t *testing.T) {
	script := writeMockScript(t, "mock-claude", `#!/bin/bash
sleep 30
`)

	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()

	_, err := Spawn(ctx, SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do something",
		Command:  script,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSpawn_LogWriter(t *testing.T) {
	script := writeMockScript(t, "mock-claude", `#!/bin/bash
echo '{"type":"result","subtype":"success","result":"done","total_cost_usd":0.1,"num_turns":1,"duration_ms":100}'
`)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Spawn(context.Background(), SpawnOpts{
		TeamName:  "test-team",
		Prompt:    "do something",
		Command:   script,
		LogWriter: logFile,
	})
	logFile.Close()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(logPath)
	if len(data) == 0 {
		t.Fatal("log file is empty")
	}
}

func TestSpawn_ProgressFunc(t *testing.T) {
	script := writeMockScript(t, "mock-claude", `#!/bin/bash
echo '{"type":"system","subtype":"init","session_id":"sess-1"}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me think"}]}}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/foo.go"}}]}}'
echo '{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done writing file"}]}}'
echo '{"type":"result","subtype":"success","result":"ok","total_cost_usd":0.5,"num_turns":3,"duration_ms":2000,"session_id":"sess-1"}'
`)

	var messages []string
	_, err := Spawn(context.Background(), SpawnOpts{
		TeamName: "test-team",
		Prompt:   "do something",
		Command:  script,
		ProgressFunc: func(team, msg string) {
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

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
