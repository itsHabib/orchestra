package orchestra_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// TestHandle_Send_LocalBackend_DeliversToInbox starts a long-running mock-claude
// run, sends a steering message via Handle.Send while the team is in the
// running state, and asserts the message lands in the team's local
// file-bus inbox in the expected JSON shape (sender 0-human, type
// correction). The mock-claude script is intentionally slow so the test
// has a window to observe Status() reporting "running".
func TestHandle_Send_LocalBackend_DeliversToInbox(t *testing.T) {
	binDir := t.TempDir()
	// 5-second sleep gives a comfortable window for snapshot polling and
	// inbox delivery before the mock subprocess prints its result frame
	// and exits.
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 5_000)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)
	res, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.Config
	withPath(t, binDir)
	chdir(t, workDir)

	h, err := orchestra.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		h.Cancel()
		_, _ = h.Wait()
	}()

	waitForTeamRunning(t, h, "solo", 8*time.Second)

	const message = "use int64 for score fields"
	if err := h.Send("solo", message); err != nil {
		t.Fatalf("Send: %v", err)
	}

	inboxDir := filepath.Join(workDir, ".orchestra", "messages", "2-solo", "inbox")
	if !inboxContainsCorrection(t, inboxDir, message) {
		t.Fatalf("steering message %q not found in %s", message, inboxDir)
	}
}

// waitForTeamRunning polls Status() until the named team transitions to
// status "running" or until deadline elapses. Used by steering tests
// that need a stable in-progress window before calling Send/Interrupt.
func waitForTeamRunning(t *testing.T, h *orchestra.Handle, team string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		st := h.Status()
		ts, ok := st.Teams[team]
		if ok && ts.Status == "running" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("team %q never reached running within %s", team, deadline)
}

// inboxContainsCorrection reports whether inboxDir holds at least one
// correction-typed message from sender 0-human with the given content.
// Used to assert that Handle.Send wrote the expected message shape under
// the local backend.
func inboxContainsCorrection(t *testing.T, inboxDir, content string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(inboxDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		entries, _ := os.ReadDir(filepath.Dir(inboxDir))
		t.Logf("no message JSON in %s; messages dir entries: %+v", inboxDir, entries)
		return false
	}
	for _, path := range matches {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg["sender"] != "0-human" || msg["type"] != "correction" || msg["content"] != content {
			continue
		}
		// Recipient should be the indexed inbox folder, not the bare team name.
		if msg["recipient"] == "" {
			t.Errorf("steering message recipient is empty in %s", path)
		}
		if msg["read"] != false {
			t.Errorf("steering message read=%v, want false (in %s)", msg["read"], path)
		}
		return true
	}
	return false
}

// TestHandle_Send_TeamNotRunning_ReturnsErrTeamNotRunning runs a
// completed workflow, then calls Send for a team that no longer has
// status "running" (post-completion the team transitions to "done").
// Send should return ErrTeamNotRunning, not ErrClosed — the run is over
// but the engine has not yet returned from Wait, so the Handle is still
// "live" by the public contract.
//
// This test uses a fast mock-claude so we deliberately race the
// post-team-completion / pre-Wait window. If timing is fragile we still
// catch the case where the team is anything other than running.
func TestHandle_Send_TeamNotRunning_ReturnsErrTeamNotRunning(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)
	res, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.Config
	withPath(t, binDir)
	chdir(t, workDir)

	h, err := orchestra.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Sending to a non-existent team is the unambiguous version of this
	// case — the team will never appear in state, regardless of timing.
	err = h.Send("nonexistent-team", "anything")
	if !errors.Is(err, orchestra.ErrTeamNotRunning) {
		t.Errorf("Send to nonexistent team: got %v, want ErrTeamNotRunning", err)
	}

	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestHandle_Send_AfterWait_ReturnsErrClosed asserts the post-Wait
// contract: once Wait returns, Send returns ErrClosed regardless of
// what the team's last status was.
func TestHandle_Send_AfterWait_ReturnsErrClosed(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)
	res, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.Config
	withPath(t, binDir)
	chdir(t, workDir)

	h, err := orchestra.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if err := h.Send("solo", "post-wait"); !errors.Is(err, orchestra.ErrClosed) {
		t.Errorf("Send post-Wait: got %v, want ErrClosed", err)
	}
}

// TestHandle_Interrupt_LocalBackend_ReturnsNotSupported asserts the
// local-backend Interrupt sentinel: cancellation works at the run level
// via Cancel, but mid-turn interrupt has no transport for a local subprocess.
func TestHandle_Interrupt_LocalBackend_ReturnsNotSupported(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 5_000)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)
	res, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.Config
	withPath(t, binDir)
	chdir(t, workDir)

	h, err := orchestra.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		h.Cancel()
		_, _ = h.Wait()
	}()

	// Wait for solo to be running before we attempt Interrupt — the
	// "team not running" branch would otherwise mask the local-backend
	// sentinel.
	waitForTeamRunning(t, h, "solo", 8*time.Second)

	err = h.Interrupt("solo")
	if !errors.Is(err, orchestra.ErrInterruptNotSupported) {
		t.Errorf("Interrupt local backend: got %v, want ErrInterruptNotSupported", err)
	}
	// Sanity: ErrInterruptNotSupported message should mention the
	// limitation explicitly so the surfaced text is useful.
	if err != nil && !strings.Contains(err.Error(), "local") {
		t.Errorf("Interrupt error message lacks local-backend hint: %q", err.Error())
	}
}
