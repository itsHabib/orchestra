package orchestra

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/itsHabib/orchestra/internal/event"
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
// status snapshots, programmatic cancellation, or events and steering.
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
		done:         make(chan struct{}),
		cancel:       cancel,
		phase:        PhaseInitializing,
		currentTier:  -1,
		startedAt:    time.Now(),
		events:       make(chan Event, options.eventBuffer),
		eventHandler: options.eventHandler,
	}

	go func() {
		defer close(h.events)
		defer h.flushDropped()
		defer close(h.done)
		defer h.setPhase(PhaseDone)
		defer release()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				h.runErr = fmt.Errorf("orchestra: engine panic: %v", r)
			}
		}()
		h.result, h.runErr = runWithLockedWorkspace(engineCtx, cfg, &options, absWorkspace, h)
		h.Emit(Event{Kind: EventRunComplete, Tier: -1, At: time.Now()})
	}()

	return h, nil
}

// Handle is a live orchestra run. All methods are goroutine-safe except
// the channel returned by [Handle.Events], which is single-consumer.
// After [Handle.Wait] has returned, [Handle.Send] / [Handle.Interrupt]
// return [ErrClosed]; [Handle.Cancel] becomes a no-op without error;
// [Handle.Events] returns the same channel, now closed.
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

	// events is the bounded channel returned by Events(). Closed once the
	// engine goroutine has emitted EventRunComplete.
	events chan Event
	// eventHandler is the optional synchronous callback set via
	// WithEventHandler. Invoked on the emit path before the channel send.
	eventHandler func(Event)
	// emitMu serializes the drop-oldest sequence so concurrent Emit calls
	// don't race the receive-then-send pattern. Single mutex is fine —
	// emission is fast (channel ops only) and the engine emits at most
	// one event per state transition per team.
	emitMu sync.Mutex
	// dropCount accumulates the number of events dropped from the bounded
	// buffer since the last EventDropped emission. Reset to zero each time
	// the dropped indicator is sent. Guarded by emitMu.
	dropCount int
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
	h.cancel()
}

// Status returns a cheap snapshot of current run state. Backed by an
// in-memory struct guarded by sync.RWMutex; safe to call frequently
// from polling-style UIs.
//
// Best-effort consistent: individual fields are internally consistent,
// but multiple fields read together may straddle a state transition.
// For strict consistency, consume [Handle.Events].
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
	snapCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	state, err := svc.Snapshot(snapCtx)
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

// Events returns a receive-only channel of structured run events. Events
// from a single team are delivered in order; events across parallel teams
// in the same tier are not strictly ordered. The channel closes when the
// run reaches a terminal state.
//
// Single-consumer. Calling Events multiple times returns the same channel;
// sharing the channel between goroutines yields nondeterministic delivery
// (standard Go channel semantics).
//
// If the consumer falls behind, the bounded buffer (default 256, set via
// [WithEventBuffer]) drops the oldest event and the next emission is
// preceded by an [EventDropped] event so the consumer can detect
// backpressure.
//
// Experimental.
func (h *Handle) Events() <-chan Event {
	return h.events
}

// Emit implements the engine-side [event.Emitter] contract: deliver ev to
// the optional synchronous handler and the bounded channel. Bounded
// channel is drop-oldest — when full, Emit dequeues one buffered event,
// increments the drop counter, and sends ev. The next emission is
// preceded by an [EventDropped] event with the accumulated count.
//
// Emit is safe for concurrent use by team goroutines. Internally the
// drop-oldest sequence is mutex-guarded so the receive-then-send pair is
// atomic with respect to other emitters.
//
//nolint:gocritic // Event-by-value is part of the public Emitter contract; pointer would surprise SDK callers.
func (h *Handle) Emit(ev Event) {
	if h.eventHandler != nil {
		h.eventHandler(ev)
	}
	h.emitMu.Lock()
	defer h.emitMu.Unlock()

	// If we have prior drops to surface, deliver the synthetic event
	// before the real one. The synthetic event uses the same drop-oldest
	// path on overflow — losing the dropped indicator itself is fine
	// because the count keeps accumulating until the consumer catches up.
	if h.dropCount > 0 && ev.Kind != EventDropped {
		dropped := Event{
			Kind:      EventDropped,
			Tier:      -1,
			DropCount: h.dropCount,
			At:        time.Now(),
		}
		h.dropCount = 0
		if h.eventHandler != nil {
			h.eventHandler(dropped)
		}
		h.deliverLocked(dropped)
	}
	h.deliverLocked(ev)
}

