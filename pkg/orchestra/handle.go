package orchestra

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Run executes the workflow described by cfg and returns its result. It is
// a thin convenience for one-shot blocking callers, equivalent to:
//
//	h, err := Start(ctx, cfg, opts...)
//	if err != nil {
//	    return nil, err
//	}
//	return h.Wait()
//
// Use [Run] when you don't need a [Handle]. Use [Start] when you want
// status snapshots, programmatic cancellation, or (in later PRs) events
// and steering.
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
	h, err := Start(ctx, cfg, opts...)
	if err != nil {
		return nil, err
	}
	return h.Wait()
}

// Start launches an orchestra run asynchronously and returns a [Handle]
// that observes and steers the running workflow. Start returns as soon
// as the workspace lock is acquired and the engine goroutine has been
// spawned — before any team begins. To get blocking behavior, call
// [Handle.Wait].
//
// Errors that occur before any tier starts (nil cfg, validation failure,
// workspace contention) are returned directly from Start with a nil
// Handle. Errors that occur during the run surface from [Handle.Wait].
//
// Concurrent Start calls against the same resolved workspace return
// [ErrRunInProgress], identical to [Run].
//
// Experimental: signature and Handle shape may change.
func Start(ctx context.Context, cfg *Config, opts ...Option) (*Handle, error) {
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

	cfg.ResolveDefaults()
	if _, err := cfg.Validate(); err != nil {
		release()
		return nil, fmt.Errorf("orchestra: validate config: %w", err)
	}

	engineCtx, cancel := context.WithCancel(ctx)
	h := &Handle{
		done:        make(chan struct{}),
		cancel:      cancel,
		phase:       PhaseInitializing,
		currentTier: -1,
		startedAt:   time.Now(),
	}

	go func() {
		defer close(h.done)
		defer h.setPhase(PhaseDone)
		defer release()
		defer func() {
			if r := recover(); r != nil {
				h.runErr = fmt.Errorf("orchestra: engine panic: %v", r)
			}
		}()
		h.result, h.runErr = runWithLockedWorkspace(engineCtx, cfg, &options, absWorkspace, h)
	}()

	return h, nil
}

// Handle is a live orchestra run. All methods are goroutine-safe except
// the channel returned by [Handle.Events], which is single-consumer.
// After [Handle.Wait] has returned, [Handle.Send] / [Handle.Interrupt]
// return [ErrClosed]; [Handle.Cancel] becomes a no-op without error;
// [Handle.Events] returns the same already-closed channel.
//
// Experimental.
type Handle struct {
	// done is closed by the engine goroutine once the run reaches a
	// terminal state. Wait blocks on it.
	done chan struct{}

	// cancel cancels the internal context that wraps the caller's ctx.
	// Cancel() invokes it; Wait returns once the engine settles.
	cancel context.CancelFunc

	// runService is set by the engine once it has been constructed so
	// Status() can call Snapshot mid-run. Guarded by mu.
	runService snapshotter

	// result and runErr are written by the engine goroutine and read by
	// Wait after done is closed. Reads before done is closed are not
	// safe; readers must observe close(done) first.
	result *Result
	runErr error

	mu          sync.RWMutex
	phase       Phase
	currentTier int
	startedAt   time.Time
}

// snapshotter is the minimal contract the Handle needs from the run
// service so Status() can build a snapshot mid-run without coupling
// pkg/orchestra to internal/run beyond what the engine already needs.
type snapshotter interface {
	Snapshot(ctx context.Context) (*RunState, error)
}

// Wait blocks until the run reaches a terminal state and returns the
// final [Result]. Wait may be called from any goroutine; subsequent
// calls return the same cached (*Result, error). Result is non-nil even
// on failure or cancellation, reflecting whatever state was reached.
//
// Experimental.
func (h *Handle) Wait() (*Result, error) {
	<-h.done
	return h.result, h.runErr
}

// Cancel signals the run to stop cooperatively. In-flight teams receive
// context cancellation; the engine waits for them to settle. Cancel
// returns immediately; call [Handle.Wait] to know when shutdown is
// complete.
//
// Experimental.
func (h *Handle) Cancel() {
	if h.cancel != nil {
		h.cancel()
	}
}

// Status returns a cheap snapshot of current run state. Backed by an
// in-memory struct guarded by sync.RWMutex; safe to call frequently
// from polling-style UIs.
//
// Best-effort consistent: individual fields are internally consistent,
// but multiple fields read together may straddle a state transition.
// For strict consistency, consume [Handle.Events] (wired in PR 2).
//
// Experimental.
func (h *Handle) Status() Status {
	h.mu.RLock()
	phase := h.phase
	currentTier := h.currentTier
	startedAt := h.startedAt
	svc := h.runService
	h.mu.RUnlock()

	status := Status{
		Phase:       phase,
		CurrentTier: currentTier,
		StartedAt:   startedAt,
		Elapsed:     time.Since(startedAt),
	}
	if svc == nil {
		return status
	}
	state, err := svc.Snapshot(context.Background())
	if err != nil || state == nil {
		return status
	}
	teams := make(map[string]TeamSnapshot, len(state.Teams))
	var totalCost float64
	for name := range state.Teams {
		ts := state.Teams[name]
		teams[name] = TeamSnapshot{TeamState: ts}
		totalCost += ts.CostUSD
	}
	status.Teams = teams
	status.TotalCost = totalCost
	return status
}

// Events returns a receive-only channel of structured run events. In
// PR 1 the channel is a package-level pre-closed stub so the API shape
// compiles; real event emission is wired in PR 2.
//
// Experimental.
func (h *Handle) Events() <-chan Event {
	return closedEventChan
}

