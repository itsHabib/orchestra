package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/pkg/store"
)

// RunConformance runs the shared behavior suite for a Store implementation.
func RunConformance(t *testing.T, factory func(*testing.T) store.Store) {
	t.Helper()
	t.Run("RunStateRoundTrip", func(t *testing.T) { testRunStateRoundTrip(t, factory) })
	t.Run("UpdateTeamStateSameTeamIsSerialized", func(t *testing.T) { testSameTeamUpdate(t, factory) })
	t.Run("UpdateTeamStateDifferentTeamsAllLand", func(t *testing.T) { testDifferentTeamUpdates(t, factory) })
	t.Run("RunLockExclusiveBlocksExclusive", func(t *testing.T) { testRunLock(t, factory) })
	t.Run("AgentRegistryCRUDAndSort", func(t *testing.T) { testAgentRegistry(t, factory) })
	t.Run("AgentLockSerializesCallback", func(t *testing.T) { testAgentLock(t, factory) })
	t.Run("EnvRegistryCRUDAndSort", func(t *testing.T) { testEnvRegistry(t, factory) })
}

func testRunStateRoundTrip(t *testing.T, factory func(*testing.T) store.Store) {
	t.Helper()
	s := factory(t)
	ctx := context.Background()
	state := sampleState()
	if err := s.SaveRunState(ctx, state); err != nil {
		t.Fatalf("SaveRunState: %v", err)
	}
	got, err := s.LoadRunState(ctx)
	if err != nil {
		t.Fatalf("LoadRunState: %v", err)
	}
	if got.Project != state.Project || got.Teams["alpha"].Status != "pending" {
		t.Fatalf("unexpected state: %+v", got)
	}
}

func testSameTeamUpdate(t *testing.T, factory func(*testing.T) store.Store) {
	t.Helper()
	s := factory(t)
	ctx := context.Background()
	if err := s.SaveRunState(ctx, sampleState()); err != nil {
		t.Fatal(err)
	}
	runParallel(t, 25, func(_ int) {
		err := s.UpdateTeamState(ctx, "alpha", func(ts *store.TeamState) {
			ts.InputTokens++
		})
		if err != nil {
			t.Errorf("UpdateTeamState: %v", err)
		}
	})
	got, err := s.LoadRunState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Teams["alpha"].InputTokens != 25 {
		t.Fatalf("InputTokens=%d, want 25", got.Teams["alpha"].InputTokens)
	}
}

func testDifferentTeamUpdates(t *testing.T, factory func(*testing.T) store.Store) {
	t.Helper()
	s := factory(t)
	ctx := context.Background()
	state := sampleState()
	for i := 0; i < 10; i++ {
		state.Teams[teamName(i)] = store.TeamState{Status: "pending"}
	}
	if err := s.SaveRunState(ctx, state); err != nil {
		t.Fatal(err)
	}
	runParallel(t, 10, func(i int) {
		team := teamName(i)
		err := s.UpdateTeamState(ctx, team, func(ts *store.TeamState) {
			ts.Status = "done"
		})
		if err != nil {
			t.Errorf("UpdateTeamState(%s): %v", team, err)
		}
	})
	assertTeamsDone(ctx, t, s, 10)
}

func testRunLock(t *testing.T, factory func(*testing.T) store.Store) {
	t.Helper()
	s := factory(t)
	ctx := context.Background()
	release, err := s.AcquireRunLock(ctx, store.LockExclusive)
	if err != nil {
		t.Fatalf("AcquireRunLock first: %v", err)
	}
	defer release()

	waitCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	_, err = s.AcquireRunLock(waitCtx, store.LockExclusive)
	if !errors.Is(err, store.ErrLockTimeout) {
		t.Fatalf("AcquireRunLock second err=%v, want ErrLockTimeout", err)
	}
	release()

	release2, err := s.AcquireRunLock(ctx, store.LockExclusive)
	if err != nil {
		t.Fatalf("AcquireRunLock after release: %v", err)
	}
	release2()
}

