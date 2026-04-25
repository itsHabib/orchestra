package orchestra

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	olog "github.com/itsHabib/orchestra/internal/log"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/memstore"
	"github.com/itsHabib/orchestra/internal/workspace"
)

func TestTeamPromptMAIgnoresMembersAndMessageBus(t *testing.T) {
	r := &orchestrationRun{cfg: &Config{Name: "p"}}
	team := &Team{
		Name: "alpha",
		Lead: Lead{Role: "Research Lead"},
		Members: []Member{
			{Role: "Analyst", Focus: "notes"},
		},
		Tasks: []Task{{Summary: "Summarize input"}},
	}

	prompt := r.teamPromptMA(team, &store.RunState{Teams: map[string]store.TeamState{}})
	for _, forbidden := range []string{"## Your Team", "TeamCreate", "Message Bus", "/loop"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("MA prompt contains %q:\n%s", forbidden, prompt)
		}
	}
	if !strings.Contains(prompt, "Managed Agents Output") {
		t.Fatalf("MA output instruction missing:\n%s", prompt)
	}
}

func TestRunTeamMATimeoutMarksFailedAndCancels(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{
		Project: "p",
		Backend: BackendManagedAgents,
		Teams: map[string]store.TeamState{
			"alpha": {Status: "running", InputTokens: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatal(err)
	}
	team := &Team{Name: "alpha", Lead: Lead{Role: "Lead"}, Tasks: []Task{{Summary: "x"}}}
	fake := &fakeManagedSession{id: "sess_timeout"}
	r := &orchestrationRun{
		cfg:        &Config{Name: "p", Defaults: Defaults{TimeoutMinutes: 0}},
		runService: runsvc.New(st),
		ws:         ws,
		startTeamMAForTest: func(ctx context.Context, _ *Team, _ *store.RunState, _ io.Writer) (managedSession, <-chan spawner.Event, error) {
			ch := make(chan spawner.Event)
			go func() {
				<-ctx.Done()
				close(ch)
			}()
			return fake, ch, nil
		},
	}

	_, err = r.runTeamMA(ctx, team, &store.RunState{})
	if err == nil || !strings.Contains(err.Error(), "hard timeout after 0 minutes") {
		t.Fatalf("runTeamMA err=%v, want timeout", err)
	}
	if !fake.canceled {
		t.Fatal("expected session cancel on timeout")
	}
	state, err := st.LoadRunState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alpha := state.Teams["alpha"]
	if alpha.Status != "failed" || alpha.SessionID != "sess_timeout" || !strings.Contains(alpha.LastError, "timeout") {
		t.Fatalf("unexpected alpha state: %+v", alpha)
	}
	if alpha.InputTokens != 10 {
		t.Fatalf("partial counters were not preserved: %+v", alpha)
	}
}

func TestRunTiers_MAPropagatesUpstreamSummaryToDownstream(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{
		Project: "p",
		Backend: BackendManagedAgents,
		Teams: map[string]store.TeamState{
			"planner": {Status: "pending"},
			"analyst": {Status: "pending"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Name:     "p",
		Backend:  Backend{Kind: BackendManagedAgents},
		Defaults: Defaults{TimeoutMinutes: 5},
		Teams: []Team{
			{Name: "planner", Lead: Lead{Role: "Planner"}, Tasks: []Task{{Summary: "plan"}}},
			{Name: "analyst", Lead: Lead{Role: "Analyst"}, DependsOn: []string{"planner"}, Tasks: []Task{{Summary: "analyze"}}},
		},
	}
	cfg.ResolveDefaults()

	var (
		mu              sync.Mutex
		analystPrompt   string
		analystSawState *store.RunState
	)
	r := &orchestrationRun{
		cfg:        cfg,
		logger:     olog.New(),
		runService: runsvc.New(st),
		ws:         ws,
	}
	r.startTeamMAForTest = func(ctx context.Context, team *Team, state *store.RunState, _ io.Writer) (managedSession, <-chan spawner.Event, error) {
		if team.Name == "analyst" {
			mu.Lock()
			analystPrompt = r.teamPromptMA(team, state)
			analystSawState = state
			mu.Unlock()
		}
		if err := st.UpdateTeamState(ctx, team.Name, func(ts *store.TeamState) {
			ts.Status = "done"
			ts.ResultSummary = team.Name + " summary"
		}); err != nil {
			return nil, nil, err
		}
		ch := make(chan spawner.Event)
		close(ch)
		return &fakeManagedSession{id: "sess_" + team.Name}, ch, nil
	}

	if err := r.runTiers(ctx, [][]string{{"planner"}, {"analyst"}}); err != nil {
		t.Fatalf("runTiers: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if analystSawState == nil {
		t.Fatal("analyst substitute never invoked")
	}
	plannerInState := analystSawState.Teams["planner"]
	if plannerInState.Status != "done" || plannerInState.ResultSummary != "planner summary" {
		t.Fatalf("analyst saw planner state=%+v, want done+summary", plannerInState)
	}
	if !strings.Contains(analystPrompt, "planner summary") {
		t.Fatalf("analyst prompt missing planner summary:\n%s", analystPrompt)
	}
}

func TestRunTier_MAStartsTeamsInParallel(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{
		Project: "p",
		Backend: BackendManagedAgents,
		Teams: map[string]store.TeamState{
			"a": {Status: "pending"},
			"b": {Status: "pending"},
			"c": {Status: "pending"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Name:     "p",
		Backend:  Backend{Kind: BackendManagedAgents},
		Defaults: Defaults{TimeoutMinutes: 5},
		Teams: []Team{
			{Name: "a", Lead: Lead{Role: "A"}, Tasks: []Task{{Summary: "x"}}},
			{Name: "b", Lead: Lead{Role: "B"}, Tasks: []Task{{Summary: "x"}}},
			{Name: "c", Lead: Lead{Role: "C"}, Tasks: []Task{{Summary: "x"}}},
		},
	}
	cfg.ResolveDefaults()

	var arrived atomic.Int32
	const expected = 3
	barrier := make(chan struct{})

	r := &orchestrationRun{
		cfg:        cfg,
		logger:     olog.New(),
		runService: runsvc.New(st),
		ws:         ws,
	}
	r.startTeamMAForTest = func(ctx context.Context, team *Team, _ *store.RunState, _ io.Writer) (managedSession, <-chan spawner.Event, error) {
		if arrived.Add(1) == expected {
			close(barrier)
		}
		select {
		case <-barrier:
		case <-time.After(2 * time.Second):
			return nil, nil, errors.New("timed out waiting for sibling teams to start in parallel")
		}
		if err := st.UpdateTeamState(ctx, team.Name, func(ts *store.TeamState) {
			ts.Status = "done"
			ts.ResultSummary = team.Name
		}); err != nil {
			return nil, nil, err
		}
		ch := make(chan spawner.Event)
		close(ch)
		return &fakeManagedSession{id: "sess_" + team.Name}, ch, nil
	}

	if err := r.runTier(ctx, 0, []string{"a", "b", "c"}); err != nil {
		t.Fatalf("runTier: %v", err)
	}
	if got := arrived.Load(); got != expected {
		t.Fatalf("teams that reached barrier=%d, want %d", got, expected)
	}
}

func TestRunTiers_MATierFailureSkipsDownstream(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	if err := st.SaveRunState(ctx, &store.RunState{
		Project: "p",
		Backend: BackendManagedAgents,
		Teams: map[string]store.TeamState{
			"a": {Status: "pending"},
			"b": {Status: "pending"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Name:     "p",
		Backend:  Backend{Kind: BackendManagedAgents},
		Defaults: Defaults{TimeoutMinutes: 5},
		Teams: []Team{
			{Name: "a", Lead: Lead{Role: "A"}, Tasks: []Task{{Summary: "x"}}},
			{Name: "b", Lead: Lead{Role: "B"}, DependsOn: []string{"a"}, Tasks: []Task{{Summary: "x"}}},
		},
	}
	cfg.ResolveDefaults()

	var (
		mu      sync.Mutex
		invoked = map[string]bool{}
	)
	r := &orchestrationRun{
		cfg:        cfg,
		logger:     olog.New(),
		runService: runsvc.New(st),
		ws:         ws,
	}
	r.startTeamMAForTest = func(ctx context.Context, team *Team, _ *store.RunState, _ io.Writer) (managedSession, <-chan spawner.Event, error) {
		mu.Lock()
		invoked[team.Name] = true
		mu.Unlock()
		if team.Name == "a" {
			return nil, nil, errors.New("simulated failure")
		}
		if err := st.UpdateTeamState(ctx, team.Name, func(ts *store.TeamState) { ts.Status = "done" }); err != nil {
			return nil, nil, err
		}
		ch := make(chan spawner.Event)
		close(ch)
		return &fakeManagedSession{id: "sess_" + team.Name}, ch, nil
	}

	if err := r.runTiers(ctx, [][]string{{"a"}, {"b"}}); err == nil {
		t.Fatal("runTiers: expected failure on tier 0, got nil")
	}

	mu.Lock()
	defer mu.Unlock()
	if !invoked["a"] {
		t.Fatal("team a substitute never invoked")
	}
	if invoked["b"] {
		t.Fatal("team b substitute invoked despite tier 0 failure")
	}

	state, err := st.LoadRunState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Teams["a"].Status != "failed" {
		t.Fatalf("team a status=%q, want failed", state.Teams["a"].Status)
	}
	if state.Teams["b"].Status != "pending" {
		t.Fatalf("team b status=%q, want pending", state.Teams["b"].Status)
	}
}

type fakeManagedSession struct {
	id       string
	err      error
	canceled bool
}

func (s *fakeManagedSession) ID() string { return s.id }
func (s *fakeManagedSession) Err() error { return s.err }

func (s *fakeManagedSession) Cancel(context.Context) error {
	s.canceled = true
	return nil
}
