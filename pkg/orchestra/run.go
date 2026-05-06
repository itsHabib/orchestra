package orchestra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/credentials"
	"github.com/itsHabib/orchestra/internal/customtools"
	"github.com/itsHabib/orchestra/internal/dag"
	"github.com/itsHabib/orchestra/internal/event"
	"github.com/itsHabib/orchestra/internal/files"
	"github.com/itsHabib/orchestra/internal/ghhost"
	"github.com/itsHabib/orchestra/internal/injection"
	"github.com/itsHabib/orchestra/internal/machost"
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
		out[i].RequiresCredentials = cloneSlice(in[i].RequiresCredentials)
		out[i].Files = cloneSlice(in[i].Files)
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

	// agentEnv is the per-agent environment overlay resolved from the
	// credentials store + host env at run start. Keyed by agent name; the
	// inner map is canonical env-var → secret. Empty when the agent has no
	// `requires_credentials:` declared. Local backend forwards via cmd.Env;
	// MA backend logs an "MA env injection deferred" warning at session
	// start (the SDK does not expose per-session env yet — see PR description).
	agentEnv map[string]map[string]string

	// fileService uploads agent-declared files to the Anthropic Files API
	// and resolves them into [spawner.ResourceRef]{Type:"file"} mounts on
	// the MA session. Nil for non-MA runs (local backend ignores Files
	// declarations with a warning emitted at session start).
	fileService *files.Service
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
		handle.setSteering(cfg.Backend.Kind, maSessionEvents(run.maSpawner))
	}

	runErr := run.execute(ctx, tiers)
	// Cancellation hook: when ctx is canceled (cancel_run sent SIGTERM,
	// caller pressed Ctrl-C, or anything else closed the run context),
	// merge any cancel-request file the MCP server dropped into the
	// run state, then flip every still-running agent to "canceled".
	// Without this hook state.json keeps the agents at "running"
	// indefinitely because the per-agent goroutines bail out without
	// writing terminal state. Use a fresh context detached from the
	// canceled parent so the writes actually land.
	if ctx.Err() != nil {
		cancelCtx := context.WithoutCancel(ctx)
		reason := mergeCancellationRequest(cancelCtx, runService, runService.Workspace())
		if cancelErr := runService.CancelAllRunningAgents(cancelCtx, reason); cancelErr != nil {
			emitter.Emit(Event{
				Kind:    EventWarn,
				Tier:    -1,
				Message: fmt.Sprintf("cancel: failed to flip running agents to canceled: %v", cancelErr),
				At:      time.Now(),
			})
		}
	}
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

