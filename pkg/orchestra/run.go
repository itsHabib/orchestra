package orchestra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/customtools"
	"github.com/itsHabib/orchestra/internal/dag"
	"github.com/itsHabib/orchestra/internal/event"
	"github.com/itsHabib/orchestra/internal/ghhost"
	"github.com/itsHabib/orchestra/internal/injection"
	"github.com/itsHabib/orchestra/internal/messaging"
	"github.com/itsHabib/orchestra/internal/notify"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
	"github.com/itsHabib/orchestra/internal/workspace"
)

// LoadConfig parses a YAML config from path, applies defaults, and runs
// validation. Returns a [ValidationResult] aggregating the parsed
// config, any warnings, and any errors. The error return is reserved
// for I/O or parse failures (file not found, malformed YAML);
// structural validation issues live in result.Errors.
//
// Typical use:
//
//	res, err := orchestra.LoadConfig("orchestra.yaml")
//	if err != nil {
//	    return err // I/O or parse failure
//	}
//	for _, w := range res.Warnings {
//	    fmt.Fprintln(os.Stderr, w)
//	}
//	if !res.Valid() {
//	    return res.Err()
//	}
//	_, err = orchestra.Run(ctx, res.Config)
//
// Experimental.
func LoadConfig(path string) (*ValidationResult, error) {
	return config.Load(path)
}

