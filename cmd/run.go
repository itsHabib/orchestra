package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/dag"
	"github.com/itsHabib/orchestra/internal/ghhost"
	"github.com/itsHabib/orchestra/internal/injection"
	olog "github.com/itsHabib/orchestra/internal/log"
	"github.com/itsHabib/orchestra/internal/messaging"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/workspace"
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

	runService := newRunService(workspaceDir, logger)
	active, err := runService.Begin(ctx, cfg)
	if err != nil {
		return fmt.Errorf("begin run: %w", err)
	}
	defer func() { _ = runService.End(active) }()

	run, tiers, err := newOrchestrationRun(cfg, logger, runService, active)
	if err != nil {
		return err
	}

	if cfg.Backend.Kind == "managed_agents" && cfg.Coordinator.Enabled {
		logger.Warn("coordinator is not supported under backend.kind=managed_agents")
	}
	var coordHandle *spawner.CoordinatorHandle
	var coordLog io.Closer
	if cfg.Backend.Kind != "managed_agents" {
		coordHandle, coordLog, err = run.startCoordinator(ctx, tiers)
		if err != nil {
			return err
		}
		if coordLog != nil {
			defer func() { _ = coordLog.Close() }()
		}
	}

	if err := run.runTiers(ctx, tiers); err != nil {
		return err
	}
	if err := run.stopCoordinator(coordHandle); err != nil {
		return err
	}

	printSummary(ctx, logger, run.runService, run.ws, cfg, time.Since(wallStart))
	return nil
}

type orchestrationRun struct {
	cfg                *config.Config
	logger             *olog.Logger
	runService         *runsvc.Service
	ws                 *workspace.Workspace
	bus                *messaging.Bus
	participants       []messaging.Participant
	inboxLookup        map[string]string
	maSpawner          *spawner.ManagedAgentsSpawner
	ghClient           *ghhost.Client // nil for non-MA runs or when no repository is configured
	ghPAT              string         // in-memory only; never persisted or logged
	startTeamMAForTest startTeamMAFunc
}

type tierResult struct {
	name string
	res  *workspace.TeamResult
	err  error
}

func newOrchestrationRun(cfg *config.Config, logger *olog.Logger, runService *runsvc.Service, active *runsvc.Active) (*orchestrationRun, [][]string, error) {
	ws := runService.Workspace()
	if ws == nil {
		return nil, nil, errors.New("run service has no workspace attached")
	}
	logger.Success("Workspace initialized at %s", ws.Path)

	tiers, err := dag.BuildTiers(cfg.Teams)
	if err != nil {
		return nil, nil, fmt.Errorf("building DAG: %w", err)
	}
	logger.Info("DAG: %d tiers", len(tiers))

	if cfg.Backend.Kind == "managed_agents" {
		ma, err := initManagedAgentsBackend(cfg, runService, logger)
		if err != nil {
			return nil, nil, err
		}
		ghPAT, ghClient, err := initGitHubClient(cfg, logger)
		if err != nil {
			return nil, nil, err
		}
		return &orchestrationRun{
			cfg:        cfg,
			logger:     logger,
			runService: runService,
			ws:         ws,
			bus:        active.Bus,
			maSpawner:  ma,
			ghClient:   ghClient,
			ghPAT:      ghPAT,
		}, tiers, nil
	}

	participants, lookup, err := initLocalBackend(cfg, ws, active, logger)
	if err != nil {
		return nil, nil, err
	}
	return &orchestrationRun{
		cfg:          cfg,
		logger:       logger,
		runService:   runService,
		ws:           ws,
		bus:          active.Bus,
		participants: participants,
		inboxLookup:  lookup,
	}, tiers, nil
}

func initManagedAgentsBackend(cfg *config.Config, runService *runsvc.Service, logger *olog.Logger) (*spawner.ManagedAgentsSpawner, error) {
	logger.Info("Managed-agents backend: file message bus and coordinator workspace are disabled")
	ma, err := spawner.NewHostManagedAgentsSpawner(
		runService.Store(),
		spawner.WithManagedAgentsConcurrency(cfg.Defaults.MAConcurrentSessions),
	)
	if err != nil {
		return nil, fmt.Errorf("managed-agents spawner: %w", err)
	}
	return ma, nil
}