// mergeCancellationRequest reads the cancel-request file written by the
// MCP server (cancellation.json under the workspace), merges it into
// the run state via [runsvc.Service.RecordCancellationRequested] (which
// holds the in-process state mutex), and returns the operator-supplied
// reason for use as the per-agent cancel cause. Returns an empty
// reason on any failure — the engine still treats the ctx-cancel as a
// deliberate shutdown rather than failing because cancellation.json
// was absent or malformed.
func mergeCancellationRequest(ctx context.Context, svc *runsvc.Service, ws *workspace.Workspace) string {
	if ws == nil {
		return ""
	}
	path := filepath.Join(ws.Path, mcpCancellationFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cf struct {
		RequestedAt time.Time `json:"requested_at"`
		Reason      string    `json:"reason,omitempty"`
	}
	if err := json.Unmarshal(data, &cf); err != nil {
		return ""
	}
	// Merge into RunState.Cancellation under the engine's in-process
	// state mutex. RecordCancellationRequested is a no-op-on-error
	// path; observability gaps here are acceptable since we already
	// have the reason in `cf` and the agent transitions still land.
	if err := svc.RecordCancellationRequestedAt(ctx, cf.Reason, cf.RequestedAt); err != nil {
		_ = err
	}
	return cf.Reason
}

// mcpCancellationFile is the file name (not path) of the MCP-written
// cancel-request file. ws.Path already points at the workspace dir
// (typically ".orchestra/"), so the join is just <ws.Path>/cancellation.json.
// Mirrors [internal/mcp.cancellationFileName] but kept as a separate
// constant so pkg/orchestra doesn't import internal/mcp (the dep arrow
// runs the other way).
const mcpCancellationFile = "cancellation.json"

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
	if r.cfg.Coordinator.Enabled {
		r.emitWarn("coordinator: {enabled: true} is deprecated in v3.0 — the file message bus was removed; the chat-side LLM now plays the coordinator role. Skipping coordinator spawn.")
	}

	if r.handle != nil {
		r.handle.setPhase(PhaseRunning)
	}
	if err := r.runTiers(ctx, tiers); err != nil {
		return err
	}
	if r.handle != nil {
		r.handle.setPhase(PhaseCompleting)
	}
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
		Project: state.Project,
		Agents:  agents,
		// Teams aliases Agents (same map instance) so v2 SDK consumers
		// reading `Run(...).Teams` keep compiling through the v3
		// migration window. AgentResult and TeamResult are the same
		// type (alias). Mutating Teams mutates Agents and vice versa
		// — the deprecated mirror is removed in v3.x, so we don't pay
		// to clone.
		Teams:      agents,
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

func newOrchestrationRun(ctx context.Context, cfg *Config, emitter event.Emitter, runService *runsvc.Service, _ *runsvc.Active, handle *Handle) (*orchestrationRun, [][]string, error) {
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

	agentEnv, err := resolveAgentCredentials(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	if cfg.Backend.Kind == BackendManagedAgents {
		return buildMAOrchestrationRun(ctx, cfg, emitter, runService, ws, handle, agentEnv, tiers)
	}

	return &orchestrationRun{
		cfg:        cfg,
		emitter:    emitter,
		runService: runService,
		ws:         ws,
		handle:     handle,
		agentEnv:   agentEnv,
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
// resolveAgentCredentials walks every agent in cfg, computes the union of
// per-agent and defaults-level `requires_credentials`, and resolves the
// resulting names against the credentials store + host env. The returned
// map is keyed by agent name; the inner map is canonical env-var name →
// secret value. Missing names produce [credentials.ErrMissing] with a
// message naming each absent credential — fail fast at run start beats
// failing 10 minutes in when the agent tries to use the secret.
func resolveAgentCredentials(ctx context.Context, cfg *Config) (map[string]map[string]string, error) {
	credStore := credentials.New("")
	out := make(map[string]map[string]string, len(cfg.Agents))
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		names := a.RequiredCredentials(&cfg.Defaults)
		if len(names) == 0 {
			continue
		}
		resolved, err := credStore.Resolve(ctx, names)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", a.Name, err)
		}
		envOverlay := make(map[string]string, len(resolved))
		// Track which credential name owns each env-var key so we can
		// fail fast on collisions: `foo-bar` and `foo_bar` both
		// normalize to `FOO_BAR`, and silently picking one produces
		// nondeterministic injection (last writer wins on map
		// iteration). Reviewer flagged both Codex P2 and Copilot.
		keyOwner := make(map[string]string, len(resolved))
		credNames := make([]string, 0, len(resolved))
		for credName := range resolved {
			credNames = append(credNames, credName)
		}
		sort.Strings(credNames)
		for _, credName := range credNames {
			envName := credentials.EnvNameFor(credName)
			if prior, dup := keyOwner[envName]; dup {
				return nil, fmt.Errorf("agent %q: credentials %q and %q both normalize to env var %q — pick one to avoid silent overwrites",
					a.Name, prior, credName, envName)
			}
			keyOwner[envName] = credName
			envOverlay[envName] = resolved[credName]
		}
		out[a.Name] = envOverlay
	}
	return out, nil
}

// emitMACredentialWarning surfaces a one-shot warning when an MA-backed
// run declares `requires_credentials:` for any agent. The Anthropic
// Managed Agents SDK does not currently expose per-session env-var
// injection (anthropic-sdk-go v1.37.0 BetaSessionNewParams has no Env
// field; the Vault credential auth union only supports mcp_oauth and
// static_bearer, not generic env vars). Orchestra resolves the names so
// dev workflows fail fast on missing credentials, but the secret is not
// propagated into the MA sandbox.
//
// For GitHub specifically, the github_repository ResourceRef path
// (host PAT → BetaManagedAgentsGitHubRepositoryResourceParams.AuthorizationToken)
// works end-to-end and is the recommended substitute. Any other secret
// (Polygon, Vault, AWS, etc.) is currently unreachable on MA.
//
// Tracking: github.com/itsHabib/orchestra/issues/42 — closing this gap
// is gated on the SDK exposing per-session env injection. See
// docs/feedback-phase-a-dogfood.md §B2 for the dogfood finding that
// surfaced this scope.
//
// Sorts the credential names before formatting so the warning text is
// stable across runs — useful for grep-by-message dashboards and the
// unit test that pins the message verbatim.
func emitMACredentialWarning(emitter event.Emitter, agentEnv map[string]map[string]string) {
	if len(agentEnv) == 0 {
		return
	}
	names := make(map[string]struct{})
	for agent := range agentEnv {
		env := agentEnv[agent]
		for name := range env {
			names[name] = struct{}{}
		}
	}
	if len(names) == 0 {
		return
	}
	flat := make([]string, 0, len(names))
	for name := range names {
		flat = append(flat, name)
	}
	sort.Strings(flat)
	emitter.Emit(Event{
		Kind: EventWarn,
		Tier: -1,
		Message: fmt.Sprintf(
			"requires_credentials resolved %v but managed-agents SDK does not yet expose "+
				"per-session env-var injection; secrets will not reach the agent sandbox. "+
				"For GitHub use the github_repository ResourceRef path. "+
				"Tracking: https://github.com/itsHabib/orchestra/issues/42 "+
				"(see docs/feedback-phase-a-dogfood.md §B2).",
			flat,
		),
		At: time.Now(),
	})
}

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

// buildMAOrchestrationRun assembles the MA-backend orchestrationRun. Lifted
// out of newOrchestrationRun to keep nesting flat — every helper is a single
// fail-fast call site, so the body becomes a flat sequence of init steps
// rather than a deeply-nested if block.
func buildMAOrchestrationRun(ctx context.Context, cfg *Config, emitter event.Emitter, runService *runsvc.Service, ws *workspace.Workspace, handle *Handle, agentEnv map[string]map[string]string, tiers [][]string) (*orchestrationRun, [][]string, error) {
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
	emitMACredentialWarning(emitter, agentEnv)
	fileService, err := initFileService(cfg, emitter)
	if err != nil {
		return nil, nil, err
	}
	return &orchestrationRun{
		cfg:             cfg,
		emitter:         emitter,
		runService:      runService,
		ws:              ws,
		maSpawner:       ma,
		ghClient:        ghClient,
		ghPAT:           ghPAT,
		handle:          handle,
		notifier:        defaultNotifier(ws),
		teamSkills:      resources.Skills,
		teamCustomTools: resources.CustomTools,
		agentEnv:        agentEnv,
		fileService:     fileService,
	}, tiers, nil
}

// initFileService returns a [files.Service] wired to the host Anthropic
// client when any agent in cfg declares files for upload. Returns (nil, nil)
// for runs with no Files declarations — avoids a stray API-key requirement
// on text-only flows.
func initFileService(cfg *Config, emitter event.Emitter) (*files.Service, error) {
	if !cfgNeedsFileService(cfg) {
		return nil, nil
	}
	client, err := machost.NewClient()
	if err != nil {
		return nil, fmt.Errorf("file service: %w", err)
	}
	emitter.Emit(Event{
		Kind:    EventInfo,
		Tier:    -1,
		Message: "File service initialized for managed-agents file mounts",
		At:      time.Now(),
	})
	return files.New(&client.Beta.Files), nil
}

// cfgNeedsFileService reports whether any MA-backed agent declares file
// mounts. Local-backend declarations are warned about separately and never
// touch the SDK, so they don't gate file-service construction.
func cfgNeedsFileService(cfg *Config) bool {
	if cfg.Backend.Kind != BackendManagedAgents {
		return false
	}
	for i := range cfg.Agents {
		if len(cfg.Agents[i].Files) > 0 {
			return true
		}
	}
	return false
}

func initManagedAgentsBackend(cfg *Config, runService *runsvc.Service, emitter event.Emitter) (*spawner.ManagedAgentsSpawner, error) {
	emitter.Emit(Event{
		Kind:    EventInfo,
		Tier:    -1,
		Message: "Managed-agents backend initialized",
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
		return nil, fmt.Errorf("agent %q not found in config", teamName)
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
		Env:            r.agentEnv[teamName],
		ProgressFunc: func(team, msg string) {
			r.emitTeamMessage(tierIdx, team, "%s", msg)
		},
		OnToolUse: func(toolName string, at time.Time) {
			// Persist on every tool event. LastEventAt is the per-event
			// liveness signal documented in the v3 design — a tight
			// Edit/Edit/Edit loop must keep advancing it so chat-side
			// observers can tell the agent is still progressing. We
			// considered deduping consecutive same-tool writes to save
			// I/O, but that froze LastEventAt during the most common
			// loop shape and defeated the field's purpose. If real
			// workloads later show state-file write contention, debounce
			// the LastEventAt-only path instead of restoring the dedupe.
			updateCtx := context.WithoutCancel(ctx)
			err := r.runService.Store().UpdateAgentState(updateCtx, teamName, func(ts *store.AgentState) {
				ts.LastTool = toolName
				ts.LastEventAt = at
			})
			if err != nil {
				// Persistence failures here are observability-only — the
				// agent keeps running. Surface the failure so the
				// chat-side coordinator (or human) sees that LastTool
				// stopped advancing instead of silently ignoring the drift.
				r.emitWarn("LastTool persist for agent %q failed: %v", teamName, err)
			}
		},
	})
}

func (r *orchestrationRun) teamPrompt(team *Team, tierNames []string, state *store.RunState) string {
	return injection.BuildPrompt(team, r.cfg.Name, state, r.cfg, tierPeers(tierNames), injection.Capabilities{})
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
