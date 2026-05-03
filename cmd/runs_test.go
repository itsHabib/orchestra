package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/store"
)

// TestBuildRunRecordForShow_ResolvesActiveAndArchive exercises the
// runs-show migration helper that wraps orchestra.LoadRun. The cmd-side
// renderer needs a runRecord with active/dir/modifiedAt populated, and
// the SDK helper deliberately doesn't surface those — this test pins
// the cmd-side reconstruction logic.
func TestBuildRunRecordForShow_ResolvesActiveAndArchive(t *testing.T) {
	workspace := t.TempDir()
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	tierZero := 0
	activeState := &store.RunState{
		Project: "active-project", RunID: "active-run", StartedAt: now,
		Agents: map[string]store.AgentState{
			"api": {Status: "running", Tier: &tierZero},
		},
	}
	writeRunState(t, filepath.Join(workspace, "state.json"), activeState)
	oldState := &store.RunState{
		Project: "old-project", RunID: "old-run", StartedAt: now.Add(-2 * time.Hour),
		Agents: map[string]store.AgentState{
			"worker": {Status: "done", Tier: &tierZero},
		},
	}
	writeRunState(t, filepath.Join(workspace, "archive", "old-run", "state.json"), oldState)

	t.Run("active alias", func(t *testing.T) {
		record, err := buildRunRecordForShow(workspace, "active", activeState)
		if err != nil {
			t.Fatalf("buildRunRecordForShow active: %v", err)
		}
		if !record.active || record.id != "active-run" {
			t.Errorf("active record: %+v, want active-run with active=true", record)
		}
		if record.dir != workspace {
			t.Errorf("active dir=%q, want %q", record.dir, workspace)
		}
	})

	t.Run("explicit archived id", func(t *testing.T) {
		record, err := buildRunRecordForShow(workspace, "old-run", oldState)
		if err != nil {
			t.Fatalf("buildRunRecordForShow archived: %v", err)
		}
		if record.active || record.id != "old-run" {
			t.Errorf("archived record: %+v, want old-run with active=false", record)
		}
		wantDir := filepath.Join(workspace, "archive", "old-run")
		if record.dir != wantDir {
			t.Errorf("archived dir=%q, want %q", record.dir, wantDir)
		}
	})
}

func TestLoadRunRecordsReadsActiveAndArchive(t *testing.T) {
	workspace := t.TempDir()
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	activeTier := 0
	writeRunState(t, filepath.Join(workspace, "state.json"), &store.RunState{
		Project:   "active-project",
		RunID:     "active-run",
		StartedAt: now,
		Agents: map[string]store.AgentState{
			"api": {Status: "running", Tier: &activeTier, StartedAt: now.Add(-2 * time.Minute)},
		},
	})
	archiveDir := filepath.Join(workspace, "archive", "old-run")
	oldTier := 0
	writeRunState(t, filepath.Join(archiveDir, "state.json"), &store.RunState{
		Project:   "old-project",
		RunID:     "old-run",
		StartedAt: now.Add(-2 * time.Hour),
		Agents: map[string]store.AgentState{
			"worker": {
				Status:     "done",
				Tier:       &oldTier,
				StartedAt:  now.Add(-2 * time.Hour),
				EndedAt:    now.Add(-90 * time.Minute),
				CostUSD:    1.25,
				DurationMs: int64((30 * time.Minute) / time.Millisecond),
			},
		},
	})

	records, err := loadRunRecords(workspace)
	if err != nil {
		t.Fatalf("loadRunRecords: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("len(records)=%d, want 2", len(records))
	}
	if records[0].id != "active-run" || !records[0].active {
		t.Fatalf("first record=%+v, want active-run active", records[0])
	}
	if records[1].id != "old-run" || records[1].active {
		t.Fatalf("second record=%+v, want archived old-run", records[1])
	}
}

func TestAggregateRunStatus(t *testing.T) {
	cases := []struct {
		name  string
		teams map[string]store.AgentState
		want  string
	}{
		{
			name:  "all done",
			teams: map[string]store.AgentState{"a": {Status: "done"}, "b": {Status: "done"}},
			want:  "done",
		},
		{
			name:  "failed wins",
			teams: map[string]store.AgentState{"a": {Status: "done"}, "b": {Status: "failed"}},
			want:  "failed",
		},
		{
			name:  "running wins over pending",
			teams: map[string]store.AgentState{"a": {Status: "running"}, "b": {Status: "pending"}},
			want:  "running",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregateRunStatus(&store.RunState{Agents: tt.teams})
			if got != tt.want {
				t.Fatalf("aggregateRunStatus=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestProtectRunAgentRefsProtectsRecentRunReferences(t *testing.T) {
	refs := runAgentRefs{
		allAgentIDs:       map[string]struct{}{"agent-active": {}, "agent-old": {}},
		protectedAgentIDs: map[string]struct{}{"agent-active": {}},
	}

	protect := protectRunAgentRefs(refs)
	if !protect("project__active", "agent-active") {
		t.Fatal("expected active agent to be protected")
	}
	if protect("project__old", "agent-old") {
		t.Fatal("expected old agent to be eligible")
	}
}

func writeRunState(t *testing.T, path string, state *store.RunState) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