// flushDropped emits a final EventDropped if any events were dropped
// during the run. Called from the engine epilogue after the last real
// event has been emitted but before the channel is closed, so consumers
// that drain after Wait() see backpressure that accumulated in the tail
// of the run. To make room, flushDropped drops one buffered event from
// h.events if necessary.
func (h *Handle) flushDropped() {
	h.emitMu.Lock()
	defer h.emitMu.Unlock()
	if h.dropCount == 0 {
		return
	}
	dropped := Event{
		Kind:      EventDropped,
		Tier:      -1,
		DropCount: h.dropCount,
		At:        time.Now(),
	}
	h.dropCount = 0
	if h.eventHandler != nil {
		h.eventHandler(dropped)
	}
	h.deliverLocked(dropped)
}

// deliverLocked attempts a non-blocking send on h.events. On overflow it
// drains exactly one event (the oldest) and retries. dropCount is
// incremented for each dropped event so the next non-Dropped emission can
// surface the count via EventDropped. Caller must hold h.emitMu.
//
//nolint:gocritic // Mirrors the Emit contract; converting to pointer here would force allocations on the hot path for no win.
func (h *Handle) deliverLocked(ev Event) {
	select {
	case h.events <- ev:
		return
	default:
	}
	// Buffer full: drop oldest, retry. Receive can race with a closing
	// channel only after the engine has finished the run, at which point
	// no Emit should be in flight; default the receive to skip blocking.
	select {
	case <-h.events:
		h.dropCount++
	default:
	}
	select {
	case h.events <- ev:
	default:
		// Pathological: still full after dropping. Treat ev itself as
		// dropped; the running counter will surface it on the next
		// successful send.
		h.dropCount++
	}
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
	// failure) or the run is canceled.
	PhaseRunning Phase = "running"
	// PhaseCompleting indicates that all tiers have returned successfully
	// and the engine is settling: stopping the coordinator, building the
	// final Result. Wait has not yet returned. Only reached on the success
	// path; canceled or failed runs transition directly from PhaseRunning
	// to PhaseDone.
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
// EventKind is an alias for [event.Kind] so the SDK surface stays
// independent of the internal package layout while engine code emits a
// single concrete type.
//
// Experimental.
type EventKind = event.Kind

// Event is a structured observation of a run-time event. Field
// population is per-Kind; consult the doc on each Kind constant for
// which fields are non-zero. Aliased from [event.Event].
//
// Experimental.
type Event = event.Event

// Event kinds. Each constant's doc on [event.Kind] describes when the
// event fires and which Event fields are populated.
//
// Experimental.
const (
	// EventTierStart fires once at the top of each tier, before any
	// team in the tier begins. Carries tier index and the joined team
	// list as Message.
	EventTierStart = event.KindTierStart
	// EventTeamStart fires once per team when its goroutine begins.
	EventTeamStart = event.KindTeamStart
	// EventTeamMessage carries the agent's natural-language output.
	// On the local backend this is the raw spawner ProgressFunc line;
	// on the managed-agents backend it is the agent.message text or
	// the user.message echo (with "human:" prefix).
	EventTeamMessage = event.KindTeamMessage
	// EventToolCall fires when an agent invokes a tool. Today only the
	// managed-agents backend distinguishes tool calls from natural
	// output; the local backend collapses everything to
	// EventTeamMessage.
	EventToolCall = event.KindToolCall
	// EventToolResult fires when a tool returns its result to the
	// agent. Today only the managed-agents backend emits this.
	EventToolResult = event.KindToolResult
	// EventTeamComplete fires when a team finishes successfully.
	EventTeamComplete = event.KindTeamComplete
	// EventTeamFailed fires when a team's run errors out.
	EventTeamFailed = event.KindTeamFailed
	// EventTierComplete fires once at the end of each tier, after every
	// team in the tier has settled.
	EventTierComplete = event.KindTierComplete
	// EventRunComplete fires once when the engine has finished all
	// tiers and is about to close the events channel.
	EventRunComplete = event.KindRunComplete
	// EventDropped is synthetic — emitted when the bounded buffer
	// dropped one or more events. DropCount carries the cumulative
	// count since the last EventDropped.
	EventDropped = event.KindDropped
	// EventInfo carries an engine-level informational message.
	EventInfo = event.KindInfo
	// EventWarn carries an engine-level non-fatal warning.
	EventWarn = event.KindWarn
	// EventError carries an engine-level error message; the run's
	// actual error is still surfaced via Wait()'s return value.
	EventError = event.KindError
)

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
