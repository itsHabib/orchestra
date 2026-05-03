package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/memstore"
	"github.com/itsHabib/orchestra/internal/workspace"
)

func TestBeginSeedsStateAndHoldsLock(t *testing.T) {
	ctx := context.Background()
	svc := New(memstore.New(), WithClock(fixedClock()))

	active, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = svc.End(active) }()

	if active.RunID == "" {
		t.Fatal("expected active run id")
	}
	if active.State.Project != "test-project" {
		t.Fatalf("Project=%q, want test-project", active.State.Project)
	}
	if active.State.Agents["alpha"].Status != "pending" {
		t.Fatalf("alpha status=%q, want pending", active.State.Agents["alpha"].Status)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	_, err = svc.Begin(waitCtx, testConfig())
	if !errors.Is(err, store.ErrLockTimeout) {
		t.Fatalf("second Begin err=%v, want ErrLockTimeout", err)
	}
}

func TestBeginSeedsTeamTiers(t *testing.T) {
	ctx := context.Background()
	svc := New(memstore.New(), WithClock(fixedClock()))
	cfg := &config.Config{
		Name: "tiered",
		Agents: []config.Agent{
			{Name: "api"},
			{Name: "web", DependsOn: []string{"api"}},
			{Name: "docs"},
		},
	}

	active, err := svc.Begin(ctx, cfg)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = svc.End(active) }()

	got, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertTeamTier(t, got.Agents["api"].Tier, 0)
	assertTeamTier(t, got.Agents["docs"].Tier, 0)
	assertTeamTier(t, got.Agents["web"].Tier, 1)
}

func TestBeginArchivesPriorStateBeforeSeeding(t *testing.T) {
	ctx := context.Background()
	base := memstore.New()
	if err := base.SaveRunState(ctx, &store.RunState{
		Project: "old",
		RunID:   "old-run",
		Agents:   map[string]store.AgentState{"old": {Status: "done"}},
	}); err != nil {
		t.Fatal(err)
	}
	spy := &archiveSpy{Store: base}
	svc := New(spy, WithClock(fixedClock()))

	active, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = svc.End(active) }()

	if spy.archiveCalls != 1 {
		t.Fatalf("archiveCalls=%d, want 1", spy.archiveCalls)
	}
	got, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Project != "test-project" {
		t.Fatalf("Project=%q, want fresh state", got.Project)
	}
	if _, ok := got.Agents["old"]; ok {
		t.Fatalf("fresh state retained old team: %+v", got.Agents)
	}
}

func TestBeginManagedAgentsSkipsMessageBus(t *testing.T) {
	ctx := context.Background()
	wsPath := filepath.Join(t.TempDir(), ".orchestra")
	svc := New(memstore.New(), WithWorkspace(workspace.ForPath(wsPath)))
	cfg := testConfig()
	cfg.Backend.Kind = "managed_agents"

	active, err := svc.Begin(ctx, cfg)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = svc.End(active) }()

	if active.Bus != nil {
		t.Fatalf("Bus=%v, want nil for managed_agents", active.Bus)
	}
	if _, err := os.Stat(filepath.Join(wsPath, "messages")); !os.IsNotExist(err) {
		t.Fatalf("messages dir err=%v, want not exist", err)
	}
}

func TestRecordTeamTransitions(t *testing.T) {
	ctx := context.Background()
	svc := New(memstore.New(), WithClock(fixedClock()))
	active, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.End(active) }()

	if err := svc.RecordTeamStart(ctx, "alpha"); err != nil {
		t.Fatalf("RecordTeamStart: %v", err)
	}
	if err := svc.RecordTeamComplete(ctx, &TeamResult{
		Agent:        "alpha",
		Status:       "success",
		Result:       "built it",
		CostUSD:      1.25,
		NumTurns:     3,
		DurationMs:   1500,
		SessionID:    "sess-alpha",
		InputTokens:  100,
		OutputTokens: 50,
	}); err != nil {
		t.Fatalf("RecordTeamComplete: %v", err)
	}

	got, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alpha := got.Agents["alpha"]
	if alpha.Status != "done" || alpha.ResultSummary != "built it" || alpha.SessionID != "sess-alpha" {
		t.Fatalf("unexpected alpha state: %+v", alpha)
	}
	if alpha.InputTokens != 100 || alpha.OutputTokens != 50 || alpha.DurationMs != 1500 {
		t.Fatalf("unexpected alpha counters: %+v", alpha)
	}
	if alpha.NumTurns != 3 {
		t.Fatalf("alpha.NumTurns=%d, want 3", alpha.NumTurns)
	}
	if alpha.StartedAt.IsZero() || alpha.EndedAt.IsZero() {
		t.Fatalf("expected start/end timestamps: %+v", alpha)
	}
}

