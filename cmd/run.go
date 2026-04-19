package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/dag"
	"github.com/itsHabib/orchestra/internal/injection"
	olog "github.com/itsHabib/orchestra/internal/log"
	"github.com/itsHabib/orchestra/internal/messaging"
	"github.com/itsHabib/orchestra/internal/workspace"
	"github.com/itsHabib/orchestra/pkg/spawner"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <config.yaml>",
	Short: "Full orchestration: init, DAG, spawn tiers, collect, summary",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		logger := olog.New()

		cfg, warnings, err := config.Load(args[0])
		if err != nil {
			logger.Error("Config error: %s", err)
			os.Exit(1)
		}
		for _, w := range warnings {
			logger.Warn("%s", w)
		}

		if err := runOrchestration(context.Background(), cfg, logger); err != nil {
			logger.Error("Orchestration failed: %s", err)
			os.Exit(1)
		}
	},
}

func runOrchestration(ctx context.Context, cfg *config.Config, logger *olog.Logger) error {
	wallStart := time.Now()

	releaseRunLock, err := workspace.AcquireRunLock(ctx, ".orchestra", workspace.LockExclusive)
	if err != nil {
		return fmt.Errorf("acquiring run lock: %w", err)
	}
	defer releaseRunLock()

	if err := workspace.ArchiveExistingRun(ctx, ".orchestra"); err != nil {
		return fmt.Errorf("archiving previous run: %w", err)
	}

	run, tiers, err := newOrchestrationRun(ctx, cfg, logger)
	if err != nil {
		return err
	}

	coordHandle, closeCoordinatorLog, err := run.startCoordinator(ctx, tiers)
	if err != nil {
		return err
	}
	if closeCoordinatorLog != nil {
		defer closeCoordinatorLog()
	}

	if err := run.runTiers(ctx, tiers); err != nil {
		return err
	}
	if err := run.stopCoordinator(coordHandle); err != nil {
		return err
	}

	// 7. Print summary
	printSummary(ctx, run.ws, cfg, time.Since(wallStart))
	return nil
}

type orchestrationRun struct {
	cfg          *config.Config
	logger       *olog.Logger
	ws           *workspace.Workspace
	bus          *messaging.Bus
	participants []messaging.Participant
	inboxLookup  map[string]string
}

type tierResult struct {
	name string
	res  *workspace.TeamResult
	err  error
}

func newOrchestrationRun(ctx context.Context, cfg *config.Config, logger *olog.Logger) (*orchestrationRun, [][]string, error) {
	ws, err := workspace.InitContext(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("init workspace: %w", err)
	}
	logger.Success("Workspace initialized at %s", ws.Path)

	tiers, err := dag.BuildTiers(cfg.Teams)
	if err != nil {
		return nil, nil, fmt.Errorf("building DAG: %w", err)
	}
	logger.Info("DAG: %d tiers", len(tiers))

	bus, participants, inboxLookup, err := initMessageBus(ws, cfg)
	if err != nil {
		return nil, nil, err
	}
	logger.Success("Message bus initialized at %s", bus.Path())

	if err := os.MkdirAll(filepath.Join(ws.Path, "coordinator"), 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating coordinator decisions directory: %w", err)
	}

	return &orchestrationRun{
		cfg:          cfg,
		logger:       logger,
		ws:           ws,
		bus:          bus,
		participants: participants,
		inboxLookup:  inboxLookup,
	}, tiers, nil
}

func initMessageBus(ws *workspace.Workspace, cfg *config.Config) (*messaging.Bus, []messaging.Participant, map[string]string, error) {
	names := teamNames(cfg.Teams)
	bus := messaging.NewBus(ws.MessagesPath())
	if err := bus.InitInboxes(names); err != nil {
		return nil, nil, nil, fmt.Errorf("init message bus: %w", err)
	}
	participants := messaging.BuildParticipants(names)
	return bus, participants, inboxLookup(participants), nil
}

func teamNames(teams []config.Team) []string {
	names := make([]string, len(teams))
	for i := range teams {
		names[i] = teams[i].Name
	}
	return names
}

