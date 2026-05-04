package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/dag"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/workspace"
)

// AgentResult is the structured result recorded for a completed agent.
type AgentResult = workspace.AgentResult

// TeamResult is the v2 alias for [AgentResult], retained so internal callers
// keep building during the v3 migration window.
//
// Deprecated: use [AgentResult].
type TeamResult = workspace.AgentResult

// Service owns run lifecycle choreography above the Store.
type Service struct {
	store     store.Store
	clock     func() time.Time
	workspace *workspace.Workspace
	warn      func(format string, args ...any)
}

// Active represents a live run holding the exclusive run lock.
type Active struct {
	RunID string
	State *store.RunState

	release func()
	once    sync.Once
}

// Option configures a Service at construction time.
type Option func(*Service)

// WithWorkspace attaches a workspace so team transitions mirror into registry.json.
// Registry mirror failures are non-fatal — state.json is the source of truth.
func WithWorkspace(ws *workspace.Workspace) Option {
	return func(s *Service) { s.workspace = ws }
}

// WithClock overrides the clock used for state timestamps. Useful in tests.
func WithClock(fn func() time.Time) Option {
	return func(s *Service) {
		if fn != nil {
			s.clock = fn
		}
	}
}

// WithWarn sets a logger hook for non-fatal registry mirror failures.
func WithWarn(fn func(format string, args ...any)) Option {
	return func(s *Service) {
		if fn != nil {
			s.warn = fn
		}
	}
}