// Validate runs the config validator standalone. Useful for callers
// that build configs programmatically. Mirrors what Run does
// internally: applies ResolveDefaults to cfg, then validates. A nil
// cfg is treated as a hard validation failure (one ConfigError entry,
// empty Field) rather than a panic — Validate never returns nil.
//
// Experimental.
func Validate(cfg *Config) *ValidationResult {
	if cfg == nil {
		return &ValidationResult{
			Errors: []ConfigError{{Message: "nil config"}},
		}
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
	clone.Backend = cloneBackend(cfg.Backend)
	clone.Agents = cloneAgents(cfg.Agents)
	return &clone
}

// cloneBackend deep-copies Backend's pointer sub-objects so concurrent
// CloneConfig consumers don't share the ManagedAgents block (which
// ResolveDefaults / repository-flow code mutates in place).
func cloneBackend(b Backend) Backend {
	out := b
	if b.ManagedAgents != nil {
		ma := *b.ManagedAgents
		ma.Repository = cloneRepositorySpec(b.ManagedAgents.Repository)
		out.ManagedAgents = &ma
	}
	return out
}

func cloneRepositorySpec(r *config.RepositorySpec) *config.RepositorySpec {
	if r == nil {
		return nil
	}
	repo := *r
	return &repo
}

func cloneEnvironmentOverride(e config.EnvironmentOverride) config.EnvironmentOverride {
	out := e
	out.Repository = cloneRepositorySpec(e.Repository)
	return out
}

func cloneAgents(in []Agent) []Agent {
	if in == nil {
		return nil
	}
	out := make([]Agent, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Members = cloneSlice(in[i].Members)
		out[i].Tasks = cloneTasks(in[i].Tasks)
		out[i].DependsOn = cloneSlice(in[i].DependsOn)
		// SkillRef and CustomToolRef are pure value types today (only
		// string fields), so cloneSlice's shallow copy is enough — but
		// the explicit clones decouple us from a future field addition
		// that introduces pointer or slice fields and would otherwise
		// silently alias.
		out[i].Skills = cloneSlice(in[i].Skills)
		out[i].CustomTools = cloneSlice(in[i].CustomTools)
		out[i].EnvironmentOverride = cloneEnvironmentOverride(in[i].EnvironmentOverride)
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
	emitter            event.Emitter
	runService         *runsvc.Service
	ws                 *workspace.Workspace
	bus                *messaging.Bus
	participants       []messaging.Participant
	inboxLookup        map[string]string
	maSpawner          *spawner.ManagedAgentsSpawner
	ghClient           *ghhost.Client  // nil for non-MA runs or when no repository is configured
	ghPAT              string          // in-memory only; never persisted or logged
	handle             *Handle         // nil when called by tests that don't construct a Handle
	notifier           notify.Notifier // notifies on signal_completion; nil disables
	startTeamMAForTest startTeamMAFunc

	// teamSkills and teamCustomTools are pre-resolved at run-construction
	// time so the agent-creation hot path is a map read, not a cache lookup.
	// Keyed by team name; nil entries mean "team has no skills/tools" rather
	// than "not yet resolved". MA backend only.
	teamSkills      map[string][]spawner.Skill
	teamCustomTools map[string][]spawner.Tool

	// toolNamesMu guards toolNamesByUseID, the MA-only tool-name lookup
	// populated on AgentToolUseEvent and consumed on AgentToolResultEvent
	// so EventToolResult.Tool carries the tool name (which the spawner's
	// ToolResult struct does not — it only carries the ToolUseID).
	toolNamesMu      sync.Mutex
	toolNamesByUseID map[string]string
}

type tierResult struct {
	name string
	res  *workspace.AgentResult
	err  error
}

// emit delivers ev through the orchestrationRun's emitter, falling back
// to a noop when no emitter is wired (tests that construct orchestrationRun
// directly without going through newOrchestrationRun).
//
//nolint:gocritic // Event-by-value matches the public Emit signature; pointer would force allocations and be inconsistent.
func (r *orchestrationRun) emit(ev Event) {
	if r.emitter == nil {
		return
	}
	r.emitter.Emit(ev)
}

// emitInfo emits an EventInfo with Tier=-1.
func (r *orchestrationRun) emitInfo(format string, args ...any) {
	r.emit(Event{
		Kind:    EventInfo,
		Tier:    -1,
		Message: fmt.Sprintf(format, args...),
		At:      time.Now(),
	})
}

// emitWarn emits an EventWarn with Tier=-1.
func (r *orchestrationRun) emitWarn(format string, args ...any) {
	r.emit(Event{
		Kind:    EventWarn,
		Tier:    -1,
		Message: fmt.Sprintf(format, args...),
		At:      time.Now(),
	})
}

// emitTeamMessage emits an EventTeamMessage scoped to a tier and team.
func (r *orchestrationRun) emitTeamMessage(tier int, team, format string, args ...any) {
	r.emit(Event{
		Kind:    EventTeamMessage,
		Tier:    tier,
		Team:    team,
		Message: fmt.Sprintf(format, args...),
		At:      time.Now(),
	})
}

func runWithLockedWorkspace(ctx context.Context, cfg *Config, _ *runOptions, workspaceDir string, handle *Handle) (*Result, error) {
	wallStart := time.Now()
	emitter := pickEmitter(handle)

	runService := newRunService(workspaceDir, emitter)
	active, err := runService.Begin(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("begin run: %w", err)
	}
	defer func() { _ = runService.End(active) }()

	if handle != nil {
		handle.setRunService(runService)
	}

	run, tiers, err := newOrchestrationRun(ctx, cfg, emitter, runService, active, handle)
	if err != nil {
		return nil, err
	}
	if handle != nil {
		handle.setSteering(cfg.Backend.Kind, run.bus, run.inboxLookup, maSessionEvents(run.maSpawner))
	}

	runErr := run.execute(ctx, tiers)
	result, snapErr := run.buildResult(ctx, tiers, time.Since(wallStart))
	switch {
	case runErr != nil && snapErr != nil:
		return result, errors.Join(runErr, snapErr)
	case runErr != nil:
		return result, runErr
	default:
		return result, snapErr
	}
}

// pickEmitter returns the Handle as the engine's emitter when available;
// otherwise a NoopEmitter so engine code can call Emit unconditionally.
// Today every real engine path threads the Handle through, but tests that
// drive the engine helpers directly without a Handle benefit from the
// fallback.
func pickEmitter(h *Handle) event.Emitter {
	if h == nil {
		return event.NoopEmitter{}
	}
	return h
}

// maSessionEvents extracts the session-events sender from a managed-agents
// spawner so the Handle can deliver Send / Interrupt events directly to the
// MA backend. Returns nil for local-backend runs (spawner == nil) or when
// the spawner was constructed without session-events support.
func maSessionEvents(ma *spawner.ManagedAgentsSpawner) spawner.SessionEventSender {
	if ma == nil {
		return nil
	}
	return ma.SessionEvents()
}

func (r *orchestrationRun) execute(ctx context.Context, tiers [][]string) error {
	if r.cfg.Backend.Kind == BackendManagedAgents && r.cfg.Coordinator.Enabled {
		r.emitWarn("coordinator is not supported under backend.kind=managed_agents")
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

	if r.handle != nil {
		r.handle.setPhase(PhaseRunning)
	}
	if err := r.runTiers(ctx, tiers); err != nil {
		return err
	}
	if r.handle != nil {
		r.handle.setPhase(PhaseCompleting)
	}
	if err := r.stopCoordinator(coordHandle); err != nil {
		return err
	}
	cleanedUp = true
	return nil
}

// buildResult snapshots the run state and packages it into the SDK
// Result. Snapshot uses a context detached from ctx so that a canceled
// caller context still produces a Result reflecting whatever state was
// reached — matching Run's documented contract. A non-nil error from
// here is a real Snapshot failure (e.g., disk I/O), not cancellation.
func (r *orchestrationRun) buildResult(ctx context.Context, tiers [][]string, dur time.Duration) (*Result, error) {
	snapCtx := context.WithoutCancel(ctx)
	state, err := r.runService.Snapshot(snapCtx)
	if err != nil {
		return nil, fmt.Errorf("orchestra: snapshot run state: %w", err)
	}
	if state == nil {
		return nil, errors.New("orchestra: snapshot returned nil state")
	}
	agents := make(map[string]AgentResult, len(state.Agents))
	for name := range state.Agents {
		ts := state.Agents[name]
		agents[name] = AgentResult{AgentState: ts}
	}
	return &Result{
		Project:    state.Project,
		Agents:     agents,
		Tiers:      tiers,
		DurationMs: dur.Milliseconds(),
	}, nil
}

// newRunService constructs a run.Service wired to the SDK emitter. The
// service's WithWarn hook fires for non-fatal mirror failures; routing
// those through EventWarn keeps engine warnings on the same channel as
// every other observation.
func newRunService(path string, emitter event.Emitter) *runsvc.Service {
	warn := func(format string, args ...any) {
		emitter.Emit(Event{
			Kind:    EventWarn,
			Tier:    -1,
			Message: fmt.Sprintf(format, args...),
			At:      time.Now(),
		})
	}
	return runsvc.New(
		filestore.New(path),
		runsvc.WithWorkspace(workspace.ForPath(path)),
		runsvc.WithWarn(warn),
	)
}

func newOrchestrationRun(ctx context.Context, cfg *Config, emitter event.Emitter, runService *runsvc.Service, active *runsvc.Active, handle *Handle) (*orchestrationRun, [][]string, error) {
	ws := runService.Workspace()
	if ws == nil {
		return nil, nil, errors.New("run service has no workspace attached")
	}
	emitter.Emit(Event{
		Kind:    EventInfo,
		Tier:    -1,
		Message: "Workspace initialized at " + ws.Path,
		At:      time.Now(),
	})

	tiers, err := dag.BuildTiers(cfg.Agents)
	if err != nil {
		return nil, nil, fmt.Errorf("building DAG: %w", err)
	}
	emitter.Emit(Event{
		Kind:    EventInfo,
		Tier:    -1,
		Message: fmt.Sprintf("DAG: %d tiers", len(tiers)),
		At:      time.Now(),
	})

	if cfg.Backend.Kind == BackendManagedAgents {
		ma, err := initManagedAgentsBackend(cfg, runService, emitter)
		if err != nil {
			return nil, nil, err
		}
		ghPAT, ghClient, err := initGitHubClient(cfg, emitter)
		if err != nil {
			return nil, nil, err
		}
		registerBuiltinCustomTools()
		resources, err := resolveTeamResources(ctx, cfg, emitter)
		if err != nil {
			return nil, nil, err
		}
		return &orchestrationRun{
			cfg:             cfg,
			emitter:         emitter,
			runService:      runService,
			ws:              ws,
			bus:             active.Bus,
			maSpawner:       ma,
			ghClient:        ghClient,
			ghPAT:           ghPAT,
			handle:          handle,
			notifier:        defaultNotifier(ws),
			teamSkills:      resources.Skills,
			teamCustomTools: resources.CustomTools,
		}, tiers, nil
	}

	participants, lookup, err := initLocalBackend(cfg, ws, active, emitter)
	if err != nil {
		return nil, nil, err
	}
	return &orchestrationRun{
		cfg:          cfg,
		emitter:      emitter,
		runService:   runService,
		ws:           ws,
		bus:          active.Bus,
		participants: participants,
		inboxLookup:  lookup,
		handle:       handle,
	}, tiers, nil
}

// registerBuiltinCustomTools registers signal_completion (and any other
// host-side custom tools) into the package-level registry. customtools.Register
// is idempotent on a same-name re-register so calling this on every Run is
// safe; tests that need to swap a fake call customtools.Reset first.
func registerBuiltinCustomTools() {
	// MustRegister panics on a malformed handler — built-ins are static so a
	// failure here is a programming error worth crashing the process for.
	customtools.MustRegister(customtools.NewSignalCompletion())
}

// defaultNotifier composes the v0 notification fan-out: an append-only NDJSON
// log under the workspace, a TTY bell + line on stderr, and a best-effort
// system notifier (osascript / notify-send / no-op). Failures in any single
// sink are logged and ignored — the design (§9.1) treats notification as
// best-effort.
//
// Stderr (not stdout) is the terminal target because the engine emits result
// JSON and event lines on stdout; mixing the bell into stdout would clutter
// machine-readable output. The Compose logger also writes to stderr at warn
// level so a flaky sink (missing notify-send, timed-out osascript) leaves a
// breadcrumb the operator can grep — an io.Discard logger here would silently
// swallow exactly the failures the design wants surfaced.
func defaultNotifier(ws *workspace.Workspace) notify.Notifier {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	sinks := make([]notify.Notifier, 0, 3)
	// Skip the NDJSON sink when there's no workspace — newOrchestrationRun
	// always wires one in production, but tests construct orchestrationRun
	// values directly without one. NewLog("") would otherwise fail every
	// notification (open: no such file) and the fan-out would silently
	// swallow it.
	if ws != nil {
		sinks = append(sinks, notify.NewLog(filepath.Join(ws.Path, "notifications.ndjson")))
	}
	sinks = append(sinks, notify.NewTerminal(os.Stderr), notify.NewSystem())
	return notify.Compose(logger, sinks...)
}

func initManagedAgentsBackend(cfg *Config, runService *runsvc.Service, emitter event.Emitter) (*spawner.ManagedAgentsSpawner, error) {
	emitter.Emit(Event{
		Kind:    EventInfo,
		Tier:    -1,
		Message: "Managed-agents backend: file message bus and coordinator workspace are disabled",
		At:      time.Now(),
	})
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
// any team has an effective repository configured. Returns ("", nil, nil) for
// runs that do not need GitHub access (text-only managed-agents flows).
// Resolved at startup so missing-token errors fail fast.
func initGitHubClient(cfg *Config, emitter event.Emitter) (string, *ghhost.Client, error) {
	if !cfgNeedsGitHub(cfg) {
		return "", nil, nil
	}
	pat, err := ghhost.ResolvePAT()
	if err != nil {
		return "", nil, fmt.Errorf("github pat: %w", err)
	}
	emitter.Emit(Event{
		Kind:    EventInfo,
		Tier:    -1,
		Message: "GitHub client initialized for managed-agents repository flow",
		At:      time.Now(),
	})
	return pat, ghhost.New(pat), nil
}

func cfgNeedsGitHub(cfg *Config) bool {
	if cfg.Backend.Kind != BackendManagedAgents {
		return false
	}
	if cfg.Backend.ManagedAgents != nil && cfg.Backend.ManagedAgents.Repository != nil {
		return true
	}
	for i := range cfg.Agents {
		if cfg.Agents[i].EnvironmentOverride.Repository != nil {
			return true
		}
	}
	return false
}

func initLocalBackend(cfg *Config, ws *workspace.Workspace, active *runsvc.Active, emitter event.Emitter) ([]messaging.Participant, map[string]string, error) {
	if active.Bus == nil {
		return nil, nil, errors.New("run began without message bus")
	}
	participants := messaging.BuildParticipants(teamNamesFromConfig(cfg.Agents))
	lookup := inboxLookupFromParticipants(participants)
	emitter.Emit(Event{
		Kind:    EventInfo,
		Tier:    -1,
		Message: "Message bus initialized at " + active.Bus.Path(),
		At:      time.Now(),
	})

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
	if r.handle != nil {
		r.handle.setCurrentTier(tierIdx)
	}
	r.emit(Event{
		Kind:    EventTierStart,
		Tier:    tierIdx,
		Message: strings.Join(tierNames, ", "),
		At:      time.Now(),
	})

	state, err := r.runService.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}

	results := make(chan tierResult, len(tierNames))
	var wg sync.WaitGroup
	for _, name := range tierNames {
		wg.Add(1)
		go r.spawnTeam(ctx, tierIdx, name, tierNames, state, results, &wg)
	}
	wg.Wait()
	close(results)

	failed, err := r.collectTierResults(ctx, tierIdx, results)
	r.emit(Event{
		Kind: EventTierComplete,
		Tier: tierIdx,
		At:   time.Now(),
	})
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("tier %d: teams failed: %v", tierIdx, failed)
	}
	return nil
}

func (r *orchestrationRun) spawnTeam(ctx context.Context, tierIdx int, teamName string, tierNames []string, state *store.RunState, results chan<- tierResult, wg *sync.WaitGroup) {
	defer wg.Done()

	res, err := r.runTeam(ctx, tierIdx, teamName, tierNames, state)
	results <- tierResult{name: teamName, res: res, err: err}
}

func (r *orchestrationRun) runTeam(ctx context.Context, tierIdx int, teamName string, tierNames []string, state *store.RunState) (*workspace.AgentResult, error) {
	team := r.cfg.AgentByName(teamName)
	if team == nil {
		return nil, fmt.Errorf("team %q not found in config", teamName)
	}
	if err := r.runService.RecordTeamStart(ctx, teamName); err != nil {
		return nil, err
	}

	r.emit(Event{
		Kind:    EventTeamStart,
		Tier:    tierIdx,
		Team:    teamName,
		Message: team.Lead.Role,
		At:      time.Now(),
	})
	if r.cfg.Backend.Kind == BackendManagedAgents {
		return r.runTeamMA(ctx, tierIdx, team, state)
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
		ProgressFunc: func(team, msg string) {
			r.emitTeamMessage(tierIdx, team, "%s", msg)
		},
	})
}

func (r *orchestrationRun) teamPrompt(team *Team, tierNames []string, state *store.RunState) string {
	return injection.BuildPrompt(team, r.cfg.Name, state, r.cfg, tierPeers(tierNames), r.inboxLookup[team.Name], r.bus.Path(), injection.Capabilities{})
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

func (r *orchestrationRun) collectTierResults(ctx context.Context, tierIdx int, results <-chan tierResult) ([]string, error) {
	var failed []string
	for result := range results {
		if result.err != nil {
			failed = append(failed, result.name)
			if err := r.markTeamFailed(ctx, tierIdx, result.name, result.err); err != nil {
				return nil, err
			}
			continue
		}
		if err := r.recordTeamResult(ctx, tierIdx, result.name, result.res); err != nil {
			return nil, err
		}
	}
	return failed, nil
}

func (r *orchestrationRun) markTeamFailed(ctx context.Context, tierIdx int, teamName string, teamErr error) error {
	r.emit(Event{
		Kind:    EventTeamFailed,
		Tier:    tierIdx,
		Team:    teamName,
		Message: fmt.Sprintf("FAILED: %s", teamErr),
		At:      time.Now(),
	})
	if err := r.runService.RecordTeamFail(ctx, teamName, teamErr); err != nil {
		return fmt.Errorf("recording failed team %s: %w", teamName, err)
	}
	return nil
}

func (r *orchestrationRun) recordTeamResult(ctx context.Context, tierIdx int, teamName string, result *workspace.AgentResult) error {
	var msg string
	if result.NumTurns > 0 {
		msg = fmt.Sprintf("Done (turns: %d, %s in / %s out)", result.NumTurns, fmtTokens(result.InputTokens), fmtTokens(result.OutputTokens))
	} else {
		msg = fmt.Sprintf("Done (%s in / %s out)", fmtTokens(result.InputTokens), fmtTokens(result.OutputTokens))
	}
	r.emit(Event{
		Kind:    EventTeamComplete,
		Tier:    tierIdx,
		Team:    teamName,
		Message: msg,
		Cost:    result.CostUSD,
		Turns:   result.NumTurns,
		At:      time.Now(),
	})
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