func inboxLookup(participants []messaging.Participant) map[string]string {
	lookup := make(map[string]string, len(participants))
	for _, p := range participants {
		lookup[p.Name] = p.FolderName()
	}
	return lookup
}

func (r *orchestrationRun) startCoordinator(ctx context.Context, tiers [][]string) (*spawner.CoordinatorHandle, func(), error) {
	if !r.cfg.Coordinator.Enabled {
		return nil, nil, nil
	}

	coordPrompt := injection.BuildCoordinatorPrompt(r.cfg, tiers, r.bus.Path(), r.participants)
	coordLogWriter, err := r.ws.LogWriter("coordinator")
	if err != nil {
		return nil, nil, fmt.Errorf("opening coordinator log: %w", err)
	}

	coordHandle, err := spawner.SpawnBackground(ctx, &spawner.SpawnOpts{
		TeamName:       "coordinator",
		Prompt:         coordPrompt,
		Model:          r.cfg.Coordinator.Model,
		MaxTurns:       r.cfg.Coordinator.MaxTurns,
		PermissionMode: r.cfg.Defaults.PermissionMode,
		TimeoutMinutes: r.cfg.Defaults.TimeoutMinutes * len(tiers),
		LogWriter:      coordLogWriter,
		ProgressFunc:   func(team, msg string) { r.logger.TeamMsg(team, "%s", msg) },
	})
	if err != nil {
		_ = coordLogWriter.Close()
		r.logger.Warn("Coordinator spawn failed (continuing without): %s", err)
		return nil, nil, nil
	}

	r.logger.Success("Coordinator agent spawned")
	return coordHandle, func() { _ = coordLogWriter.Close() }, nil
}

func (r *orchestrationRun) runTiers(ctx context.Context, tiers [][]string) error {
	for tierIdx, tierNames := range tiers {
		if err := r.runTier(ctx, tierIdx, tierNames); err != nil {
			return err
		}
	}
	return nil
}

func (r *orchestrationRun) runTier(ctx context.Context, tierIdx int, tierNames []string) error {
	r.logger.TierStart(tierIdx, tierNames)

	state, err := r.ws.ReadStateContext(ctx)
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}

	results := make(chan tierResult, len(tierNames))
	var wg sync.WaitGroup
	for _, name := range tierNames {
		wg.Add(1)
		go r.spawnTeam(ctx, name, tierNames, state, results, &wg)
	}
	wg.Wait()
	close(results)

	failed, err := r.collectTierResults(ctx, results)
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("tier %d: teams failed: %v", tierIdx, failed)
	}
	return nil
}

func (r *orchestrationRun) spawnTeam(ctx context.Context, teamName string, tierNames []string, state *workspace.State, results chan<- tierResult, wg *sync.WaitGroup) {
	defer wg.Done()

	res, err := r.runTeam(ctx, teamName, tierNames, state)
	results <- tierResult{name: teamName, res: res, err: err}
}

func (r *orchestrationRun) runTeam(ctx context.Context, teamName string, tierNames []string, state *workspace.State) (*workspace.TeamResult, error) {
	team := r.cfg.TeamByName(teamName)
	if team == nil {
		return nil, fmt.Errorf("team %q not found in config", teamName)
	}
	if err := r.markTeamRunning(teamName); err != nil {
		return nil, err
	}

	r.logger.TeamMsg(teamName, "Starting %s", team.Lead.Role)
	if err := r.seedBootstrapMessages(team, state); err != nil {
		return nil, err
	}

	logWriter, err := r.ws.LogWriter(teamName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = logWriter.Close() }()

	return spawner.Spawn(ctx, &spawner.SpawnOpts{
		TeamName:       teamName,
		Prompt:         r.teamPrompt(team, tierNames, state),
		Model:          r.teamModel(team),
		MaxTurns:       r.cfg.Defaults.MaxTurns,
		PermissionMode: r.cfg.Defaults.PermissionMode,
		TimeoutMinutes: r.cfg.Defaults.TimeoutMinutes,
		LogWriter:      logWriter,
		ProgressFunc:   func(team, msg string) { r.logger.TeamMsg(team, "%s", msg) },
	})
}

