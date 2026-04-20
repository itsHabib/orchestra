package cmd

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itsHabib/orchestra/internal/config"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/itsHabib/orchestra/internal/workspace"
	"github.com/itsHabib/orchestra/pkg/spawner"
	"github.com/itsHabib/orchestra/pkg/store"
	"github.com/itsHabib/orchestra/pkg/store/memstore"
)

func TestTeamPromptMAIgnoresMembersAndMessageBus(t *testing.T) {
	r := &orchestrationRun{cfg: &config.Config{Name: "p"}}
	team := &config.Team{
		Name: "alpha",
		Lead: config.Lead{Role: "Research Lead"},
		Members: []config.Member{
			{Role: "Analyst", Focus: "notes"},
		},
		Tasks: []config.Task{{Summary: "Summarize input"}},
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
		Backend: "managed_agents",
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
	team := &config.Team{Name: "alpha", Lead: config.Lead{Role: "Lead"}, Tasks: []config.Task{{Summary: "x"}}}
	fake := &fakeManagedSession{id: "sess_timeout"}
	r := &orchestrationRun{
		cfg:        &config.Config{Name: "p", Defaults: config.Defaults{TimeoutMinutes: 0}},
		runService: runsvc.New(st),
		ws:         ws,
		startTeamMAForTest: func(ctx context.Context, _ *config.Team, _ *store.RunState, _ io.Writer) (managedSession, <-chan spawner.Event, error) {
			ch := make(chan spawner.Event)
			go func() {
				<-ctx.Done()
				close(ch)
			}()
			return fake, ch, nil
		},
	}

	_, err = r.runTeamMA(ctx, team, &store.RunState{})
	if err == nil || !strings.Contains(err.Error(), "timeout: no events for 0 minutes") {
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
