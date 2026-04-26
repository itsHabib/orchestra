package orchestra_test

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// TestListRuns_RoundTripsArchive runs a short workflow against the
// mock-claude binary, lets the run archive itself by starting a second
// run in the same workspace, then asserts that ListRuns surfaces both
// the active and the archived run with the expected metadata.
func TestListRuns_RoundTripsArchive(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)
	withPath(t, binDir)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)
	chdir(t, workDir)

	cfg, _, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestra.Run(context.Background(), cfg); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// Second run forces the first one into the archive directory.
	cfg2, _, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestra.Run(context.Background(), cfg2); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	workspace := filepath.Join(workDir, ".orchestra")
	summaries, err := orchestra.ListRuns(workspace)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(summaries) < 2 {
		t.Fatalf("ListRuns returned %d summaries, want >= 2", len(summaries))
	}
	// The first entry is newest-first: the most recently started run, which
	// is the second Run() call. Active flag tracks "is in workspace root"
	// vs "is in archive/" — exactly one entry should be active.
	activeCount := 0
	for _, s := range summaries {
		if s.Active {
			activeCount++
		}
		if s.RunID == "" {
			t.Errorf("RunID empty in summary: %+v", s)
		}
		if s.Project != "minimal-sdk" {
			t.Errorf("Project=%q, want minimal-sdk", s.Project)
		}
		if s.TeamCount != 1 {
			t.Errorf("TeamCount=%d, want 1 (one team in fixture)", s.TeamCount)
		}
		if s.Status != "done" {
			t.Errorf("Status=%q, want done (mock-claude returns success)", s.Status)
		}
		if s.State == nil {
			t.Errorf("State should be non-nil, got nil for %s", s.RunID)
		}
	}
	if activeCount != 1 {
		t.Errorf("active summaries=%d, want exactly 1", activeCount)
	}
}

// TestLoadRun_UnknownRunReturnsNotExist asserts the documented sentinel
// behavior — calling LoadRun with a bogus ID surfaces an error wrapping
// fs.ErrNotExist so callers can use errors.Is for lookup-failure
// detection.
func TestLoadRun_UnknownRunReturnsNotExist(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)
	withPath(t, binDir)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)
	chdir(t, workDir)

	cfg, _, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestra.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	workspace := filepath.Join(workDir, ".orchestra")
	if _, err := orchestra.LoadRun(workspace, "definitely-not-a-real-id"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("LoadRun unknown: got %v, want errors.Is fs.ErrNotExist", err)
	}
}

// TestLoadRun_ActiveAlias_ResolvesCurrentRun asserts the "active" alias —
// passing "active" to LoadRun returns the currently-active state.json's
// run, regardless of its persisted RunID.
func TestLoadRun_ActiveAlias_ResolvesCurrentRun(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)
	withPath(t, binDir)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)
	chdir(t, workDir)

	cfg, _, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestra.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	workspace := filepath.Join(workDir, ".orchestra")
	state, err := orchestra.LoadRun(workspace, "active")
	if err != nil {
		t.Fatalf("LoadRun active: %v", err)
	}
	if state == nil {
		t.Fatal("LoadRun active: nil state")
	}
	if state.Project != "minimal-sdk" {
		t.Errorf("active state Project=%q, want minimal-sdk", state.Project)
	}
}

// TestListSessions_LocalBackendReturnsEmpty asserts the documented
// no-error contract for local-backend workspaces. The SDK helper is
// callable from any backend and returns an empty slice when there are
// no managed-agents sessions to enumerate.
func TestListSessions_LocalBackendReturnsEmpty(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)
	withPath(t, binDir)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)
	chdir(t, workDir)

	cfg, _, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestra.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	workspace := filepath.Join(workDir, ".orchestra")
	sessions, err := orchestra.ListSessions(workspace)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("ListSessions on local-backend run: got %d entries, want 0", len(sessions))
	}
}

// TestListSessions_NoActiveRunReturnsEmpty asserts that pointing at a
// workspace dir with no state.json yields the empty slice rather than an
// error — matches the SDK's "safe to call any time" contract.
func TestListSessions_NoActiveRunReturnsEmpty(t *testing.T) {
	workDir := t.TempDir()
	sessions, err := orchestra.ListSessions(workDir)
	if err != nil {
		t.Errorf("ListSessions empty workspace: got error %v, want nil", err)
	}
	if len(sessions) != 0 {
		t.Errorf("ListSessions empty workspace: got %d entries, want 0", len(sessions))
	}
}