func (r *orchestrationRun) markTeamRunning(teamName string) error {
	return r.ws.UpdateRegistryEntry(teamName, func(e *workspace.RegistryEntry) {
		e.Status = "running"
		e.StartedAt = time.Now()
	})
}

func (r *orchestrationRun) seedBootstrapMessages(team *config.Team, state *workspace.State) error {
	for _, dep := range team.DependsOn {
		summary := r.dependencySummary(dep, state)
		if summary == "" {
			continue
		}
		if err := r.bus.Send(r.bootstrapMessage(dep, team.Name, summary)); err != nil {
			return err
		}
	}
	return nil
}

func (r *orchestrationRun) dependencySummary(dep string, state *workspace.State) string {
	depResult, err := r.ws.ReadResult(dep)
	if err != nil || depResult == nil {
		return ""
	}
	if depResult.Result != "" {
		return depResult.Result
	}
	if ts, ok := state.Teams[dep]; ok {
		return ts.ResultSummary
	}
	return ""
}

func (r *orchestrationRun) bootstrapMessage(dep, teamName, summary string) *messaging.Message {
	return &messaging.Message{
		ID:        "bootstrap-" + dep + "-to-" + teamName,
		Sender:    "orchestrator",
		Recipient: r.inboxLookup[teamName],
		Type:      messaging.MsgBootstrap,
		Subject:   "Results from " + dep + " (completed)",
		Content:   summary,
		Timestamp: time.Now(),
		Read:      false,
	}
}

func (r *orchestrationRun) teamPrompt(team *config.Team, tierNames []string, state *workspace.State) string {
	return injection.BuildPrompt(team, r.cfg.Name, state, r.cfg, tierPeers(tierNames), r.inboxLookup[team.Name], r.bus.Path())
}

func tierPeers(tierNames []string) []string {
	if len(tierNames) <= 1 {
		return nil
	}
	return tierNames
}

func (r *orchestrationRun) teamModel(team *config.Team) string {
	if team.Lead.Model != "" {
		return team.Lead.Model
	}
	return r.cfg.Defaults.Model
}

func (r *orchestrationRun) collectTierResults(ctx context.Context, results <-chan tierResult) ([]string, error) {
	var failed []string
	for result := range results {
		if result.err != nil {
			failed = append(failed, result.name)
			if err := r.markTeamFailed(ctx, result.name, result.err); err != nil {
				return nil, err
			}
			continue
		}
		if err := r.recordTeamResult(ctx, result.name, result.res); err != nil {
			return nil, err
		}
	}
	return failed, nil
}

func (r *orchestrationRun) markTeamFailed(ctx context.Context, teamName string, teamErr error) error {
	r.logger.TeamMsg(teamName, "FAILED: %s", teamErr)
	if err := r.ws.UpdateTeamStateContext(ctx, teamName, func(ts *workspace.TeamState) {
		*ts = workspace.TeamState{Status: "failed"}
	}); err != nil {
		return fmt.Errorf("updating failed team state for %s: %w", teamName, err)
	}
	if err := r.ws.UpdateRegistryEntry(teamName, func(e *workspace.RegistryEntry) {
		e.Status = "failed"
		e.EndedAt = time.Now()
	}); err != nil {
		return fmt.Errorf("updating failed registry entry for %s: %w", teamName, err)
	}
	return nil
}

func (r *orchestrationRun) recordTeamResult(ctx context.Context, teamName string, result *workspace.TeamResult) error {
	r.logger.TeamMsg(teamName, "Done (turns: %d, %s in / %s out)", result.NumTurns, fmtTokens(result.InputTokens), fmtTokens(result.OutputTokens))
	if err := r.ws.WriteResult(result); err != nil {
		return fmt.Errorf("writing result for %s: %w", teamName, err)
	}
	if err := r.updateCompletedTeamState(ctx, teamName, result); err != nil {
		return err
	}
	return r.updateCompletedRegistryEntry(teamName, result.SessionID)
}