func testAgentRegistry(t *testing.T, factory func(*testing.T) store.Store) {
	t.Helper()
	s := factory(t)
	ctx := context.Background()
	if err := s.PutAgent(ctx, "b", &store.AgentRecord{Project: "p", Role: "b", AgentID: "agent-b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgent(ctx, "a", &store.AgentRecord{Project: "p", Role: "a", AgentID: "agent-a"}); err != nil {
		t.Fatal(err)
	}
	assertAgentPresent(ctx, t, s)
	assertAgentListSorted(ctx, t, s)
	if err := s.DeleteAgent(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	_, ok, err := s.GetAgent(ctx, "a")
	if err != nil || ok {
		t.Fatalf("GetAgent after delete ok=%v err=%v", ok, err)
	}
	if err := s.DeleteAgent(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteAgent missing err=%v", err)
	}
}

func testAgentLock(t *testing.T, factory func(*testing.T) store.Store) {
	t.Helper()
	s := factory(t)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithAgentLock(ctx, "k", func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	waitCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	err := s.WithAgentLock(waitCtx, "k", func(context.Context) error { return nil })
	if !errors.Is(err, store.ErrLockTimeout) {
		close(release)
		t.Fatalf("WithAgentLock err=%v, want ErrLockTimeout", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first lock callback: %v", err)
	}
}

func testEnvRegistry(t *testing.T, factory func(*testing.T) store.Store) {
	t.Helper()
	s := factory(t)
	ctx := context.Background()
	if err := s.PutEnv(ctx, "z", &store.EnvRecord{Project: "p", Name: "z", EnvID: "env-z"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEnv(ctx, "m", &store.EnvRecord{Project: "p", Name: "m", EnvID: "env-m"}); err != nil {
		t.Fatal(err)
	}
	assertEnvPresent(ctx, t, s)
	assertEnvListSorted(ctx, t, s)
	if err := s.DeleteEnv(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEnv(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteEnv missing err=%v", err)
	}
}

func sampleState() *store.RunState {
	return &store.RunState{
		Project:   "project",
		Backend:   "local",
		RunID:     "run-1",
		StartedAt: time.Unix(100, 0).UTC(),
		Teams: map[string]store.TeamState{
			"alpha": {Status: "pending"},
			"beta":  {Status: "pending"},
		},
	}
}

func runParallel(t *testing.T, count int, fn func(int)) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fn(i)
		}(i)
	}
	wg.Wait()
}

func assertTeamsDone(ctx context.Context, t *testing.T, s store.Store, count int) {
	t.Helper()
	got, err := s.LoadRunState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		team := teamName(i)
		if got.Teams[team].Status != "done" {
			t.Fatalf("%s status=%q", team, got.Teams[team].Status)
		}
	}
}

func assertAgentPresent(ctx context.Context, t *testing.T, s store.Store) {
	t.Helper()
	got, ok, err := s.GetAgent(ctx, "a")
	if err != nil || !ok {
		t.Fatalf("GetAgent ok=%v err=%v", ok, err)
	}
	if got.Key != "a" || got.AgentID != "agent-a" {
		t.Fatalf("unexpected agent: %+v", got)
	}
}

func assertAgentListSorted(ctx context.Context, t *testing.T, s store.Store) {
	t.Helper()
	list, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Key != "a" || list[1].Key != "b" {
		t.Fatalf("unsorted list: %+v", list)
	}
}

func assertEnvPresent(ctx context.Context, t *testing.T, s store.Store) {
	t.Helper()
	got, ok, err := s.GetEnv(ctx, "m")
	if err != nil || !ok {
		t.Fatalf("GetEnv ok=%v err=%v", ok, err)
	}
	if got.Key != "m" || got.EnvID != "env-m" {
		t.Fatalf("unexpected env: %+v", got)
	}
}

func assertEnvListSorted(ctx context.Context, t *testing.T, s store.Store) {
	t.Helper()
	list, err := s.ListEnvs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Key != "m" || list[1].Key != "z" {
		t.Fatalf("unsorted env list: %+v", list)
	}
}

func teamName(i int) string {
	return fmt.Sprintf("team-%d", i)
}
