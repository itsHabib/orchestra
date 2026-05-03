package orchestra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/itsHabib/orchestra/internal/artifacts"
	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/customtools"
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
	// Send delivers a user event into the session. The dispatcher uses this
	// to relay user.custom_tool_result events back after a host-side custom
	// tool handler runs. Adding it here (rather than a separate interface)
	// keeps the engine's MA codepath using one consistent value.
	Send(context.Context, *spawner.UserEvent) error
}

type startTeamMAFunc func(context.Context, *Team, *store.RunState, io.Writer) (managedSession, <-chan spawner.Event, error)

func (r *orchestrationRun) runTeamMA(ctx context.Context, tierIdx int, team *Team, state *store.RunState) (*workspace.AgentResult, error) {
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

	for ev := range events {
		r.reportMAEvent(tierIdx, team.Name, ev)
		r.dispatchCustomToolEvent(teamCtx, tierIdx, team.Name, session, ev)
	}

	return r.finalizeMATeam(ctx, teamCtx, team, session)
}

// dispatchCustomToolEvent routes an agent.custom_tool_use event to a host-side
// handler from the customtools registry, then ships the handler's result back
// as a user.custom_tool_result so the agent's loop can advance. Non-custom
// events are no-ops here — reportMAEvent already logged them. An unknown
// tool, a parse failure, or a handler error all collapse to is_error=true on
// the result event so the agent gets actionable feedback rather than hanging
// on a never-arriving response.
func (r *orchestrationRun) dispatchCustomToolEvent(ctx context.Context, tierIdx int, teamName string, session managedSession, event spawner.Event) {
	cev, ok := event.(spawner.AgentCustomToolUseEvent)
	if !ok {
		return
	}
	r.handleCustomToolUse(ctx, tierIdx, teamName, session, &cev)
}

func (r *orchestrationRun) handleCustomToolUse(ctx context.Context, tierIdx int, teamName string, session managedSession, ev *spawner.AgentCustomToolUseEvent) {
	toolUseID := spawner.EventID(ev.ToolUse.ID)
	handler, found := customtools.Lookup(ev.ToolUse.Name)
	if !found {
		errMsg := fmt.Sprintf("no handler registered for custom tool %q", ev.ToolUse.Name)
		r.emitTeamMessage(tierIdx, teamName, "custom_tool_use(%s) → %s", ev.ToolUse.Name, errMsg)
		r.sendCustomToolResult(ctx, tierIdx, teamName, session, toolUseID, nil, errMsg)
		return
	}

	input, err := marshalToolInput(ev.ToolUse.Input)
	if err != nil {
		errMsg := fmt.Sprintf("marshal input: %s", err)
		r.emitTeamMessage(tierIdx, teamName, "custom_tool_use(%s) → %s", ev.ToolUse.Name, errMsg)
		r.sendCustomToolResult(ctx, tierIdx, teamName, session, toolUseID, nil, errMsg)
		return
	}

	runCtx := r.customToolRunContext(ctx)
	result, handleErr := handler.Handle(ctx, runCtx, teamName, input)
	if handleErr != nil {
		errMsg := handleErr.Error()
		r.emitTeamMessage(tierIdx, teamName, "custom_tool_use(%s) failed: %s", ev.ToolUse.Name, errMsg)
		r.sendCustomToolResult(ctx, tierIdx, teamName, session, toolUseID, nil, errMsg)
		return
	}
	r.emitTeamMessage(tierIdx, teamName, "custom_tool_use(%s) ok", ev.ToolUse.Name)
	r.sendCustomToolResult(ctx, tierIdx, teamName, session, toolUseID, result, "")
}

// sendCustomToolResult pushes the user.custom_tool_result event back into the
// session. The send error path emits a warn-level event (not an EventError —
// the team's own error path handles a fatally broken session) so a transient
// network blip during a tool result write is visible in the run log without
// terminating the team. The agent will eventually re-stream a stop_reason
// idle-with-error if MA notices the missing result.
func (r *orchestrationRun) sendCustomToolResult(ctx context.Context, tierIdx int, teamName string, session managedSession, toolUseID spawner.EventID, result json.RawMessage, errMsg string) {
	if session == nil {
		return
	}
	sendErr := session.Send(ctx, &spawner.UserEvent{
		Type: spawner.UserEventTypeCustomToolResult,
		CustomToolResult: &spawner.CustomToolResult{
			ToolUseID: toolUseID,
			Result:    result,
			Error:     errMsg,
		},
	})
	if sendErr != nil {
		r.emit(Event{
			Kind:    EventWarn,
			Tier:    tierIdx,
			Team:    teamName,
			Message: fmt.Sprintf("custom_tool_result send: %s", sendErr),
			At:      time.Now(),
		})
	}
}

