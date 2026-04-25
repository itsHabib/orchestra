package orchestra

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
	"github.com/itsHabib/orchestra/internal/injection"
	"github.com/itsHabib/orchestra/internal/messaging"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
	"github.com/itsHabib/orchestra/internal/workspace"
)

// Run executes the workflow described by cfg and returns its result. ctx
// is honored throughout; on cancellation, in-flight teams are canceled
// and all spawned subprocesses (team agents, coordinator) are stopped
// before Run returns. The returned *Result reflects whatever state was
// reached, even on error.
//
// Run takes ownership of cfg for the call duration. It may call
// ResolveDefaults / Validate on the pointer; concurrent caller mutation
// is undefined behavior. Callers sharing a Config across goroutines must
// clone — see [CloneConfig].
//
// Concurrent Run invocations from the same process targeting the same
// resolved [WithWorkspaceDir] return [ErrRunInProgress]. Different
// workspaces are independent.
//
// Experimental: signature and Result shape may change.
func Run(ctx context.Context, cfg *Config, opts ...Option) (*Result, error) {
	if cfg == nil {
		return nil, errors.New("orchestra: nil config")
	}

	options := defaultRunOptions()
	for _, opt := range opts {
		opt(&options)
	}

	absWorkspace, err := absWorkspaceDir(options.workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("orchestra: resolve workspace dir: %w", err)
	}
	release, err := acquireWorkspace(absWorkspace)
	if err != nil {
		return nil, err
	}
	defer release()

	cfg.ResolveDefaults()
	if _, err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("orchestra: validate config: %w", err)
	}

	return runWithLockedWorkspace(ctx, cfg, &options, absWorkspace)
}

// LoadConfig parses a YAML config from path, applies defaults, and runs
// validation. Warnings are returned even when err is non-nil so that
// callers can surface validation context.
//
// Experimental.
//
//nolint:gocritic // (cfg, warnings, err) tuple mirrors internal config.Load and is part of the documented SDK signature.
func LoadConfig(path string) (*Config, []Warning, error) {
	return config.Load(path)
}

// Validate runs the config validator standalone. Useful for callers that
// build configs programmatically. Mirrors what Run does internally:
// applies ResolveDefaults to cfg, then validates.
//
// Experimental.
func Validate(cfg *Config) ([]Warning, error) {
	if cfg == nil {
		return nil, errors.New("orchestra: nil config")
	}
	cfg.ResolveDefaults()
	return cfg.Validate()
}

// CloneConfig returns a deep copy of cfg. Use this when sharing a Config
// across goroutines that may invoke Run concurrently — Run takes
// ownership of its cfg for the call duration, so callers must clone to
// avoid undefined behavior.
//
// Experimental.
func CloneConfig(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	clone := *cfg
	clone.Teams = cloneTeams(cfg.Teams)
	return &clone
}

func cloneTeams(in []Team) []Team {
	if in == nil {
		return nil
	}
	out := make([]Team, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Members = cloneSlice(in[i].Members)
		out[i].Tasks = cloneTasks(in[i].Tasks)
		out[i].DependsOn = cloneSlice(in[i].DependsOn)
	}
	return out
}

func cloneTasks(in []Task) []Task {
	if in == nil {
		return nil
	}
	out := make([]Task, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Deliverables = cloneSlice(in[i].Deliverables)
	}
	return out
}

func cloneSlice[T any](in []T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}

// orchestrationRun holds the per-call state of a Run invocation. Methods
// on this type are unexported; tests inside the package may construct
// one directly.
type orchestrationRun struct {
	cfg                *Config
	logger             Logger
	runService         *runsvc.Service
	ws                 *workspace.Workspace
	bus                *messaging.Bus
	participants       []messaging.Participant
	inboxLookup        map[string]string
	maSpawner          *spawner.ManagedAgentsSpawner
	startTeamMAForTest startTeamMAFunc
}

type tierResult struct {
	name string
	res  *workspace.TeamResult
	err  error
}