// New creates a run service backed by the provided Store.
func New(s store.Store, opts ...Option) *Service {
	svc := &Service{
		store: s,
		clock: time.Now,
		warn:  func(string, ...any) {},
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// IsNotFound reports whether err wraps the store not-found sentinel.
func IsNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

// Workspace returns the workspace attached via WithWorkspace, or nil.
func (s *Service) Workspace() *workspace.Workspace { return s.workspace }

// Store returns the underlying state store.
func (s *Service) Store() store.Store { return s.store }

// Now returns the service clock reading. Callers use this to stamp times
// consistently with the rest of the run lifecycle.
func (s *Service) Now() time.Time { return s.clock() }

// Begin acquires the exclusive run lock, archives any prior active run, and seeds fresh state.
func (s *Service) Begin(ctx context.Context, cfg *config.Config) (*Active, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: nil config", store.ErrInvalidArgument)
	}

	release, err := s.store.AcquireRunLock(ctx, store.LockExclusive)
	if err != nil {
		return nil, fmt.Errorf("run.Begin acquire lock: %w", err)
	}

	active := &Active{release: release}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()

	if err := s.archivePrevious(ctx); err != nil {
		return nil, err
	}

	ws, err := s.ensureWorkspace()
	if err != nil {
		return nil, err
	}

	state, err := s.seedState(cfg)
	if err != nil {
		return nil, fmt.Errorf("run.Begin seed state: %w", err)
	}
	if err := s.store.SaveRunState(ctx, state); err != nil {
		return nil, fmt.Errorf("run.Begin save state: %w", err)
	}

	if err := s.seedWorkspaceFiles(ws, cfg); err != nil {
		return nil, err
	}

	active.RunID = state.RunID
	active.State = state
	ok = true
	return active, nil
}

// SharedSnapshot acquires a shared run lock and reads the current state.
// Missing state is returned as a nil snapshot because spawn can run without a prior run.
func (s *Service) SharedSnapshot(ctx context.Context) (*store.RunState, func(), error) {
	release, err := s.store.AcquireRunLock(ctx, store.LockShared)
	if err != nil {
		return nil, nil, fmt.Errorf("run.SharedSnapshot acquire lock: %w", err)
	}

	state, err := s.Snapshot(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, release, nil
	case err != nil:
		release()
		return nil, nil, err
	default:
		return state, release, nil
	}
}

// Snapshot reads the current run state without taking the run lock.
func (s *Service) Snapshot(ctx context.Context) (*store.RunState, error) {
	state, err := s.store.LoadRunState(ctx)
	if err != nil {
		return nil, fmt.Errorf("run.Snapshot: %w", err)
	}
	return state, nil
}

// RecordTeamStart transitions a team to running and stamps its start time.
func (s *Service) RecordTeamStart(ctx context.Context, team string) error {
	now := s.clock().UTC()
	if err := s.store.UpdateAgentState(ctx, team, func(ts *store.AgentState) {
		ts.Status = "running"
		ts.StartedAt = now
		ts.EndedAt = time.Time{}
		ts.LastError = ""
	}); err != nil {
		return fmt.Errorf("run.RecordTeamStart %s: %w", team, err)
	}

	s.mirrorRegistry("RecordTeamStart", team, func(e *workspace.RegistryEntry) {
		e.Status = "running"
		e.StartedAt = now
		e.EndedAt = time.Time{}
	})
	return nil
}

// RecordTeamComplete transitions a team to done and records result counters.
// The team name is taken from result.Agent.
func (s *Service) RecordTeamComplete(ctx context.Context, result *TeamResult) error {
	if result == nil {
		return fmt.Errorf("%w: nil team result", store.ErrInvalidArgument)
	}
	if result.Agent == "" {
		return fmt.Errorf("%w: team result missing team name", store.ErrInvalidArgument)
	}

	now := s.clock().UTC()
	team := result.Agent
	var endedAt time.Time
	if err := s.store.UpdateAgentState(ctx, team, func(ts *store.AgentState) {
		ts.Status = "done"
		if ts.EndedAt.IsZero() {
			ts.EndedAt = now
		}
		endedAt = ts.EndedAt
		ts.SessionID = result.SessionID
		ts.LastError = ""
		ts.ResultSummary = result.Result
		ts.CostUSD = result.CostUSD
		ts.DurationMs = result.DurationMs
		ts.InputTokens = result.InputTokens
		ts.OutputTokens = result.OutputTokens
		ts.NumTurns = result.NumTurns
	}); err != nil {
		return fmt.Errorf("run.RecordTeamComplete %s: %w", team, err)
	}

	s.mirrorRegistry("RecordTeamComplete", team, func(e *workspace.RegistryEntry) {
		e.Status = "done"
		e.SessionID = result.SessionID
		e.EndedAt = endedAt
	})
	return nil
}

// RecordAgentCancel transitions an agent to "canceled" and stamps an
// explanatory LastError so observers can distinguish a deliberate cancel
// from a crash. Used by the engine's signal handler when a cancel_run
// request lands on a still-running agent.
//
// Race-safe transition: the UpdateAgentState callback skips the rewrite
// when the agent has already reached a terminal state. Without this
// guard, an agent that completed successfully right as cancellation
// was processed would see its done/failed status clobbered to
// "canceled" — Copilot caught this in round-2 review.
func (s *Service) RecordAgentCancel(ctx context.Context, agent, reason string) error {
	now := s.clock().UTC()
	cause := "canceled by cancel_run"
	if reason != "" {
		cause = "canceled by cancel_run: " + reason
	}
	canceled := false
	if err := s.store.UpdateAgentState(ctx, agent, func(ts *store.AgentState) {
		switch ts.Status {
		case "done", "failed", "canceled", "terminated":
			// Already terminal — leave history alone.
			return
		}
		ts.Status = "canceled"
		ts.EndedAt = now
		ts.LastError = cause
		canceled = true
	}); err != nil {
		return fmt.Errorf("run.RecordAgentCancel %s: %w", agent, err)
	}
	if !canceled {
		return nil
	}
	s.mirrorRegistry("RecordAgentCancel", agent, func(e *workspace.RegistryEntry) {
		e.Status = "canceled"
		e.EndedAt = now
	})
	return nil
}

// RecordCancellationRequested writes a [store.Cancellation] entry into
// the run document, stamping it with the service clock. Called by the
// engine when it observes ctx-cancel without an external cancel
// request file (e.g. caller pressed Ctrl-C).
func (s *Service) RecordCancellationRequested(ctx context.Context, reason string) error {
	return s.RecordCancellationRequestedAt(ctx, reason, s.clock().UTC())
}

// RecordCancellationRequestedAt is the timestamp-explicit variant used
// by the engine's signal handler when it merges a cancel request that
// originated outside the engine (the MCP server's cancellation.json
// file carries its own RequestedAt). Preserving the original timestamp
// keeps observability honest — observers see *when* the user asked,
// not when the engine got around to merging it.
func (s *Service) RecordCancellationRequestedAt(ctx context.Context, reason string, requestedAt time.Time) error {
	state, err := s.store.LoadRunState(ctx)
	if err != nil {
		return fmt.Errorf("run.RecordCancellationRequested: %w", err)
	}
	state.Cancellation = &store.Cancellation{
		RequestedAt: requestedAt,
		Reason:      reason,
	}
	if err := s.store.SaveRunState(ctx, state); err != nil {
		return fmt.Errorf("run.RecordCancellationRequested save: %w", err)
	}
	return nil
}

// CancelAllRunningAgents transitions every agent currently in the
// "pending" or "running" state to "canceled". Called by the engine on
// signal receipt so observers see a clean transition rather than agents
// stuck mid-run forever.
func (s *Service) CancelAllRunningAgents(ctx context.Context, reason string) error {
	state, err := s.store.LoadRunState(ctx)
	if err != nil {
		return fmt.Errorf("run.CancelAllRunningAgents: %w", err)
	}
	for name := range state.Agents {
		ts := state.Agents[name]
		if ts.Status != "running" && ts.Status != "pending" {
			continue
		}
		if err := s.RecordAgentCancel(ctx, name, reason); err != nil {
			return err
		}
	}
	return nil
}

// RecordTeamFail transitions a team to failed and records the error summary.
func (s *Service) RecordTeamFail(ctx context.Context, team string, cause error) error {
	now := s.clock().UTC()
	causeText := ""
	if cause != nil {
		causeText = cause.Error()
	}

	if err := s.store.UpdateAgentState(ctx, team, func(ts *store.AgentState) {
		ts.Status = "failed"
		ts.EndedAt = now
		ts.LastError = causeText
	}); err != nil {
		return fmt.Errorf("run.RecordTeamFail %s: %w", team, err)
	}

	s.mirrorRegistry("RecordTeamFail", team, func(e *workspace.RegistryEntry) {
		e.Status = "failed"
		e.EndedAt = now
	})
	return nil
}

// End releases the run lock. It is safe to call multiple times.
// The error return is reserved for future use; today it is always nil.
func (s *Service) End(active *Active) error {
	if active == nil {
		return nil
	}
	active.once.Do(func() {
		if active.release != nil {
			active.release()
		}
	})
	return nil
}

// mirrorRegistry best-effort mirrors a team state transition into registry.json.
// state.json (written above) is authoritative; registry.json is a human-facing
// view used by status/monitor, so a mirror failure is logged and swallowed
// rather than failing the whole run.
func (s *Service) mirrorRegistry(op, team string, fn func(*workspace.RegistryEntry)) {
	if s.workspace == nil {
		return
	}
	if err := s.workspace.UpdateRegistryEntry(team, fn); err != nil {
		s.warn("run.%s registry mirror %s: %s", op, team, err)
	}
}

// archivePrevious retires the currently-active run (if any). An empty runID
// tells the store to archive whichever run is currently active.
func (s *Service) archivePrevious(ctx context.Context) error {
	err := s.store.ArchiveRun(ctx, "")
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("run.Begin archive previous run: %w", err)
	default:
		return nil
	}
}