// Send delivers a steering message to a running team — equivalent of
// `orchestra msg <team> <message>` from the CLI.
//
// PR 1 stub: always returns [ErrClosed]. The real implementation lands
// in PR 3 and will return [ErrTeamNotRunning] when the team's state is
// not "running", or [ErrClosed] after [Handle.Wait] has returned.
//
// Experimental.
func (h *Handle) Send(team, message string) error {
	_ = team
	_ = message
	return ErrClosed
}

// Interrupt asks a running team to stop its current turn and return
// control to the engine — equivalent of `orchestra interrupt <team>`.
//
// PR 1 stub: always returns [ErrClosed]. The real implementation lands
// in PR 3 and will return [ErrTeamNotRunning] when the team's state is
// not "running", or [ErrClosed] after [Handle.Wait] has returned.
//
// Experimental.
func (h *Handle) Interrupt(team string) error {
	_ = team
	return ErrClosed
}

// setPhase atomically updates the run phase. Called from the engine
// goroutine on tier transitions.
func (h *Handle) setPhase(p Phase) {
	h.mu.Lock()
	h.phase = p
	h.mu.Unlock()
}

// setCurrentTier atomically updates the active tier index. Called from
// the engine goroutine at the top of each runTier.
func (h *Handle) setCurrentTier(tierIdx int) {
	h.mu.Lock()
	h.currentTier = tierIdx
	h.mu.Unlock()
}

// setRunService records the run service so Status() can read team data
// mid-run. Called from the engine once the service is constructed.
func (h *Handle) setRunService(s snapshotter) {
	h.mu.Lock()
	h.runService = s
	h.mu.Unlock()
}

// === Phase ================================================================

// Phase identifies which lifecycle stage a run is currently in. Status()
// returns the phase under an RWMutex; values progress monotonically from
// [PhaseInitializing] to [PhaseDone].
//
// Experimental.
type Phase string

// Run lifecycle phases. Status reports the current phase via
// [Status.Phase]; transitions happen inside the engine goroutine.
const (
	// PhaseInitializing is the brief window between Start returning and
	// the first tier beginning. Workspace lock is held; the engine has
	// not yet dispatched any team.
	PhaseInitializing Phase = "initializing"
	// PhaseRunning indicates that the engine has begun dispatching
	// tiers. It remains the phase until all tiers finish (success or
	// failure) or the run is cancelled.
	PhaseRunning Phase = "running"
	// PhaseCompleting indicates that all tiers have returned and the
	// engine is settling: stopping the coordinator, building the final
	// Result. Wait has not yet returned.
	PhaseCompleting Phase = "completing"
	// PhaseDone is terminal: Wait has either returned or is unblocked
	// and the engine goroutine has exited. Send/Interrupt return
	// ErrClosed in this phase.
	PhaseDone Phase = "done"
)

// === Status ===============================================================

// Status is the in-memory snapshot returned by [Handle.Status]. Fields
// are individually consistent under the Handle's RWMutex; multiple
// fields read together may straddle a transition.
//
// Experimental: field set may grow as more lifecycle events are tracked.
type Status struct {
	// Phase is the current lifecycle phase. See [Phase] constants.
	Phase Phase
	// CurrentTier is the index of the tier currently executing, or -1
	// before any tier has begun.
	CurrentTier int
	// Teams maps team name to a live snapshot of its TeamState. nil
	// before the engine has constructed its run service or when the
	// snapshot read fails.
	Teams map[string]TeamSnapshot
	// StartedAt is the wall-clock time at which Start returned the
	// Handle.
	StartedAt time.Time
	// Elapsed is time.Since(StartedAt) at the moment Status was called.
	Elapsed time.Duration
	// TotalCost is the sum of CostUSD across all teams in the snapshot.
	TotalCost float64
}

// TeamSnapshot is the live counterpart of [TeamResult] — same shape
// (status, turns, cost, tokens) but populated mid-run. After
// [Handle.Wait] returns, snapshots match the corresponding
// [TeamResult] exactly.
//
// Experimental.
type TeamSnapshot struct {
	TeamState
}

// === Event ================================================================

// EventKind enumerates the structured event types emitted during a run.
// PR 1 declares the type so the API shape compiles; PR 2 introduces the
// individual kind constants and wires emission.
//
// Experimental.
type EventKind int

// Event is a structured observation of a runtime event. PR 1 declares
// the type so the API shape compiles; PR 2 populates fields and emits
// instances on the Events channel.
//
// Experimental.
type Event struct {
	Kind EventKind
	At   time.Time
}

// closedEventChan is the package-level pre-closed channel returned by
// [Handle.Events] in PR 1. PR 2 replaces this with a per-Handle bounded
// channel driven by the engine emitter.
var closedEventChan = func() chan Event {
	c := make(chan Event)
	close(c)
	return c
}()

// === Errors ===============================================================

// ErrTeamNotRunning is returned by [Handle.Send] and [Handle.Interrupt]
// when the addressed team's TeamState.Status is not "running". The team
// may not have started yet, may have already completed, or may have
// failed. Declared in PR 1; wired by PR 3.
//
// Experimental.
var ErrTeamNotRunning = errors.New("orchestra: team not running")

// ErrClosed is returned by [Handle.Send] and [Handle.Interrupt] after
// [Handle.Wait] has returned (the run reached a terminal state and the
// Handle is no longer connected to a live engine).
//
// Experimental.
var ErrClosed = errors.New("orchestra: handle closed")