func runWithLockedWorkspace(ctx context.Context, cfg *Config, options *runOptions, workspaceDir string) (*Result, error) {
	wallStart := time.Now()
	logger := options.logger

	runService := newRunService(workspaceDir, logger)
	active, err := runService.Begin(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("begin run: %w", err)
	}
	defer func() { _ = runService.End(active) }()

	run, tiers, err := newOrchestrationRun(cfg, logger, runService, active)
	if err != nil {
		return nil, err
	}

	runErr := run.execute(ctx, tiers)
	result := run.buildResult(ctx, tiers, time.Since(wallStart))
	return result, runErr
}

func (r *orchestrationRun) execute(ctx context.Context, tiers [][]string) error {
	if r.cfg.Backend.Kind == BackendManagedAgents && r.cfg.Coordinator.Enabled {
		r.logger.Warn("coordinator is not supported under backend.kind=managed_agents")
	}

	var coordHandle *spawner.CoordinatorHandle
	var coordLog io.Closer
	if r.cfg.Backend.Kind != BackendManagedAgents {
		var err error
		coordHandle, coordLog, err = r.startCoordinator(ctx, tiers)
		if err != nil {
			return err
		}
	}
	if coordLog != nil {
		defer func() { _ = coordLog.Close() }()
	}
	cleanedUp := false
	defer func() {
		if cleanedUp || coordHandle == nil {
			return
		}
		coordHandle.Cancel()
		<-coordHandle.Done()
	}()

	if err := r.runTiers(ctx, tiers); err != nil {
		return err
	}
	if err := r.stopCoordinator(coordHandle); err != nil {
		return err
	}
	cleanedUp = true
	return nil
}

func (r *orchestrationRun) buildResult(ctx context.Context, tiers [][]string, dur time.Duration) *Result {
	state, err := r.runService.Snapshot(ctx)
	if err != nil || state == nil {
		return nil
	}
	teams := make(map[string]TeamResult, len(state.Teams))
	for name := range state.Teams {
		ts := state.Teams[name]
		teams[name] = TeamResult{TeamState: ts}
	}
	return &Result{
		Project:    state.Project,
		Teams:      teams,
		Tiers:      tiers,
		DurationMs: dur.Milliseconds(),
	}
}

func newRunService(path string, logger Logger) *runsvc.Service {
	return runsvc.New(
		filestore.New(path),
		runsvc.WithWorkspace(workspace.ForPath(path)),
		runsvc.WithWarn(logger.Warn),
	)
}

func newOrchestrationRun(cfg *Config, logger Logger, runService *runsvc.Service, active *runsvc.Active) (*orchestrationRun, [][]string, error) {
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

	if cfg.Backend.Kind == BackendManagedAgents {
		ma, err := initManagedAgentsBackend(cfg, runService, logger)
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

func initManagedAgentsBackend(cfg *Config, runService *runsvc.Service, logger Logger) (*spawner.ManagedAgentsSpawner, error) {
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

func initLocalBackend(cfg *Config, ws *workspace.Workspace, active *runsvc.Active, logger Logger) ([]messaging.Participant, map[string]string, error) {
	if active.Bus == nil {
		return nil, nil, errors.New("run began without message bus")
	}
	participants := messaging.BuildParticipants(teamNamesFromConfig(cfg.Teams))
	lookup := inboxLookupFromParticipants(participants)
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
	if r.cfg.Backend.Kind == BackendManagedAgents {
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

func (r *orchestrationRun) teamPrompt(team *Team, tierNames []string, state *store.RunState) string {
	return injection.BuildPrompt(team, r.cfg.Name, state, r.cfg, tierPeers(tierNames), r.inboxLookup[team.Name], r.bus.Path())
}

func tierPeers(tierNames []string) []string {
	if len(tierNames) <= 1 {
		return nil
	}
	return tierNames
}

func (r *orchestrationRun) teamModel(team *Team) string {
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

// --- workspace registry for ErrRunInProgress -------------------------------

var (
	workspaceMu      sync.Mutex
	activeWorkspaces = map[string]struct{}{}
)

func absWorkspaceDir(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(cwd, path)), nil
}

func acquireWorkspace(absPath string) (func(), error) {
	workspaceMu.Lock()
	defer workspaceMu.Unlock()
	if _, busy := activeWorkspaces[absPath]; busy {
		return nil, ErrRunInProgress
	}
	activeWorkspaces[absPath] = struct{}{}
	return func() {
		workspaceMu.Lock()
		delete(activeWorkspaces, absPath)
		workspaceMu.Unlock()
	}, nil
}