// customToolRunContext bundles the engine state custom-tool handlers need.
// Constructed per dispatch so handlers see the live store and notifier even
// if either is swapped mid-run (none are today, but this keeps the contract
// honest). Phase is snapshot at construction time — UpdateAgentState
// serializes per-team writes so a phase change between this snapshot and
// Handle's signal-state write is impossible in practice; a static value is
// simpler than a closure.
func (r *orchestrationRun) customToolRunContext(ctx context.Context) *customtools.RunContext {
	var (
		runID string
		phase string
	)
	if r.runService != nil {
		if snapshot, err := r.runService.Snapshot(ctx); err == nil && snapshot != nil {
			runID = snapshot.RunID
			phase = snapshot.Phase
		}
	}
	var st store.Store
	if r.runService != nil {
		st = r.runService.Store()
	}
	return &customtools.RunContext{
		Store:     st,
		Notifier:  r.notifier,
		RunID:     runID,
		Now:       r.runServiceNow,
		Artifacts: r.artifactStore(),
		Phase:     phase,
	}
}

// artifactStore returns the run's artifact persistence store rooted under
// the workspace's .orchestra directory. r.ws.Path IS the .orchestra dir
// (see internal/workspace.Ensure), so the artifact root is one level deeper.
// Returns nil when the workspace isn't wired — handlers tolerate that and
// drop artifacts silently rather than erroring.
func (r *orchestrationRun) artifactStore() artifacts.Store {
	if r.ws == nil || r.ws.Path == "" {
		return nil
	}
	return artifacts.NewFileStore(filepath.Join(r.ws.Path, "artifacts"))
}

// runServiceNow returns the engine's clock when wired through the run
// service, falling back to time.Now().UTC(). Keeps signal_at and
// notification timestamps consistent with the rest of the run state.
func (r *orchestrationRun) runServiceNow() time.Time {
	if r.runService != nil {
		return r.runService.Now().UTC()
	}
	return time.Now().UTC()
}

// marshalToolInput rebuilds the json.RawMessage handlers expect from the
// engine-translated `any` value the spawner already decoded. Round-tripping
// through json.Marshal is cheap (input payloads are small) and keeps the
// Handler API narrow — handlers don't need to know that the engine eagerly
// decodes Input on the way in.
//
// Any non-nil, non-RawMessage value goes through json.Marshal. That includes
// Go strings: a JSON string scalar like `"hello"` is decoded by the spawner
// into a Go string `hello`, and a previous shortcut that returned that as
// raw bytes would strip the JSON quoting and produce invalid JSON for the
// handler. Always re-marshal.
func marshalToolInput(in any) (json.RawMessage, error) {
	if in == nil {
		return json.RawMessage(`{}`), nil
	}
	if raw, ok := in.(json.RawMessage); ok {
		if len(raw) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return raw, nil
	}
	out, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return out, nil
}

func (r *orchestrationRun) startTeamMASession(ctx context.Context, team *Team, state *store.RunState, logWriter io.Writer) (managedSession, <-chan spawner.Event, error) {
	if r.startTeamMAForTest != nil {
		return r.startTeamMAForTest(ctx, team, state, logWriter)
	}
	return r.startTeamMA(ctx, team, state, logWriter)
}

