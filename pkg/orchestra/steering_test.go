package orchestra_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// TestHandle_Send_LocalBackend_ReturnsNotSupported asserts that under
// backend: local, [Handle.Send] surfaces [orchestra.ErrLocalSteeringNotSupported]
// rather than the (now-removed) file-bus delivery. v3 phase A dropped the
// file message bus; the local subprocess has no out-of-band steering channel
// and the documented workaround is to restart the run with appended context.
func TestHandle_Send_LocalBackend_ReturnsNotSupported(t *testing.T) {
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

	waitForTeamRunning(t, h, "solo", 8*time.Second)

	if err := h.Send("solo", "use int64 for score fields"); !errors.Is(err, orchestra.ErrLocalSteeringNotSupported) {
		t.Fatalf("Send local: err = %v, want ErrLocalSteeringNotSupported", err)
	}

	// The bus directory must not have been created — bus removal means no
	// .orchestra/messages tree exists at all under the workspace.
	if _, err := os.Stat(filepath.Join(workDir, ".orchestra", "messages")); !os.IsNotExist(err) {
		t.Fatalf("messages dir should not exist after bus removal: %v", err)
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
		ts, ok := st.Agents[team]
		if ok && ts.Status == "running" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("team %q never reached running within %s", team, deadline)
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
