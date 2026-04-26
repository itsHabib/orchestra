package orchestra_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// TestStart_Cancel_ReturnsPartialResult exercises the Cancel path: a
// long-running mock-claude is canceled mid-tier via h.Cancel(); Wait()
// must still return a non-nil Result reflecting partial state alongside
// a non-nil error. Mirrors TestRun_ContextCancellationReturnsPartialResult
// but drives cancellation through the Handle instead of the caller's ctx.
func TestStart_Cancel_ReturnsPartialResult(t *testing.T) {
	binDir := t.TempDir()
	// Mock-claude that sleeps long enough for us to cancel mid-run.
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 5_000)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)

	loaded, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loaded.Config
	withPath(t, binDir)
	chdir(t, workDir)

	h, err := orchestra.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		h.Cancel()
	}()

	res, err := h.Wait()
	if err == nil {
		t.Fatal("Wait: expected error from canceled run, got nil")
	}
	if res == nil {
		t.Fatal("Wait: expected partial result after Cancel, got nil")
	}
	if res.Project != "minimal-sdk" {
		t.Errorf("Project=%q, want minimal-sdk", res.Project)
	}
	if _, ok := res.Teams["solo"]; !ok {
		t.Errorf("solo team missing from canceled-run Result.Teams: %+v", res.Teams)
	}

	// Wait again — the (*Result, error) tuple should be cached, not
	// re-derived. Same pointer / same error.
	res2, err2 := h.Wait()
	if res2 != res {
		t.Errorf("second Wait returned different *Result pointer (%p vs %p)", res2, res)
	}
	if !errors.Is(err2, err) {
		t.Errorf("second Wait returned different error: first=%v second=%v", err, err2)
	}
}

// TestHandle_Status_ReflectsLiveTier polls Status() while a multi-tier
// run is executing and asserts that PhaseRunning is observed and
// CurrentTier is non-negative mid-run. The mock-claude sleeps long
// enough that we have a window to observe the running phase.
func TestHandle_Status_ReflectsLiveTier(t *testing.T) {
	binDir := t.TempDir()
	// Each team takes ~500ms — long enough to poll, short enough to
	// finish the test in seconds.
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 500)

	workDir := t.TempDir()
	configPath := writeTwoTierConfig(t, workDir)

	loaded, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loaded.Config
	withPath(t, binDir)
	chdir(t, workDir)

	h, err := orchestra.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Initial status: phase may be Initializing or already Running, but
	// elapsed should be non-negative and StartedAt should be set.
	initial := h.Status()
	if initial.StartedAt.IsZero() {
		t.Errorf("Status().StartedAt is zero immediately after Start")
	}
	if initial.Elapsed < 0 {
		t.Errorf("Status().Elapsed=%v, want >= 0", initial.Elapsed)
	}

	// Poll until we see PhaseRunning with CurrentTier >= 0, or fail.
	deadline := time.Now().Add(8 * time.Second)
	sawRunning := false
	for time.Now().Before(deadline) {
		st := h.Status()
		if st.Phase == orchestra.PhaseRunning && st.CurrentTier >= 0 {
			sawRunning = true
			break
		}
		if st.Phase == orchestra.PhaseDone {
			// Run finished before we observed running — fixture timing
			// regressed, fail explicitly so it surfaces.
			t.Fatalf("run completed before Status reported PhaseRunning")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawRunning {
		t.Fatalf("never observed PhaseRunning with CurrentTier >= 0 within deadline")
	}

	res, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res == nil {
		t.Fatal("Wait: expected non-nil Result")
	}

	// Final status: after Wait, phase should be Done.
	final := h.Status()
	if final.Phase != orchestra.PhaseDone {
		t.Errorf("post-Wait Status().Phase=%q, want %q", final.Phase, orchestra.PhaseDone)
	}
}

// writeTwoTierConfig writes an orchestra.yaml with two teams arranged
// as two tiers (second depends on first). Used by Status tests that
// need a non-trivial tier sequence.
func writeTwoTierConfig(t *testing.T, workDir string) string {
	t.Helper()
	yaml := `name: status-fixture

defaults:
  model: sonnet
  max_turns: 5
  permission_mode: acceptEdits
  timeout_minutes: 5

teams:
  - name: first
    lead:
      role: First Lead
    tasks:
      - summary: "First work"
        details: "Run first."
        deliverables:
          - "out1"
        verify: "true"
  - name: second
    lead:
      role: Second Lead
    depends_on: [first]
    tasks:
      - summary: "Second work"
        details: "Run second."
        deliverables:
          - "out2"
        verify: "true"
`
	path := filepath.Join(workDir, "orchestra.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