func (s *Service) ensureWorkspace() (*workspace.Workspace, error) {
	if s.workspace == nil {
		return nil, nil
	}
	if _, err := workspace.Ensure(s.workspace.Path); err != nil {
		return nil, fmt.Errorf("run.Begin ensure workspace: %w", err)
	}
	return s.workspace, nil
}

func (s *Service) seedState(cfg *config.Config) (*store.RunState, error) {
	now := s.clock().UTC()
	backend := cfg.Backend.Kind
	if backend == "" {
		backend = "local"
	}
	state := &store.RunState{
		Project:   cfg.Name,
		Backend:   backend,
		RunID:     now.Format("20060102T150405.000000000Z"),
		StartedAt: now,
		Agents:    make(map[string]store.AgentState, len(cfg.Agents)),
	}
	tiers, err := dag.BuildTiers(cfg.Agents)
	if err != nil {
		return nil, err
	}
	tierByTeam := make(map[string]int, len(cfg.Agents))
	for tierIdx, names := range tiers {
		for _, name := range names {
			tierByTeam[name] = tierIdx
		}
	}
	for i := range cfg.Agents {
		tier := tierByTeam[cfg.Agents[i].Name]
		state.Agents[cfg.Agents[i].Name] = store.AgentState{
			Status: "pending",
			Tier:   &tier,
		}
	}
	return state, nil
}

func (s *Service) seedWorkspaceFiles(ws *workspace.Workspace, cfg *config.Config) error {
	if ws == nil {
		return nil
	}
	// Workspace seeding now boils down to the registry mirror. The file
	// message bus was removed in v3 phase A — see DESIGN-v3 §6 for the
	// `steer` + `signal_completion(artifacts={...})` replacement story.
	if err := ws.SeedRegistry(cfg); err != nil {
		return fmt.Errorf("run.Begin seed registry: %w", err)
	}
	return nil
}