// initGitHubClient resolves the GitHub PAT and returns a ghhost.Client when
// any team has an effective repository configured. Returns (nil, nil, nil) for
// runs that do not need GitHub access (text-only managed-agents flows).
// Resolved at startup so missing-token errors fail fast (design §10 Q6 Option A).
func initGitHubClient(cfg *config.Config, logger *olog.Logger) (string, *ghhost.Client, error) {
	if !cfgNeedsGitHub(cfg) {
		return "", nil, nil
	}
	pat, err := ghhost.ResolvePAT()
	if err != nil {
		return "", nil, fmt.Errorf("github pat: %w", err)
	}
	logger.Info("GitHub client initialized for managed-agents repository flow")
	return pat, ghhost.New(pat), nil
}

func cfgNeedsGitHub(cfg *config.Config) bool {
	if cfg.Backend.Kind != "managed_agents" {
		return false
	}
	if cfg.Backend.ManagedAgents != nil && cfg.Backend.ManagedAgents.Repository != nil {
		return true
	}
	for i := range cfg.Teams {
		if cfg.Teams[i].EnvironmentOverride.Repository != nil {
			return true
		}
	}
	return false
}

func initLocalBackend(cfg *config.Config, ws *workspace.Workspace, active *runsvc.Active, logger *olog.Logger) ([]messaging.Participant, map[string]string, error) {
	if active.Bus == nil {
		return nil, nil, errors.New("run began without message bus")
	}
	participants := messaging.BuildParticipants(teamNames(cfg.Teams))
	lookup := inboxLookup(participants)
	logger.Success("Message bus initialized at %s", active.Bus.Path())

	if err := os.MkdirAll(filepath.Join(ws.Path, "coordinator"), 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating coordinator decisions directory: %w", err)
	}
	return participants, lookup, nil
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

	state, err := r.runService.Snapshot(ctx)
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

func (r *orchestrationRun) spawnTeam(ctx context.Context, teamName string, tierNames []string, state *store.RunState, results chan<- tierResult, wg *sync.WaitGroup) {
	defer wg.Done()

	res, err := r.runTeam(ctx, teamName, tierNames, state)
	results <- tierResult{name: teamName, res: res, err: err}
}

func (r *orchestrationRun) runTeam(ctx context.Context, teamName string, tierNames []string, state *store.RunState) (*workspace.TeamResult, error) {
	team := r.cfg.TeamByName(teamName)
	if team == nil {
		return nil, fmt.Errorf("team %q not found in config", teamName)
	}
	if err := r.runService.RecordTeamStart(ctx, teamName); err != nil {
		return nil, err
	}

	r.logger.TeamMsg(teamName, "Starting %s", team.Lead.Role)
	if r.cfg.Backend.Kind == "managed_agents" {
		return r.runTeamMA(ctx, team, state)
	}

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

func (r *orchestrationRun) teamPrompt(team *config.Team, tierNames []string, state *store.RunState) string {
	return injection.BuildPrompt(team, r.cfg.Name, state, r.cfg, tierPeers(tierNames), r.inboxLookup[team.Name], r.bus.Path(), injection.Capabilities{})
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
	if err := r.runService.RecordTeamFail(ctx, teamName, teamErr); err != nil {
		return fmt.Errorf("recording failed team %s: %w", teamName, err)
	}
	return nil
}

func (r *orchestrationRun) recordTeamResult(ctx context.Context, teamName string, result *workspace.TeamResult) error {
	if result.NumTurns > 0 {
		r.logger.TeamMsg(teamName, "Done (turns: %d, %s in / %s out)", result.NumTurns, fmtTokens(result.InputTokens), fmtTokens(result.OutputTokens))
	} else {
		r.logger.TeamMsg(teamName, "Done (%s in / %s out)", fmtTokens(result.InputTokens), fmtTokens(result.OutputTokens))
	}
	if err := r.ws.WriteResult(result); err != nil {
		return fmt.Errorf("writing result for %s: %w", teamName, err)
	}
	return r.runService.RecordTeamComplete(ctx, result)
}
