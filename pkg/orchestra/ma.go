package orchestra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/itsHabib/orchestra/internal/injection"
	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/workspace"
)

type managedSession interface {
	ID() string
	Err() error
	Cancel(context.Context) error
}

type startTeamMAFunc func(context.Context, *Team, *store.RunState, io.Writer) (managedSession, <-chan spawner.Event, error)

func (r *orchestrationRun) runTeamMA(ctx context.Context, team *Team, state *store.RunState) (*workspace.TeamResult, error) {
	logWriter, err := r.ws.NDJSONLogWriter(team.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = logWriter.Close() }()

	teamCtx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.Defaults.TimeoutMinutes)*time.Minute)
	defer cancel()

	session, events, err := r.startTeamMASession(teamCtx, team, state, logWriter)
	if err != nil {
		return nil, err
	}

	for event := range events {
		r.reportMAEvent(team.Name, event)
	}

	return r.finalizeMATeam(ctx, teamCtx, team, session)
}

func (r *orchestrationRun) startTeamMASession(ctx context.Context, team *Team, state *store.RunState, logWriter io.Writer) (managedSession, <-chan spawner.Event, error) {
	if r.startTeamMAForTest != nil {
		return r.startTeamMAForTest(ctx, team, state, logWriter)
	}
	return r.startTeamMA(ctx, team, state, logWriter)
}

func (r *orchestrationRun) finalizeMATeam(parentCtx, teamCtx context.Context, team *Team, session managedSession) (*workspace.TeamResult, error) {
	cleanupCtx := context.WithoutCancel(parentCtx)
	if errors.Is(teamCtx.Err(), context.DeadlineExceeded) {
		return nil, r.handleMATimeout(cleanupCtx, team, session)
	}
	if errors.Is(teamCtx.Err(), context.Canceled) {
		_ = session.Cancel(cleanupCtx)
		return nil, teamCtx.Err()
	}
	if err := session.Err(); err != nil {
		return nil, err
	}

	snapshot, err := r.runService.Snapshot(cleanupCtx)
	if err != nil {
		return nil, err
	}
	ts := snapshot.Teams[team.Name]
	if ts.Status != "done" {
		if ts.LastError != "" {
			return nil, errors.New(ts.LastError)
		}
		return nil, fmt.Errorf("managed-agents session ended with status %q", ts.Status)
	}
	return &workspace.TeamResult{
		Team:         team.Name,
		Status:       "success",
		Result:       ts.ResultSummary,
		CostUSD:      ts.CostUSD,
		DurationMs:   ts.DurationMs,
		SessionID:    ts.SessionID,
		InputTokens:  ts.InputTokens,
		OutputTokens: ts.OutputTokens,
	}, nil
}

func (r *orchestrationRun) handleMATimeout(ctx context.Context, team *Team, session managedSession) error {
	_ = session.Cancel(ctx)
	msg := fmt.Sprintf("hard timeout after %d minutes", r.cfg.Defaults.TimeoutMinutes)
	if err := r.runService.Store().UpdateTeamState(ctx, team.Name, func(ts *store.TeamState) {
		ts.Status = "failed"
		ts.EndedAt = r.runService.Now().UTC()
		ts.LastError = msg
		ts.SessionID = session.ID()
	}); err != nil {
		return err
	}
	return errors.New(msg)
}

