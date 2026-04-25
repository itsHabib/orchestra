package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/ghhost"
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

type startTeamMAFunc func(context.Context, *config.Team, *store.RunState, io.Writer) (managedSession, <-chan spawner.Event, error)

func (r *orchestrationRun) runTeamMA(ctx context.Context, team *config.Team, state *store.RunState) (*workspace.TeamResult, error) {
	logWriter, err := r.ws.NDJSONLogWriter(team.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = logWriter.Close() }()

	teamCtx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.Defaults.TimeoutMinutes)*time.Minute)
	defer cancel()

	var session managedSession
	var events <-chan spawner.Event
	if r.startTeamMAForTest != nil {
		session, events, err = r.startTeamMAForTest(teamCtx, team, state, logWriter)
	} else {
		session, events, err = r.startTeamMA(teamCtx, team, state, logWriter)
	}
	if err != nil {
		return nil, err
	}

	for event := range events {
		r.reportMAEvent(team.Name, event)
	}

	cleanupCtx := context.WithoutCancel(ctx)
	if errors.Is(teamCtx.Err(), context.DeadlineExceeded) {
		_ = session.Cancel(cleanupCtx)
		msg := fmt.Sprintf("hard timeout after %d minutes", r.cfg.Defaults.TimeoutMinutes)
		if err := r.runService.Store().UpdateTeamState(cleanupCtx, team.Name, func(ts *store.TeamState) {
			ts.Status = "failed"
			ts.EndedAt = r.runService.Now().UTC()
			ts.LastError = msg
			ts.SessionID = session.ID()
		}); err != nil {
			return nil, err
		}
		return nil, errors.New(msg)
	}
	if errors.Is(teamCtx.Err(), context.Canceled) {
		_ = session.Cancel(cleanupCtx)
		return nil, teamCtx.Err()
	}
	if err := session.Err(); err != nil {
		return nil, err
	}

	if err := r.resolveTeamArtifact(cleanupCtx, team); err != nil {
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

func (r *orchestrationRun) startTeamMA(ctx context.Context, team *config.Team, state *store.RunState, logWriter io.Writer) (*spawner.Session, <-chan spawner.Event, error) {
	ma := r.maSpawner
	if ma == nil {
		return nil, nil, errors.New("managed-agents spawner not initialized")
	}

	agent, err := ma.EnsureAgent(ctx, r.managedAgentSpec(team))
	if err != nil {
		return nil, nil, fmt.Errorf("ensure_agent: %w", err)
	}
	env, err := ma.EnsureEnvironment(ctx, r.managedEnvSpec())
	if err != nil {
		return nil, nil, fmt.Errorf("ensure_environment: %w", err)
	}
	if err := r.recordMAHandles(ctx, team.Name, &agent, &env); err != nil {
		return nil, nil, err
	}

	resources, err := r.buildSessionResources(team, state)
	if err != nil {
		return nil, nil, fmt.Errorf("build session resources: %w", err)
	}
	pending, err := ma.StartSession(ctx, spawner.StartSessionRequest{
		Agent:         agent,
		Env:           env,
		Resources:     resources,
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

func (r *orchestrationRun) managedAgentSpec(team *config.Team) spawner.AgentSpec {
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

func (r *orchestrationRun) teamPromptMA(team *config.Team, state *store.RunState) string {
	maTeam := *team
	maTeam.Members = nil
	caps := injection.Capabilities{ArtifactPublish: r.artifactPublishSpec(team, state)}
	prompt := injection.BuildPrompt(&maTeam, r.cfg.Name, state, r.cfg, nil, "", "", caps)
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n## Managed Agents Output\n")
	b.WriteString("Return the final deliverable as a markdown summary in your last message. Do not assume a shared local .orchestra message bus is available.\n")
	return b.String()
}

// artifactPublishSpec returns the ArtifactPublishSpec for a managed-agents team
// when the team's effective repository is configured. Returns nil for
// text-only teams.
func (r *orchestrationRun) artifactPublishSpec(team *config.Team, state *store.RunState) *injection.ArtifactPublishSpec {
	repo := team.EffectiveRepository(r.cfg)
	if repo == nil || state == nil {
		return nil
	}
	spec := &injection.ArtifactPublishSpec{
		MountPath:  repo.MountPath,
		BranchName: branchName(team.Name, state.RunID),
	}
	for _, dep := range team.DependsOn {
		ts, ok := state.Teams[dep]
		if !ok || len(ts.RepositoryArtifacts) == 0 {
			continue
		}
		latest := ts.RepositoryArtifacts[len(ts.RepositoryArtifacts)-1]
		spec.UpstreamMounts = append(spec.UpstreamMounts, injection.UpstreamMount{
			TeamName:  dep,
			MountPath: upstreamMountPath(dep),
			Branch:    latest.Branch,
		})
	}
	return spec
}

// branchName composes the deterministic branch a managed-agents team is
// expected to push to: "orchestra/<team>-<run-id>". Both inputs are sanitized
// only for the purposes of branch name format — empty inputs yield an empty
// name, which signals "no artifact publish" upstream.
func branchName(team, runID string) string {
	if team == "" || runID == "" {
		return ""
	}
	return "orchestra/" + team + "-" + runID
}

// upstreamMountPath is the read-only mount path under which an upstream team's
// branch is exposed to a downstream team's session.
func upstreamMountPath(team string) string {
	return "/workspace/upstream/" + team
}

// buildSessionResources composes the github_repository ResourceRefs attached
// to a managed-agents session. Returns nil for text-only teams (no effective
// repository). For teams with a repository it returns the team's working-copy
// mount plus one mount per upstream that has recorded a repository artifact.
func (r *orchestrationRun) buildSessionResources(team *config.Team, state *store.RunState) ([]spawner.ResourceRef, error) {
	repo := team.EffectiveRepository(r.cfg)
	if repo == nil {
		return nil, nil
	}
	if r.ghPAT == "" {
		return nil, fmt.Errorf("github pat unavailable for team %q with repository %q", team.Name, repo.URL)
	}

	resources := []spawner.ResourceRef{{
		Type:               "github_repository",
		URL:                repo.URL,
		MountPath:          repo.MountPath,
		AuthorizationToken: r.ghPAT,
		Checkout:           &spawner.RepoCheckout{Type: "branch", Name: repo.DefaultBranch},
	}}

	if state == nil {
		return resources, nil
	}
	for _, dep := range team.DependsOn {
		ts, ok := state.Teams[dep]
		if !ok || len(ts.RepositoryArtifacts) == 0 {
			continue
		}
		// Use the upstream's effective repository (config-side, not the
		// stored URL) as the source of truth for cross-repo detection.
		// Stored RepositoryArtifact.URL may be missing on legacy/manually
		// edited state and must not silently route a downstream session to
		// the wrong repo. Empty stored URL is also treated as unknown.
		depTeam := r.cfg.TeamByName(dep)
		var depRepoURL string
		if depTeam != nil {
			if depRepo := depTeam.EffectiveRepository(r.cfg); depRepo != nil {
				depRepoURL = depRepo.URL
			}
		}
		if depRepoURL != repo.URL {
			continue
		}
		latest := ts.RepositoryArtifacts[len(ts.RepositoryArtifacts)-1]
		if latest.URL != "" && latest.URL != repo.URL {
			continue
		}
		resources = append(resources, spawner.ResourceRef{
			Type:               "github_repository",
			URL:                repo.URL,
			MountPath:          upstreamMountPath(dep),
			AuthorizationToken: r.ghPAT,
			Checkout:           &spawner.RepoCheckout{Type: "branch", Name: latest.Branch},
		})
	}
	return resources, nil
}

// resolveTeamArtifact runs after the session reaches end_turn. When the team
// has an effective repository, it queries the deterministic branch via the
// GitHub API and records a RepositoryArtifact in the team's state. A 404 is
// surfaced as "no branch pushed" — the team is marked failed and the caller
// reports the error via the snapshot status check. When OpenPullRequests is
// set, a PR is opened (best-effort) and its URL is recorded.
func (r *orchestrationRun) resolveTeamArtifact(ctx context.Context, team *config.Team) error {
	if r.ghClient == nil {
		return nil
	}
	repo := team.EffectiveRepository(r.cfg)
	if repo == nil {
		return nil
	}
	owner, name, err := ghhost.ParseRepoURL(repo.URL)
	if err != nil {
		return fmt.Errorf("parse repo url: %w", err)
	}

	runID, err := r.currentRunID(ctx)
	if err != nil {
		return err
	}
	branch := branchName(team.Name, runID)

	gh, err := r.ghClient.GetBranch(ctx, owner, name, branch, repo.DefaultBranch)
	if err != nil {
		if errors.Is(err, ghhost.ErrBranchNotFound) {
			return r.markTeamMissingBranch(ctx, team.Name, branch)
		}
		return fmt.Errorf("get branch: %w", err)
	}
	// Detect a real "agent created the branch but pushed no new commits" only
	// when BaseSHA was actually resolved from compare(default...branch). When
	// resolution failed (compare 404, default branch misconfigured, etc.) the
	// BaseSHA is empty and we trust the branch HEAD as the deliverable.
	if gh.BaseSHA != "" && gh.CommitSHA == gh.BaseSHA {
		return r.markTeamMissingBranch(ctx, team.Name, branch)
	}

	artifact := store.RepositoryArtifact{
		URL:        repo.URL,
		Branch:     gh.Name,
		BaseSHA:    gh.BaseSHA,
		CommitSHA:  gh.CommitSHA,
		ResolvedAt: r.runService.Now().UTC(),
	}

	r.maybeOpenPullRequest(ctx, team, repo, owner, name, runID, &artifact)

	return r.runService.Store().UpdateTeamState(ctx, team.Name, func(ts *store.TeamState) {
		ts.RepositoryArtifacts = append(ts.RepositoryArtifacts, artifact)
	})
}

func (r *orchestrationRun) currentRunID(ctx context.Context) (string, error) {
	snapshot, err := r.runService.Snapshot(ctx)
	if err != nil {
		return "", fmt.Errorf("snapshot for run id: %w", err)
	}
	if snapshot.RunID == "" {
		return "", errors.New("run id missing from state")
	}
	return snapshot.RunID, nil
}

func (r *orchestrationRun) markTeamMissingBranch(ctx context.Context, teamName, branch string) error {
	msg := "no branch pushed: " + branch
	if err := r.runService.Store().UpdateTeamState(ctx, teamName, func(ts *store.TeamState) {
		ts.Status = "failed"
		ts.LastError = msg
	}); err != nil {
		return fmt.Errorf("mark missing branch: %w", err)
	}
	return nil
}

func (r *orchestrationRun) maybeOpenPullRequest(ctx context.Context, team *config.Team, _ *config.RepositorySpec, owner, repo, runID string, artifact *store.RepositoryArtifact) {
	if r.cfg.Backend.ManagedAgents == nil || !r.cfg.Backend.ManagedAgents.OpenPullRequests {
		return
	}
	defaultBranch := team.EffectiveRepository(r.cfg).DefaultBranch
	pr, err := r.ghClient.OpenPullRequest(ctx, &ghhost.OpenPRRequest{
		Owner: owner,
		Repo:  repo,
		Head:  artifact.Branch,
		Base:  defaultBranch,
		Title: fmt.Sprintf("[orchestra] %s: %s", team.Name, runID),
		Body:  fmt.Sprintf("Opened by orchestra run %s.", runID),
	})
	switch {
	case err == nil:
		artifact.PullRequestURL = pr.URL
	case errors.Is(err, ghhost.ErrPullRequestExists):
		var existing *ghhost.PullRequestExistsError
		if errors.As(err, &existing) {
			artifact.PullRequestURL = existing.URL
		}
	default:
		r.logger.Warn("team %s: open pull request: %v", team.Name, err)
	}
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
	case spawner.UserMessageEchoEvent:
		// Skip the bootstrap prompt the orchestrator itself sent via
		// Session.Send — labeling it as a human nudge would be both
		// misleading and would leak prompt fragments to console output.
		// Only out-of-process steering deliveries (orchestra msg) reach
		// here with FromOrchestrator=false.
		if ev.FromOrchestrator {
			return
		}
		text := truncateForLog(compactForLog(ev.Text), 140)
		if text == "" {
			text = "(empty)"
		}
		r.logger.TeamMsg(teamName, "human: %s", text)
	case spawner.UserInterruptEchoEvent:
		if ev.FromOrchestrator {
			return
		}
		r.logger.TeamMsg(teamName, "human: <interrupt>")
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