func (r *orchestrationRun) finalizeMATeam(parentCtx, teamCtx context.Context, team *Team, session managedSession) (*workspace.AgentResult, error) {
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

	if err := r.resolveTeamArtifact(cleanupCtx, team); err != nil {
		return nil, err
	}

	snapshot, err := r.runService.Snapshot(cleanupCtx)
	if err != nil {
		return nil, err
	}
	ts := snapshot.Agents[team.Name]
	if ts.Status != "done" {
		if ts.LastError != "" {
			return nil, errors.New(ts.LastError)
		}
		return nil, fmt.Errorf("managed-agents session ended with status %q", ts.Status)
	}
	// NumTurns is propagated for symmetry with the local backend even
	// though MA doesn't currently emit per-turn signals — leaving it off
	// would let RecordTeamComplete zero out anything a future event
	// counter manages to record.
	return &workspace.AgentResult{
		Agent:        team.Name,
		Status:       "success",
		Result:       ts.ResultSummary,
		CostUSD:      ts.CostUSD,
		DurationMs:   ts.DurationMs,
		SessionID:    ts.SessionID,
		InputTokens:  ts.InputTokens,
		OutputTokens: ts.OutputTokens,
		NumTurns:     ts.NumTurns,
	}, nil
}

func (r *orchestrationRun) handleMATimeout(ctx context.Context, team *Team, session managedSession) error {
	_ = session.Cancel(ctx)
	msg := fmt.Sprintf("hard timeout after %d minutes", r.cfg.Defaults.TimeoutMinutes)
	if err := r.runService.Store().UpdateAgentState(ctx, team.Name, func(ts *store.AgentState) {
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
	tools := []spawner.Tool{
		{Name: "bash"},
		{Name: "read"},
		{Name: "write"},
		{Name: "edit"},
		{Name: "grep"},
		{Name: "glob"},
	}
	tools = append(tools, agentsToolCopy(r.teamCustomTools[team.Name])...)
	return spawner.AgentSpec{
		Project:      r.cfg.Name,
		Role:         team.Name,
		Name:         team.Lead.Role,
		Model:        managedAgentsModel(r.teamModel(team)),
		SystemPrompt: "You are an Orchestra managed-agents team lead. Work only on the user's assigned task and return a concise final markdown summary.",
		Tools:        tools,
		Skills:       agentsSkillCopy(r.teamSkills[team.Name]),
		Metadata:     map[string]string{"team": team.Name},
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
func (r *orchestrationRun) artifactPublishSpec(team *Team, state *store.RunState) *injection.ArtifactPublishSpec {
	repo := team.EffectiveRepository(r.cfg)
	if repo == nil || state == nil {
		return nil
	}
	spec := &injection.ArtifactPublishSpec{
		MountPath:  repo.MountPath,
		BranchName: branchName(team.Name, state.RunID),
	}
	for _, dep := range team.DependsOn {
		ts, ok := state.Agents[dep]
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
func (r *orchestrationRun) buildSessionResources(team *Team, state *store.RunState) ([]spawner.ResourceRef, error) {
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
		ts, ok := state.Agents[dep]
		if !ok || len(ts.RepositoryArtifacts) == 0 {
			continue
		}
		// Use the upstream's effective repository (config-side, not the
		// stored URL) as the source of truth for cross-repo detection.
		// Stored RepositoryArtifact.URL may be missing on legacy/manually
		// edited state and must not silently route a downstream session to
		// the wrong repo. Empty stored URL is also treated as unknown.
		depTeam := r.cfg.AgentByName(dep)
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
func (r *orchestrationRun) resolveTeamArtifact(ctx context.Context, team *Team) error {
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

	return r.runService.Store().UpdateAgentState(ctx, team.Name, func(ts *store.AgentState) {
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
	if err := r.runService.Store().UpdateAgentState(ctx, teamName, func(ts *store.AgentState) {
		ts.Status = "failed"
		ts.LastError = msg
	}); err != nil {
		return fmt.Errorf("mark missing branch: %w", err)
	}
	return nil
}

func (r *orchestrationRun) maybeOpenPullRequest(ctx context.Context, team *Team, _ *config.RepositorySpec, owner, repo, runID string, artifact *store.RepositoryArtifact) {
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
		r.emitWarn("team %s: open pull request: %v", team.Name, err)
	}
}

func (r *orchestrationRun) recordMAHandles(ctx context.Context, teamName string, agent *spawner.AgentHandle, _ *spawner.EnvHandle) error {
	return r.runService.Store().UpdateAgentState(ctx, teamName, func(ts *store.AgentState) {
		ts.AgentID = agent.ID
		ts.AgentVersion = agent.Version
	})
}

func (r *orchestrationRun) recordMASession(ctx context.Context, teamName, sessionID string) error {
	return r.runService.Store().UpdateAgentState(ctx, teamName, func(ts *store.AgentState) {
		ts.SessionID = sessionID
	})
}

func (r *orchestrationRun) reportMAEvent(tierIdx int, teamName string, event spawner.Event) {
	switch ev := event.(type) {
	case spawner.AgentMessageEvent:
		if ev.Text != "" {
			r.emitTeamMessage(tierIdx, teamName, "%s", truncateForLog(compactForLog(ev.Text)))
		}
	case spawner.AgentToolUseEvent:
		input := compactForLog(fmt.Sprintf("%v", ev.ToolUse.Input))
		r.recordToolName(ev.ToolUse.ID, ev.ToolUse.Name)
		r.emit(Event{
			Kind:    EventToolCall,
			Tier:    tierIdx,
			Team:    teamName,
			Tool:    ev.ToolUse.Name,
			Message: truncateForLog(input),
			At:      time.Now(),
		})
	case spawner.AgentToolResultEvent:
		result := compactForLog(fmt.Sprintf("%v", ev.ToolResult.Content))
		r.emit(Event{
			Kind:    EventToolResult,
			Tier:    tierIdx,
			Team:    teamName,
			Tool:    r.takeToolName(ev.ToolResult.ToolUseID),
			Message: truncateForLog(result),
			At:      time.Now(),
		})
	case spawner.SessionStatusRunningEvent:
		r.emitTeamMessage(tierIdx, teamName, "managed-agents session running")
	case spawner.SessionStatusIdleEvent:
		r.emitTeamMessage(tierIdx, teamName, "managed-agents session idle (%s)", ev.Status.StopReason.Type)
	case spawner.SpanModelRequestEndEvent:
		r.emitTeamMessage(tierIdx, teamName, "tokens %s in / %s out", fmtTokens(ev.Usage.InputTokens), fmtTokens(ev.Usage.OutputTokens))
	case spawner.UserMessageEchoEvent:
		// Skip the bootstrap prompt the orchestrator itself sent via
		// Session.Send — labeling it as a human nudge would be both
		// misleading and would leak prompt fragments to console output.
		// Only out-of-process steering deliveries (orchestra msg) reach
		// here with FromOrchestrator=false.
		if ev.FromOrchestrator {
			return
		}
		text := truncateForLog(compactForLog(ev.Text))
		if text == "" {
			text = "(empty)"
		}
		r.emitTeamMessage(tierIdx, teamName, "human: %s", text)
	case spawner.UserInterruptEchoEvent:
		if ev.FromOrchestrator {
			return
		}
		r.emitTeamMessage(tierIdx, teamName, "human: <interrupt>")
	}
}

// recordToolName remembers the tool name associated with a ToolUseID so a
// later AgentToolResultEvent (which carries only the ID) can populate
// EventToolResult.Tool. Concurrent emit paths from sibling teams in the
// same tier share the map under toolNamesMu.
func (r *orchestrationRun) recordToolName(useID, name string) {
	if useID == "" || name == "" {
		return
	}
	r.toolNamesMu.Lock()
	defer r.toolNamesMu.Unlock()
	if r.toolNamesByUseID == nil {
		r.toolNamesByUseID = make(map[string]string)
	}
	r.toolNamesByUseID[useID] = name
}

// takeToolName returns the tool name previously recorded for useID and
// deletes the entry to keep the map bounded across long-lived runs.
// Returns "" when the result arrives without a matching call (out-of-order
// or dropped event); EventToolResult.Tool is documented as best-effort.
func (r *orchestrationRun) takeToolName(useID string) string {
	if useID == "" {
		return ""
	}
	r.toolNamesMu.Lock()
	defer r.toolNamesMu.Unlock()
	name := r.toolNamesByUseID[useID]
	delete(r.toolNamesByUseID, useID)
	return name
}

func managedAgentsModel(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "", "sonnet":
		return "claude-sonnet-4-6"
	case "opus":
		return "claude-opus-4-7"
	case "haiku":
		return "claude-haiku-4-5-20251001"
	default:
		return model
	}
}

func compactForLog(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// maxMALogLine bounds how much of a managed-agents agent or tool payload
// is rendered into a single log line. Long enough to be useful, short
// enough that one verbose tool call doesn't drown out the run.
const maxMALogLine = 140

func truncateForLog(s string) string {
	if len(s) <= maxMALogLine {
		return s
	}
	return s[:maxMALogLine] + "..."
}
