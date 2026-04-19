package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/messaging"
	"github.com/itsHabib/orchestra/internal/workspace"
	"github.com/itsHabib/orchestra/pkg/store"
	"github.com/itsHabib/orchestra/pkg/store/filestore"
)

// TeamResult is the structured result recorded for a completed team.
type TeamResult = workspace.TeamResult

// Service owns run lifecycle choreography above the Store.
type Service struct {
	store     store.Store
	clock     func() time.Time
	workspace *workspace.Workspace
}

// Active represents a live run holding the exclusive run lock.
type Active struct {
	RunID string
	State *store.RunState
	Bus   *messaging.Bus

	release func()
	once    sync.Once
}

// New creates a run service backed by the provided Store.
func New(s store.Store) *Service {
	return &Service{store: s, clock: time.Now}
}

// NewFile creates a run service backed by a filesystem workspace.
func NewFile(path string) *Service {
	return &Service{
		store:     filestore.New(path),
		clock:     time.Now,
		workspace: workspace.ForPath(path),
	}
}

// IsNotFound reports whether err wraps the store not-found sentinel.
func IsNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

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

	state := s.seedState(cfg)
	if err := s.store.SaveRunState(ctx, state); err != nil {
		return nil, fmt.Errorf("run.Begin seed state: %w", err)
	}

	bus, err := s.seedWorkspaceFiles(ws, cfg)
	if err != nil {
		return nil, err
	}

	active.RunID = state.RunID
	active.State = state
	active.Bus = bus
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
	if err := s.store.UpdateTeamState(ctx, team, func(ts *store.TeamState) {
		ts.Status = "running"
		ts.StartedAt = now
		ts.EndedAt = time.Time{}
		ts.LastError = ""
	}); err != nil {
		return fmt.Errorf("run.RecordTeamStart %s: %w", team, err)
	}

	if s.workspace == nil {
		return nil
	}
	if err := s.workspace.UpdateRegistryEntry(team, func(e *workspace.RegistryEntry) {
		e.Status = "running"
		e.StartedAt = now
		e.EndedAt = time.Time{}
	}); err != nil {
		return fmt.Errorf("run.RecordTeamStart registry %s: %w", team, err)
	}
	return nil
}

// RecordTeamComplete transitions a team to done and records result counters.
func (s *Service) RecordTeamComplete(ctx context.Context, team string, result *TeamResult) error {
	if result == nil {
		return fmt.Errorf("%w: nil team result", store.ErrInvalidArgument)
	}

	now := s.clock().UTC()
	if err := s.store.UpdateTeamState(ctx, team, func(ts *store.TeamState) {
		ts.Status = "done"
		ts.EndedAt = now
		ts.SessionID = result.SessionID
		ts.LastError = ""
		ts.ResultSummary = result.Result
		ts.CostUSD = result.CostUSD
		ts.DurationMs = result.DurationMs
		ts.InputTokens = result.InputTokens
		ts.OutputTokens = result.OutputTokens
	}); err != nil {
		return fmt.Errorf("run.RecordTeamComplete %s: %w", team, err)
	}

	if s.workspace == nil {
		return nil
	}
	if err := s.workspace.UpdateRegistryEntry(team, func(e *workspace.RegistryEntry) {
		e.Status = "done"
		e.SessionID = result.SessionID
		e.EndedAt = now
	}); err != nil {
		return fmt.Errorf("run.RecordTeamComplete registry %s: %w", team, err)
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

	if err := s.store.UpdateTeamState(ctx, team, func(ts *store.TeamState) {
		ts.Status = "failed"
		ts.EndedAt = now
		ts.LastError = causeText
	}); err != nil {
		return fmt.Errorf("run.RecordTeamFail %s: %w", team, err)
	}

	if s.workspace == nil {
		return nil
	}
	if err := s.workspace.UpdateRegistryEntry(team, func(e *workspace.RegistryEntry) {
		e.Status = "failed"
		e.EndedAt = now
	}); err != nil {
		return fmt.Errorf("run.RecordTeamFail registry %s: %w", team, err)
	}
	return nil
}

// End releases the run lock. It is safe to call multiple times.
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

func (s *Service) seedState(cfg *config.Config) *store.RunState {
	now := s.clock().UTC()
	state := &store.RunState{
		Project:   cfg.Name,
		Backend:   "local",
		RunID:     now.Format("20060102T150405.000000000Z"),
		StartedAt: now,
		Teams:     make(map[string]store.TeamState, len(cfg.Teams)),
	}
	for i := range cfg.Teams {
		state.Teams[cfg.Teams[i].Name] = store.TeamState{Status: "pending"}
	}
	return state
}

func (s *Service) seedWorkspaceFiles(ws *workspace.Workspace, cfg *config.Config) (*messaging.Bus, error) {
	if ws == nil {
		return nil, nil
	}
	if err := ws.SeedRegistry(cfg); err != nil {
		return nil, fmt.Errorf("run.Begin seed registry: %w", err)
	}

	names := make([]string, len(cfg.Teams))
	for i := range cfg.Teams {
		names[i] = cfg.Teams[i].Name
	}
	bus := messaging.NewBus(ws.MessagesPath())
	if err := bus.InitInboxes(names); err != nil {
		return nil, fmt.Errorf("run.Begin init message bus: %w", err)
	}
	return bus, nil
}