func TestRecordTeamCompleteRejectsBadResult(t *testing.T) {
	ctx := context.Background()
	svc := New(memstore.New(), WithClock(fixedClock()))
	active, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.End(active) }()

	if err := svc.RecordTeamComplete(ctx, nil); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("RecordTeamComplete(nil) err=%v, want ErrInvalidArgument", err)
	}
	if err := svc.RecordTeamComplete(ctx, &TeamResult{}); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("RecordTeamComplete(empty-team) err=%v, want ErrInvalidArgument", err)
	}
}

func TestRecordTeamFailKeepsOtherTeams(t *testing.T) {
	ctx := context.Background()
	svc := New(memstore.New(), WithClock(fixedClock()))
	active, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.End(active) }()

	if err := svc.RecordTeamFail(ctx, "alpha", errors.New("boom")); err != nil {
		t.Fatalf("RecordTeamFail: %v", err)
	}

	got, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agents["alpha"].Status != "failed" || got.Agents["alpha"].LastError != "boom" {
		t.Fatalf("unexpected alpha state: %+v", got.Agents["alpha"])
	}
	if got.Agents["beta"].Status != "pending" {
		t.Fatalf("beta status=%q, want pending", got.Agents["beta"].Status)
	}
}

func TestEndReleasesLockAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc := New(memstore.New())
	active, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.End(active); err != nil {
		t.Fatal(err)
	}
	if err := svc.End(active); err != nil {
		t.Fatal(err)
	}

	next, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatalf("Begin after End: %v", err)
	}
	_ = svc.End(next)
}

func TestConcurrentTeamTransitions(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Name: "many"}
	for i := 0; i < 10; i++ {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("team-%d", i)})
	}
	svc := New(memstore.New())
	active, err := svc.Begin(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.End(active) }()

	var wg sync.WaitGroup
	for _, team := range cfg.Agents {
		teamName := team.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.RecordTeamStart(ctx, teamName); err != nil {
				t.Errorf("RecordTeamStart(%s): %v", teamName, err)
				return
			}
			if err := svc.RecordTeamComplete(ctx, &TeamResult{
				Agent:  teamName,
				Status: "success",
				Result: "ok",
			}); err != nil {
				t.Errorf("RecordTeamComplete(%s): %v", teamName, err)
			}
		}()
	}
	wg.Wait()

	got, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, team := range cfg.Agents {
		if got.Agents[team.Name].Status != "done" {
			t.Fatalf("%s status=%q, want done", team.Name, got.Agents[team.Name].Status)
		}
	}
}

func TestSnapshotWhileActiveLockHeld(t *testing.T) {
	ctx := context.Background()
	svc := New(memstore.New())
	active, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.End(active) }()

	got, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got.RunID != active.RunID {
		t.Fatalf("RunID=%q, want %q", got.RunID, active.RunID)
	}
}

func TestSharedSnapshotReturnsStateAndReleasesLock(t *testing.T) {
	ctx := context.Background()
	svc := New(memstore.New(), WithClock(fixedClock()))
	active, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.End(active)

	state, release, err := svc.SharedSnapshot(ctx)
	if err != nil {
		t.Fatalf("SharedSnapshot: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state")
	}
	if state.Project != "test-project" {
		t.Fatalf("Project=%q, want test-project", state.Project)
	}
	if release == nil {
		t.Fatal("expected non-nil release")
	}
	release()

	// After release we can acquire exclusive lock again.
	next, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatalf("Begin after SharedSnapshot release: %v", err)
	}
	_ = svc.End(next)
}

func TestSharedSnapshotNoActiveRunReturnsNilState(t *testing.T) {
	ctx := context.Background()
	svc := New(memstore.New())

	state, release, err := svc.SharedSnapshot(ctx)
	if err != nil {
		t.Fatalf("SharedSnapshot: %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil state, got %+v", state)
	}
	if release == nil {
		t.Fatal("expected release even when no run exists")
	}
	release()

	// Lock was released so a fresh Begin succeeds.
	active, err := svc.Begin(ctx, testConfig())
	if err != nil {
		t.Fatalf("Begin after release: %v", err)
	}
	_ = svc.End(active)
}

type archiveSpy struct {
	store.Store
	archiveCalls int
}

func (s *archiveSpy) ArchiveRun(ctx context.Context, runID string) error {
	s.archiveCalls++
	return s.Store.ArchiveRun(ctx, runID)
}

func testConfig() *config.Config {
	return &config.Config{
		Name: "test-project",
		Agents: []config.Agent{
			{Name: "alpha", Lead: config.Lead{Role: "Lead A"}},
			{Name: "beta", Lead: config.Lead{Role: "Lead B"}},
		},
	}
}

func fixedClock() func() time.Time {
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}

func assertTeamTier(t *testing.T, tier *int, want int) {
	t.Helper()
	if tier == nil {
		t.Fatalf("Tier=nil, want %d", want)
	}
	if *tier != want {
		t.Fatalf("Tier=%d, want %d", *tier, want)
	}
}