func (r *orchestrationRun) startTeamMA(ctx context.Context, team *Team, state *store.RunState, logWriter io.Writer) (managedSession, <-chan spawner.Event, error) {
	ma := r.maSpawner
	if ma == nil {
		return nil, nil, errors.New("managed-agents spawner not initialized")
	}

	agent, env, err := r.ensureManagedResources(ctx, team)
	if err != nil {
		return nil, nil, err
	}
	if err := r.recordMAHandles(ctx, team.Name, &agent, &env); err != nil {
		return nil, nil, err
	}

	pending, err := ma.StartSession(ctx, spawner.StartSessionRequest{
		Agent:         agent,
		Env:           env,
		Metadata:      map[string]string{"project": r.cfg.Name, "team": team.Name},
		TeamName:      team.Name,
		LogWriter:     logWriter,
		Store:         r.runService.Store(),
		SummaryWriter: r.ws.WriteSummary,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start_session: %w", err)
	}
	if err := r.recordMASession(ctx, team.Name, pending.ID()); err != nil {
		_ = pending.Cancel(context.WithoutCancel(ctx))
		return nil, nil, err
	}

	session, events, err := pending.Stream(ctx)
	if err != nil {
		_ = pending.Cancel(context.WithoutCancel(ctx))
		return nil, nil, fmt.Errorf("events: %w", err)
	}
	if err := session.Send(ctx, &spawner.UserEvent{
		Type:    spawner.UserEventTypeMessage,
		Message: r.teamPromptMA(team, state),
	}); err != nil {
		_ = session.Cancel(context.WithoutCancel(ctx))
		return nil, nil, fmt.Errorf("send_initial: %w", err)
	}
	return session, events, nil
}

func (r *orchestrationRun) ensureManagedResources(ctx context.Context, team *Team) (spawner.AgentHandle, spawner.EnvHandle, error) {
	ma := r.maSpawner
	agent, err := ma.EnsureAgent(ctx, r.managedAgentSpec(team))
	if err != nil {
		return spawner.AgentHandle{}, spawner.EnvHandle{}, fmt.Errorf("ensure_agent: %w", err)
	}
	env, err := ma.EnsureEnvironment(ctx, r.managedEnvSpec())
	if err != nil {
		return spawner.AgentHandle{}, spawner.EnvHandle{}, fmt.Errorf("ensure_environment: %w", err)
	}
	return agent, env, nil
}

func (r *orchestrationRun) managedAgentSpec(team *Team) spawner.AgentSpec {
	return spawner.AgentSpec{
		Project:      r.cfg.Name,
		Role:         team.Name,
		Name:         team.Lead.Role,
		Model:        managedAgentsModel(r.teamModel(team)),
		SystemPrompt: "You are an Orchestra managed-agents team lead. Work only on the user's assigned task and return a concise final markdown summary.",
		Tools: []spawner.Tool{
			{Name: "bash"},
			{Name: "read"},
			{Name: "write"},
			{Name: "edit"},
			{Name: "grep"},
			{Name: "glob"},
		},
		Metadata: map[string]string{"team": team.Name},
	}
}

func (r *orchestrationRun) managedEnvSpec() spawner.EnvSpec {
	return spawner.EnvSpec{
		Project: r.cfg.Name,
		Name:    "default",
		Networking: spawner.NetworkSpec{
			Type:                 "unrestricted",
			AllowPackageManagers: true,
			AllowMCPServers:      true,
		},
	}
}

func (r *orchestrationRun) teamPromptMA(team *Team, state *store.RunState) string {
	maTeam := *team
	maTeam.Members = nil
	prompt := injection.BuildPrompt(&maTeam, r.cfg.Name, state, r.cfg, nil, "", "")
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n## Managed Agents Output\n")
	b.WriteString("Return the final deliverable as a markdown summary in your last message. Do not assume a shared local .orchestra message bus is available.\n")
	return b.String()
}

func (r *orchestrationRun) recordMAHandles(ctx context.Context, teamName string, agent *spawner.AgentHandle, _ *spawner.EnvHandle) error {
	return r.runService.Store().UpdateTeamState(ctx, teamName, func(ts *store.TeamState) {
		ts.AgentID = agent.ID
		ts.AgentVersion = agent.Version
	})
}

func (r *orchestrationRun) recordMASession(ctx context.Context, teamName, sessionID string) error {
	return r.runService.Store().UpdateTeamState(ctx, teamName, func(ts *store.TeamState) {
		ts.SessionID = sessionID
	})
}

func (r *orchestrationRun) reportMAEvent(teamName string, event spawner.Event) {
	switch ev := event.(type) {
	case spawner.AgentMessageEvent:
		if ev.Text != "" {
			r.logger.TeamMsg(teamName, "%s", truncateForLog(compactForLog(ev.Text), 140))
		}
	case spawner.SessionStatusRunningEvent:
		r.logger.TeamMsg(teamName, "managed-agents session running")
	case spawner.SessionStatusIdleEvent:
		r.logger.TeamMsg(teamName, "managed-agents session idle (%s)", ev.Status.StopReason.Type)
	case spawner.SpanModelRequestEndEvent:
		r.logger.TeamMsg(teamName, "tokens %s in / %s out", fmtTokens(ev.Usage.InputTokens), fmtTokens(ev.Usage.OutputTokens))
	}
}

func managedAgentsModel(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "", "sonnet":
		return "claude-sonnet-4-6"
	case "opus":
		return "claude-opus-4-7"
	default:
		return model
	}
}

func compactForLog(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