func (r *orchestrationRun) updateCompletedTeamState(ctx context.Context, teamName string, result *workspace.TeamResult) error {
	err := r.ws.UpdateTeamStateContext(ctx, teamName, func(ts *workspace.TeamState) {
		*ts = workspace.TeamState{
			Status:        "done",
			ResultSummary: result.Result,
			CostUSD:       result.CostUSD,
			DurationMs:    result.DurationMs,
			InputTokens:   result.InputTokens,
			OutputTokens:  result.OutputTokens,
		}
	})
	if err != nil {
		return fmt.Errorf("updating team state for %s: %w", teamName, err)
	}
	return nil
}

func (r *orchestrationRun) updateCompletedRegistryEntry(teamName, sessionID string) error {
	err := r.ws.UpdateRegistryEntry(teamName, func(e *workspace.RegistryEntry) {
		e.Status = "done"
		e.SessionID = sessionID
		e.EndedAt = time.Now()
	})
	if err != nil {
		return fmt.Errorf("updating registry entry for %s: %w", teamName, err)
	}
	return nil
}

func (r *orchestrationRun) stopCoordinator(coordHandle *spawner.CoordinatorHandle) error {
	if coordHandle == nil {
		return nil
	}

	r.logger.Info("Signaling coordinator to stop...")
	coordHandle.Cancel()
	coordResult, coordErr := coordHandle.Wait()
	if coordErr != nil {
		r.logger.Warn("Coordinator exited with error: %s", coordErr)
		return nil
	}
	if coordResult == nil {
		return nil
	}
	r.logger.TeamMsg("coordinator", "Done (cost: $%.2f, turns: %d)", coordResult.CostUSD, coordResult.NumTurns)
	if err := r.ws.WriteResult(coordResult); err != nil {
		return fmt.Errorf("writing coordinator result: %w", err)
	}
	return nil
}

// fmtTokens formats a token count as a human-readable string (e.g. "284K", "1.2M").
func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dK", n/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

func printSummary(ctx context.Context, ws *workspace.Workspace, _ *config.Config, wallClock time.Duration) {
	state, err := ws.ReadStateContext(ctx)
	if err != nil {
		return
	}
	reg, err := ws.ReadRegistry()
	if err != nil {
		return
	}

	bold := color.New(color.Bold)
	fmt.Println()
	_, _ = bold.Println("═══════════════════════════════════════════════════")
	_, _ = bold.Printf("  Orchestra: %s — Complete\n", state.Project)
	_, _ = bold.Println("═══════════════════════════════════════════════════")
	fmt.Println()

	fmt.Printf("  %-16s │ %-8s │ %-12s │ %-6s │ %-10s\n", "Team", "Status", "Tokens", "Turns", "Duration")
	fmt.Printf("  ────────────────┼──────────┼──────────────┼────────┼────────────\n")

	var totalIn, totalOut int64
	var totalTurns int
	for _, entry := range reg.Teams {
		ts := state.Teams[entry.Name]
		tokens := ""
		turns := ""
		dur := ""
		if ts.InputTokens > 0 || ts.OutputTokens > 0 {
			tokens = fmt.Sprintf("%s→%s", fmtTokens(ts.InputTokens), fmtTokens(ts.OutputTokens))
			totalIn += ts.InputTokens
			totalOut += ts.OutputTokens
		}
		// Read result for turns
		if res, err := ws.ReadResult(entry.Name); err == nil {
			turns = strconv.Itoa(res.NumTurns)
			totalTurns += res.NumTurns
		}
		if ts.DurationMs > 0 {
			d := time.Duration(ts.DurationMs) * time.Millisecond
			dur = fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
		}
		fmt.Printf("  %-16s │ %-8s │ %-12s │ %-6s │ %s\n", entry.Name, ts.Status, tokens, turns, dur)
	}

	fmt.Printf("  ────────────────┼──────────┼──────────────┼────────┼────────────\n")
	fmt.Printf("  %-16s │          │ %s→%-7s │ %-6d │\n", "Total", fmtTokens(totalIn), fmtTokens(totalOut), totalTurns)
	fmt.Println()

	wc := wallClock.Round(time.Second)
	fmt.Printf("  Wall clock: %dm %02ds\n", int(wc.Minutes()), int(wc.Seconds())%60)
	fmt.Printf("  Results:    %s/results/\n", ws.Path)
	fmt.Printf("  Logs:       %s/logs/\n", ws.Path)
	fmt.Println()
}
