package spawner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/itsHabib/orchestra/internal/store"
)

const (
	defaultSessionSeenLimit = 512
	defaultAPIMaxAttempts   = 5
	defaultAPIRetryBase     = time.Second
	defaultAPIRetryMax      = 30 * time.Second
)

type managedSessionAPI interface {
	New(context.Context, anthropic.BetaSessionNewParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSession, error)
	Get(context.Context, string, anthropic.BetaSessionGetParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSession, error)
}

type managedSessionEventsAPI interface {
	Send(context.Context, string, anthropic.BetaSessionEventSendParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSendSessionEvents, error)
	StreamEvents(context.Context, string, anthropic.BetaSessionEventStreamParams, ...option.RequestOption) eventStream
	ListAutoPaging(context.Context, string, anthropic.BetaSessionEventListParams, ...option.RequestOption) eventPager
}

type eventStream interface {
	Next() bool
	Current() managedEvent
	Err() error
	Close() error
}

type eventPager interface {
	Next() bool
	Current() managedEvent
	Err() error
}

type managedEvent struct {
	raw []byte
}

type sdkSessionEventsAPI struct {
	events *anthropic.BetaSessionEventService
}

func (a sdkSessionEventsAPI) Send(ctx context.Context, sessionID string, params anthropic.BetaSessionEventSendParams, opts ...option.RequestOption) (*anthropic.BetaManagedAgentsSendSessionEvents, error) {
	return a.events.Send(ctx, sessionID, params, opts...)
}

func (a sdkSessionEventsAPI) StreamEvents(ctx context.Context, sessionID string, params anthropic.BetaSessionEventStreamParams, opts ...option.RequestOption) eventStream {
	return &sdkEventStream{stream: a.events.StreamEvents(ctx, sessionID, params, opts...)}
}

func (a sdkSessionEventsAPI) ListAutoPaging(ctx context.Context, sessionID string, params anthropic.BetaSessionEventListParams, opts ...option.RequestOption) eventPager { //nolint:gocritic // signature matches SDK value-type parameter
	return &sdkEventPager{pager: a.events.ListAutoPaging(ctx, sessionID, params, opts...)}
}

type sdkEventStream struct {
	stream interface {
		Next() bool
		Current() anthropic.BetaManagedAgentsStreamSessionEventsUnion
		Err() error
		Close() error
	}
}

func (s *sdkEventStream) Next() bool { return s.stream.Next() }
func (s *sdkEventStream) Current() managedEvent {
	return managedEvent{raw: []byte(s.stream.Current().RawJSON())}
}
func (s *sdkEventStream) Err() error   { return s.stream.Err() }
func (s *sdkEventStream) Close() error { return s.stream.Close() }

type sdkEventPager struct {
	pager interface {
		Next() bool
		Current() anthropic.BetaManagedAgentsSessionEventUnion
		Err() error
	}
}

func (p *sdkEventPager) Next() bool { return p.pager.Next() }
func (p *sdkEventPager) Current() managedEvent {
	return managedEvent{raw: []byte(p.pager.Current().RawJSON())}
}
func (p *sdkEventPager) Err() error { return p.pager.Err() }

type unsupportedSessionAPI struct{}

func (unsupportedSessionAPI) New(context.Context, anthropic.BetaSessionNewParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSession, error) {
	return nil, ErrUnsupported
}

func (unsupportedSessionAPI) Get(context.Context, string, anthropic.BetaSessionGetParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSession, error) {
	return nil, ErrUnsupported
}

type unsupportedSessionEventsAPI struct{}

func (unsupportedSessionEventsAPI) Send(context.Context, string, anthropic.BetaSessionEventSendParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSendSessionEvents, error) {
	return nil, ErrUnsupported
}

func (unsupportedSessionEventsAPI) StreamEvents(context.Context, string, anthropic.BetaSessionEventStreamParams, ...option.RequestOption) eventStream {
	return errorEventStream{err: ErrUnsupported}
}

func (unsupportedSessionEventsAPI) ListAutoPaging(context.Context, string, anthropic.BetaSessionEventListParams, ...option.RequestOption) eventPager {
	return errorEventPager{err: ErrUnsupported}
}

type errorEventStream struct {
	err error
}

func (s errorEventStream) Next() bool            { return false }
func (s errorEventStream) Current() managedEvent { return managedEvent{} }
func (s errorEventStream) Err() error            { return s.err }
func (s errorEventStream) Close() error          { return nil }

type errorEventPager struct {
	err error
}

func (p errorEventPager) Next() bool            { return false }
func (p errorEventPager) Current() managedEvent { return managedEvent{} }
func (p errorEventPager) Err() error            { return p.err }

// PendingSession is a created-but-not-streaming MA session. It deliberately has
// no Send method; callers must open the event stream first.
type PendingSession struct {
	id             string
	agent          AgentHandle
	env            EnvHandle
	sessions       managedSessionAPI
	events         managedSessionEventsAPI
	teamName       string
	log            io.Writer
	store          store.Store
	summaryWriter  func(teamName, text string) error
	clock          ManagedAgentsClock
	logger         *slog.Logger
	seenLimit      int
	apiMaxAttempts int
	apiRetryBase   time.Duration
	apiRetryMax    time.Duration

	mu       sync.Mutex
	streamed bool
	canceled bool
}

// ID returns the backend session identifier.
func (p *PendingSession) ID() string { return p.id }

// Stream opens the MA event stream and starts the single translator goroutine.
func (p *PendingSession) Stream(ctx context.Context) (*Session, <-chan Event, error) {
	p.mu.Lock()
	switch {
	case p.canceled:
		p.mu.Unlock()
		return nil, nil, errors.New("pending session canceled")
	case p.streamed:
		p.mu.Unlock()
		return nil, nil, errors.New("pending session already streamed")
	}
	p.streamed = true
	p.mu.Unlock()

	stream := p.events.StreamEvents(ctx, p.id, anthropic.BetaSessionEventStreamParams{})
	if stream == nil {
		return nil, nil, errors.New("events: nil stream")
	}

	streamCtx, cancel := context.WithCancel(ctx)
	ch := make(chan Event, 64)
	session := &Session{
		id:             p.id,
		agent:          p.agent,
		env:            p.env,
		sessions:       p.sessions,
		events:         p.events,
		teamName:       p.teamName,
		log:            p.log,
		store:          p.store,
		summaryWriter:  p.summaryWriter,
		clock:          p.clock,
		logger:         p.logger,
		seen:           newSeenSet(p.seenLimit),
		apiMaxAttempts: p.apiMaxAttempts,
		apiRetryBase:   p.apiRetryBase,
		apiRetryMax:    p.apiRetryMax,
		eventsCh:       ch,
		cancel:         cancel,
		done:           make(chan struct{}),
	}
	session.setStream(stream)
	go session.run(streamCtx, stream)
	return session, ch, nil
}

// Cancel marks a pending session as locally abandoned. It does not archive the
// MA session; P1.8 can decide whether to resume it later.
func (p *PendingSession) Cancel(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.canceled {
		return nil
	}
	p.canceled = true
	return nil
}

// Session is a streaming MA session with Send capability.
type Session struct {
	id             string
	agent          AgentHandle
	env            EnvHandle
	sessions       managedSessionAPI
	events         managedSessionEventsAPI
	teamName       string
	log            io.Writer
	store          store.Store
	summaryWriter  func(teamName, text string) error
	clock          ManagedAgentsClock
	logger         *slog.Logger
	seen           *seenSet
	apiMaxAttempts int
	apiRetryBase   time.Duration
	apiRetryMax    time.Duration

	eventsCh chan Event
	cancel   context.CancelFunc
	done     chan struct{}

	streamMu sync.Mutex
	stream   eventStream

	errMu         sync.Mutex
	err           error
	lastAgentText string
	lastEventID   EventID
}

// ID returns the MA session ID.
func (s *Session) ID() string { return s.id }

// Events returns the channel opened by PendingSession.Stream.
func (s *Session) Events(context.Context) (<-chan Event, error) {
	if s.eventsCh == nil {
		return nil, errors.New("session stream not opened")
	}
	return s.eventsCh, nil
}

// Err returns the translator error after the event channel closes.
func (s *Session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// Status fetches the current MA session status.
func (s *Session) Status(ctx context.Context) (SessionStatus, error) {
	var ma *anthropic.BetaManagedAgentsSession
	err := s.withRetry(ctx, "session_get", func(ctx context.Context) error {
		var err error
		ma, err = s.sessions.Get(ctx, s.id, anthropic.BetaSessionGetParams{})
		return err
	})
	if err != nil {
		return SessionStatus{}, err
	}
	return sessionStatusFromMA(ma), nil
}

// Usage fetches cumulative MA token usage.
func (s *Session) Usage(ctx context.Context) (Usage, error) {
	var ma *anthropic.BetaManagedAgentsSession
	err := s.withRetry(ctx, "session_usage", func(ctx context.Context) error {
		var err error
		ma, err = s.sessions.Get(ctx, s.id, anthropic.BetaSessionGetParams{})
		return err
	})
	if err != nil {
		return Usage{}, err
	}
	return usageFromMASession(&ma.Usage), nil
}

// Send delivers a user.message or user.interrupt to MA.
func (s *Session) Send(ctx context.Context, event *UserEvent) error {
	params, err := toSessionEventSendParams(event)
	if err != nil {
		return err
	}
	return s.withRetry(ctx, "session_event_send", func(ctx context.Context) error {
		_, err := s.events.Send(ctx, s.id, params)
		return err
	})
}

// History lists already-recorded events, optionally after a known event ID.
func (s *Session) History(ctx context.Context, after EventID) ([]Event, error) {
	params := anthropic.BetaSessionEventListParams{
		Limit: anthropic.Int(100),
		Order: anthropic.BetaSessionEventListParamsOrderAsc,
	}
	pager := s.events.ListAutoPaging(ctx, s.id, params)
	afterSeen := after == ""
	var out []Event
	for pager.Next() {
		raw := pager.Current().raw
		evt, parsed, err := translateMAEvent(raw, s.now())
		if err != nil {
			return nil, err
		}
		if !afterSeen {
			if EventID(parsed.ID) == after {
				afterSeen = true
			}
			continue
		}
		out = append(out, evt)
	}
	if err := pager.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListProducedFiles is out of scope for the P1.4 text-only managed-agents flow.
func (s *Session) ListProducedFiles(context.Context) ([]FileRef, error) {
	return nil, ErrUnsupported
}

// DownloadFile is out of scope for the P1.4 text-only managed-agents flow.
func (s *Session) DownloadFile(context.Context, FileRef, io.Writer) error {
	return ErrUnsupported
}

// Interrupt sends a user.interrupt event.
func (s *Session) Interrupt(ctx context.Context) error {
	return s.Send(ctx, &UserEvent{Type: UserEventTypeInterrupt})
}

// Cancel stops the local stream reader. It does not archive the MA session;
// the backend session keeps running until it idles on its own.
func (s *Session) Cancel(context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.done != nil {
		<-s.done
	}
	return nil
}

func (s *Session) run(ctx context.Context, stream eventStream) {
	defer close(s.done)
	defer close(s.eventsCh)
	defer func() { _ = stream.Close() }()

	attempt := 1
	for {
		outcome, err := s.consumeStream(ctx, stream)
		if err != nil {
			s.setErr(err)
			return
		}
		if outcome.Terminal {
			return
		}
		if outcome.Processed {
			attempt = 1
		}
		streamErr := stream.Err()
		if streamErr == nil {
			return
		}

		nextStream, terminal, err := s.reconnect(ctx, stream, streamErr, attempt)
		if err != nil {
			s.setErr(err)
			return
		}
		if terminal {
			return
		}
		stream = nextStream
		attempt++
	}
}

// streamOutcome reports how consumeStream exited. Terminal means a terminal
// event was observed; Processed means at least one event was received from
// the stream before it ended.
type streamOutcome struct {
	Terminal  bool
	Processed bool
}

// consumeStream drains events from the stream until it returns terminal, an
// error, or the stream ends.
func (s *Session) consumeStream(ctx context.Context, stream eventStream) (streamOutcome, error) {
	var out streamOutcome
	for stream.Next() {
		out.Processed = true
		terminal, err := s.process(ctx, stream.Current())
		if err != nil {
			return out, err
		}
		if terminal {
			out.Terminal = true
			return out, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Session) reconnect(ctx context.Context, stream eventStream, streamErr error, attempt int) (eventStream, bool, error) {
	if !isRetryableMAError(streamErr) || attempt >= s.retryAttempts() {
		wrapped := fmt.Errorf("events: %w", streamErr)
		_ = s.failTeam(context.WithoutCancel(ctx), "", time.Time{}, wrapped.Error())
		return nil, false, wrapped
	}
	if waitErr := s.waitRetry(ctx, streamErr, attempt); waitErr != nil {
		return nil, false, waitErr
	}

	_ = stream.Close()
	next := s.events.StreamEvents(ctx, s.id, anthropic.BetaSessionEventStreamParams{})
	if next == nil {
		return nil, false, errors.New("events: nil stream on reconnect")
	}
	s.setStream(next)

	terminal, backfillErr := s.backfill(ctx)
	if backfillErr != nil {
		return nil, false, backfillErr
	}
	return next, terminal, nil
}

func (s *Session) backfill(ctx context.Context) (bool, error) {
	var terminal bool
	err := s.withRetry(ctx, "events_backfill", func(ctx context.Context) error {
		terminal = false
		params := anthropic.BetaSessionEventListParams{
			Limit: anthropic.Int(100),
			Order: anthropic.BetaSessionEventListParamsOrderAsc,
		}
		pager := s.events.ListAutoPaging(ctx, s.id, params)
		for pager.Next() {
			t, err := s.process(ctx, pager.Current())
			if err != nil {
				return err
			}
			if t {
				terminal = true
				return nil
			}
		}
		return pager.Err()
	})
	if err != nil {
		return false, fmt.Errorf("events backfill: %w", err)
	}
	return terminal, nil
}

func (s *Session) process(ctx context.Context, ev managedEvent) (bool, error) {
	raw := ev.raw
	translated, parsed, err := translateMAEvent(raw, s.now())
	if err != nil {
		s.logger.Warn("unrecognized managed-agents event shape", "team", s.teamName, "session_id", s.id, "error", err)
		return false, nil
	}
	if parsed.ID != "" && s.seen.Has(parsed.ID) {
		return false, nil
	}
	if parsed.ID != "" {
		s.seen.Push(parsed.ID)
		s.lastEventID = EventID(parsed.ID)
	}
	if err := s.writeRawLog(raw); err != nil {
		wrapped := fmt.Errorf("log_write: %w", err)
		_ = s.failTeam(context.WithoutCancel(ctx), parsed.ID, parsed.ProcessedAt, wrapped.Error())
		return true, wrapped
	}

	select {
	case s.eventsCh <- translated:
	case <-ctx.Done():
		return true, ctx.Err()
	}

	return s.apply(ctx, translated, &parsed)
}

func (s *Session) apply(ctx context.Context, event Event, raw *maEvent) (bool, error) {
	switch ev := event.(type) {
	case AgentMessageEvent:
		if ev.Text != "" {
			s.lastAgentText = ev.Text
		}
		return false, nil
	case SessionStatusRunningEvent:
		return false, s.updateTeam(ctx, raw.ID, raw.ProcessedAt, func(ts *store.TeamState) {
			ts.Status = "running"
			if ts.StartedAt.IsZero() {
				ts.StartedAt = raw.ProcessedAt
			}
			ts.EndedAt = time.Time{}
			ts.SessionID = s.id
			ts.AgentID = s.agent.ID
			ts.AgentVersion = s.agent.Version
			ts.LastError = ""
		})
	case SessionStatusIdleEvent:
		return s.applyIdle(ctx, &ev, raw)
	case SessionStatusTerminatedEvent:
		return true, s.updateTeam(ctx, raw.ID, raw.ProcessedAt, func(ts *store.TeamState) {
			ts.Status = "terminated"
			ts.EndedAt = s.eventTime(raw.ProcessedAt)
			ts.SessionID = s.id
			ts.AgentID = s.agent.ID
			ts.AgentVersion = s.agent.Version
			ts.DurationMs = durationMillis(ts.StartedAt, ts.EndedAt)
		})
	case SessionErrorEvent:
		return false, s.updateTeam(ctx, raw.ID, raw.ProcessedAt, func(ts *store.TeamState) {
			ts.LastError = firstNonEmpty(ev.Message, ev.Code, "session error")
			ts.SessionID = s.id
		})
	case SpanModelRequestEndEvent:
		return false, s.updateTeam(ctx, raw.ID, raw.ProcessedAt, func(ts *store.TeamState) {
			ts.InputTokens += ev.Usage.InputTokens
			ts.OutputTokens += ev.Usage.OutputTokens
			ts.CacheCreationInputTokens += ev.Usage.CacheCreationInputTokens
			ts.CacheReadInputTokens += ev.Usage.CacheReadInputTokens
			ts.CostUSD += ev.Usage.CostUSD
			ts.SessionID = s.id
		})
	default:
		return false, nil
	}
}

func (s *Session) applyIdle(ctx context.Context, ev *SessionStatusIdleEvent, raw *maEvent) (bool, error) {
	reason := ev.Status.StopReason.Type
	switch reason {
	case "end_turn":
		if s.summaryWriter != nil {
			if err := s.summaryWriter(s.teamName, s.lastAgentText); err != nil {
				msg := fmt.Sprintf("summary_write: %s", err)
				if stateErr := s.failTeam(ctx, raw.ID, raw.ProcessedAt, msg); stateErr != nil {
					return true, stateErr
				}
				return true, errors.New(msg)
			}
		}
		return true, s.updateTeam(ctx, raw.ID, raw.ProcessedAt, func(ts *store.TeamState) {
			ts.Status = "done"
			ts.EndedAt = s.eventTime(raw.ProcessedAt)
			ts.SessionID = s.id
			ts.AgentID = s.agent.ID
			ts.AgentVersion = s.agent.Version
			ts.LastError = ""
			ts.ResultSummary = s.lastAgentText
			ts.DurationMs = durationMillis(ts.StartedAt, ts.EndedAt)
		})
	case "requires_action":
		return true, s.failTeam(ctx, raw.ID, raw.ProcessedAt, "tool confirmation requested; not supported in v1")
	case "max_turns":
		return true, s.failTeam(ctx, raw.ID, raw.ProcessedAt, "max turns reached")
	case "retries_exhausted":
		return true, s.failTeam(ctx, raw.ID, raw.ProcessedAt, "retries exhausted")
	case "error":
		msg := firstNonEmpty(raw.ErrorMessage(), raw.StopReason.Raw, "session error")
		return true, s.failTeam(ctx, raw.ID, raw.ProcessedAt, "session error: "+msg)
	default:
		return true, s.failTeam(ctx, raw.ID, raw.ProcessedAt, "unknown idle stop reason: "+reason)
	}
}

func (s *Session) updateTeam(ctx context.Context, eventID string, eventAt time.Time, fn func(*store.TeamState)) error {
	if s.store == nil || s.teamName == "" {
		return nil
	}
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return s.store.UpdateTeamState(ctx, s.teamName, func(ts *store.TeamState) {
			fn(ts)
			if eventID != "" {
				ts.LastEventID = eventID
			}
			if !eventAt.IsZero() {
				ts.LastEventAt = eventAt
			}
		})
	}()
	if err == nil {
		return nil
	}
	msg := "state_write: " + err.Error()
	fallbackCtx := context.WithoutCancel(ctx)
	fallbackErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return s.store.UpdateTeamState(fallbackCtx, s.teamName, func(ts *store.TeamState) {
			ts.Status = "failed"
			ts.EndedAt = s.now()
			ts.LastError = msg
		})
	}()
	if fallbackErr != nil {
		s.logger.Error("failed to record managed-agents state fallback", "team", s.teamName, "session_id", s.id, "error", fallbackErr)
	}
	return errors.New(msg)
}

func (s *Session) failTeam(ctx context.Context, eventID string, eventAt time.Time, message string) error {
	return s.updateTeam(ctx, eventID, eventAt, func(ts *store.TeamState) {
		ts.Status = "failed"
		ts.EndedAt = s.eventTime(eventAt)
		ts.LastError = message
		ts.SessionID = s.id
		ts.AgentID = s.agent.ID
		ts.AgentVersion = s.agent.Version
		ts.DurationMs = durationMillis(ts.StartedAt, ts.EndedAt)
	})
}

func (s *Session) writeRawLog(raw []byte) error {
	if s.log == nil {
		return nil
	}
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if _, err := s.log.Write(raw); err != nil {
		return err
	}
	_, err := s.log.Write([]byte("\n"))
	return err
}

func (s *Session) setStream(stream eventStream) {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	s.stream = stream
}

func (s *Session) setErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *Session) now() time.Time {
	if s.clock == nil {
		return time.Now().UTC()
	}
	return s.clock().UTC()
}

func (s *Session) eventTime(t time.Time) time.Time {
	if t.IsZero() {
		return s.now()
	}
	return t.UTC()
}

func (s *Session) retryAttempts() int {
	if s == nil || s.apiMaxAttempts <= 0 {
		return defaultAPIMaxAttempts
	}
	return s.apiMaxAttempts
}

func (s *Session) withRetry(ctx context.Context, op string, fn func(context.Context) error) error {
	return retryMA(ctx, s.logger, op, s.retryAttempts(), s.retryBaseDelay(), s.retryMaxDelay(), fn)
}

func (s *Session) waitRetry(ctx context.Context, err error, attempt int) error {
	delay := retryDelay(err, attempt, s.retryBaseDelay(), s.retryMaxDelay())
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) retryBaseDelay() time.Duration {
	if s == nil || s.apiRetryBase <= 0 {
		return defaultAPIRetryBase
	}
	return s.apiRetryBase
}

func (s *Session) retryMaxDelay() time.Duration {
	if s == nil || s.apiRetryMax <= 0 {
		return defaultAPIRetryMax
	}
	return s.apiRetryMax
}

// StartSession wraps Beta.Sessions.New. No prompt is sent here.
//
//nolint:gocritic // signature matches Spawner interface
func (s *ManagedAgentsSpawner) StartSession(ctx context.Context, req StartSessionRequest) (*PendingSession, error) {
	if req.Agent.ID == "" {
		return nil, fmt.Errorf("%w: missing agent id", store.ErrInvalidArgument)
	}
	if req.Env.ID == "" {
		return nil, fmt.Errorf("%w: missing environment id", store.ErrInvalidArgument)
	}
	var created *anthropic.BetaManagedAgentsSession
	params := toSessionNewParams(&req)
	err := s.withRetry(ctx, "start_session", func(ctx context.Context) error {
		var err error
		created, err = s.sessions.New(ctx, params)
		return err
	})
	if err != nil {
		return nil, err
	}
	seenLimit := s.cfg.SessionEventSeenLimit
	if seenLimit <= 0 {
		seenLimit = defaultSessionSeenLimit
	}
	return &PendingSession{
		id:             created.ID,
		agent:          req.Agent,
		env:            req.Env,
		sessions:       s.sessions,
		events:         s.sessionEvents,
		teamName:       req.TeamName,
		log:            req.LogWriter,
		store:          req.Store,
		summaryWriter:  req.SummaryWriter,
		clock:          s.clock,
		logger:         s.logger,
		seenLimit:      seenLimit,
		apiMaxAttempts: s.cfg.APIMaxAttempts,
		apiRetryBase:   s.cfg.APIRetryBaseDelay,
		apiRetryMax:    s.cfg.APIRetryMaxDelay,
	}, nil
}

// ResumeSession is implemented in P1.8.
func (s *ManagedAgentsSpawner) ResumeSession(context.Context, string) (*Session, error) {
	return nil, ErrUnsupported
}

func (s *ManagedAgentsSpawner) withRetry(ctx context.Context, op string, fn func(context.Context) error) error {
	return retryMA(ctx, s.logger, op, s.cfg.APIMaxAttempts, s.cfg.APIRetryBaseDelay, s.cfg.APIRetryMaxDelay, fn)
}

func retryMA(
	ctx context.Context,
	logger *slog.Logger,
	op string,
	maxAttempts int,
	baseDelay time.Duration,
	maxDelay time.Duration,
	fn func(context.Context) error,
) error {
	if maxAttempts <= 0 {
		maxAttempts = defaultAPIMaxAttempts
	}
	if baseDelay < 0 {
		baseDelay = 0
	}
	if baseDelay == 0 {
		baseDelay = defaultAPIRetryBase
	}
	if maxDelay <= 0 {
		maxDelay = defaultAPIRetryMax
	}
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn(ctx)
		if err == nil {
			return nil
		}
		last = err
		if !isRetryableMAError(err) || attempt == maxAttempts {
			return err
		}
		delay := retryDelay(err, attempt, baseDelay, maxDelay)
		if logger != nil {
			logger.Debug("managed-agents api retry", "op", op, "attempt", attempt, "wait", delay, "error", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return last
}

func isRetryableMAError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusTooManyRequests {
			return true
		}
		return apiErr.StatusCode >= 500
	}
	return true
}

func retryDelay(err error, attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	if d, ok := retryAfterDelay(err); ok {
		return d
	}
	pow := math.Pow(2, float64(max(0, attempt-1)))
	delay := time.Duration(float64(baseDelay) * pow)
	if delay > maxDelay {
		delay = maxDelay
	}
	if delay <= 0 {
		return 0
	}
	jitter := 1.0 + (rand.Float64()-0.5)*0.5
	return time.Duration(float64(delay) * jitter)
}

func retryAfterDelay(err error) (time.Duration, bool) {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) || apiErr.Response == nil {
		return 0, false
	}
	h := apiErr.Response.Header.Get("Retry-After")
	if h == "" {
		return 0, false
	}
	if secs, parseErr := strconv.Atoi(h); parseErr == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, parseErr := http.ParseTime(h); parseErr == nil {
		return time.Until(t), true
	}
	return 0, false
}

func toSessionNewParams(req *StartSessionRequest) anthropic.BetaSessionNewParams {
	params := anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfBetaManagedAgentsAgents: &anthropic.BetaManagedAgentsAgentParams{
				ID:      req.Agent.ID,
				Type:    anthropic.BetaManagedAgentsAgentParamsTypeAgent,
				Version: anthropic.Int(int64(req.Agent.Version)),
			},
		},
		EnvironmentID: req.Env.ID,
		VaultIDs:      append([]string(nil), req.VaultIDs...),
		Resources:     toSessionResources(req.Resources),
		Metadata:      cloneStringMap(req.Metadata),
	}
	if req.TeamName != "" {
		params.Title = param.NewOpt(req.TeamName)
	}
	return params
}

func toSessionResources(resources []ResourceRef) []anthropic.BetaSessionNewParamsResourceUnion {
	if len(resources) == 0 {
		return nil
	}
	out := make([]anthropic.BetaSessionNewParamsResourceUnion, 0, len(resources))
	for i := range resources {
		ref := &resources[i]
		switch ref.Type {
		case "file":
			file := &anthropic.BetaManagedAgentsFileResourceParams{
				FileID: ref.FileID,
				Type:   anthropic.BetaManagedAgentsFileResourceParamsTypeFile,
			}
			if ref.MountPath != "" {
				file.MountPath = param.NewOpt(ref.MountPath)
			}
			out = append(out, anthropic.BetaSessionNewParamsResourceUnion{OfFile: file})
		case "github_repository":
			repo := &anthropic.BetaManagedAgentsGitHubRepositoryResourceParams{
				AuthorizationToken: ref.AuthorizationToken,
				Type:               anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsTypeGitHubRepository,
				URL:                ref.URL,
				Checkout:           toSessionRepoCheckout(ref.Checkout),
			}
			if ref.MountPath != "" {
				repo.MountPath = param.NewOpt(ref.MountPath)
			}
			out = append(out, anthropic.BetaSessionNewParamsResourceUnion{OfGitHubRepository: repo})
		}
	}
	return out
}

func toSessionRepoCheckout(checkout *RepoCheckout) anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsCheckoutUnion {
	if checkout == nil {
		return anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsCheckoutUnion{}
	}
	switch checkout.Type {
	case "branch":
		return anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsCheckoutUnion{
			OfBranch: &anthropic.BetaManagedAgentsBranchCheckoutParam{
				Name: checkout.Name,
				Type: anthropic.BetaManagedAgentsBranchCheckoutTypeBranch,
			},
		}
	case "commit":
		return anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsCheckoutUnion{
			OfCommit: &anthropic.BetaManagedAgentsCommitCheckoutParam{
				Sha:  checkout.SHA,
				Type: anthropic.BetaManagedAgentsCommitCheckoutTypeCommit,
			},
		}
	default:
		return anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsCheckoutUnion{}
	}
}

func toSessionEventSendParams(event *UserEvent) (anthropic.BetaSessionEventSendParams, error) {
	switch event.Type {
	case UserEventTypeMessage:
		if event.Message == "" {
			return anthropic.BetaSessionEventSendParams{}, fmt.Errorf("%w: empty user message", store.ErrInvalidArgument)
		}
		content := []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
			OfText: &anthropic.BetaManagedAgentsTextBlockParam{
				Text: event.Message,
				Type: anthropic.BetaManagedAgentsTextBlockTypeText,
			},
		}}
		return anthropic.BetaSessionEventSendParams{
			Events: []anthropic.BetaManagedAgentsEventParamsUnion{
				anthropic.BetaManagedAgentsEventParamsOfUserMessage(content),
			},
		}, nil
	case UserEventTypeInterrupt:
		return anthropic.BetaSessionEventSendParams{
			Events: []anthropic.BetaManagedAgentsEventParamsUnion{
				anthropic.BetaManagedAgentsEventParamsOfUserInterrupt(anthropic.BetaManagedAgentsUserInterruptEventParamsTypeUserInterrupt),
			},
		}, nil
	default:
		return anthropic.BetaSessionEventSendParams{}, fmt.Errorf("%w: managed-agents send does not support %q", ErrUnsupported, event.Type)
	}
}

func sessionStatusFromMA(session *anthropic.BetaManagedAgentsSession) SessionStatus {
	if session == nil {
		return SessionStatus{}
	}
	return SessionStatus{
		ID:        session.ID,
		State:     SessionState(session.Status),
		UpdatedAt: session.UpdatedAt,
	}
}

func usageFromMASession(usage *anthropic.BetaManagedAgentsSessionUsage) Usage {
	return Usage{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreation.Ephemeral1hInputTokens + usage.CacheCreation.Ephemeral5mInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}
}

type maEvent struct {
	ID                  string          `json:"id"`
	Type                string          `json:"type"`
	ProcessedAt         time.Time       `json:"processed_at"`
	Content             []maContent     `json:"content"`
	Name                string          `json:"name"`
	Input               json.RawMessage `json:"input"`
	ToolUseID           string          `json:"tool_use_id"`
	MCPServerName       string          `json:"mcp_server_name"`
	MCPToolUseID        string          `json:"mcp_tool_use_id"`
	CustomToolUseID     string          `json:"custom_tool_use_id"`
	IsError             bool            `json:"is_error"`
	Error               json.RawMessage `json:"error"`
	StopReason          maStopReason    `json:"stop_reason"`
	ModelRequestStartID string          `json:"model_request_start_id"`
	ModelUsage          maModelUsage    `json:"model_usage"`
}

type maContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

type maStopReason struct {
	Type     string   `json:"type"`
	EventIDs []string `json:"event_ids"`
	Raw      string   `json:"-"`
}

func (r *maStopReason) UnmarshalJSON(data []byte) error {
	type alias maStopReason
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*r = maStopReason(a)
	r.Raw = string(data)
	return nil
}

type maModelUsage struct {
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	CostUSD                  float64 `json:"cost_usd"`
}

func translateMAEvent(raw []byte, now time.Time) (Event, maEvent, error) {
	var ev maEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return UnknownEvent{}, maEvent{}, err
	}
	if ev.ProcessedAt.IsZero() {
		ev.ProcessedAt = now.UTC()
	}
	base := BaseEvent{
		ID:          EventID(ev.ID),
		Type:        EventType(ev.Type),
		ProcessedAt: ev.ProcessedAt,
	}
	if translated, ok := translateAgentEvent(base, &ev); ok {
		return translated, ev, nil
	}
	if translated, ok := translateSessionEvent(base, &ev); ok {
		return translated, ev, nil
	}
	base.Type = EventTypeUnknown
	return UnknownEvent{BaseEvent: base, Payload: json.RawMessage(raw)}, ev, nil
}

func translateAgentEvent(base BaseEvent, ev *maEvent) (Event, bool) {
	switch EventType(ev.Type) {
	case EventTypeAgentMessage:
		text := contentText(ev.Content)
		return AgentMessageEvent{BaseEvent: base, Role: "assistant", Content: toContentBlocks(ev.Content), Text: text}, true
	case EventTypeAgentThinking:
		return AgentThinkingEvent{BaseEvent: base}, true
	case EventTypeAgentToolUse:
		return AgentToolUseEvent{BaseEvent: base, ToolUse: ToolUse{ID: ev.ID, Name: ev.Name, Input: rawInput(ev.Input)}}, true
	case EventTypeAgentToolResult:
		return AgentToolResultEvent{BaseEvent: base, ToolResult: ToolResult{ToolUseID: ev.ToolUseID, Content: contentTextOrRaw(ev.Content), Error: toolError(ev.IsError)}}, true
	case EventTypeAgentMCPToolUse:
		return AgentMCPToolUseEvent{BaseEvent: base, ServerName: ev.MCPServerName, ToolUse: ToolUse{ID: ev.ID, Name: ev.Name, Input: rawInput(ev.Input)}}, true
	case EventTypeAgentMCPToolResult:
		return AgentMCPToolResultEvent{BaseEvent: base, ServerName: ev.MCPServerName, ToolResult: ToolResult{ToolUseID: firstNonEmpty(ev.MCPToolUseID, ev.ToolUseID), Content: contentTextOrRaw(ev.Content), Error: toolError(ev.IsError)}}, true
	case EventTypeAgentCustomToolUse:
		return AgentCustomToolUseEvent{BaseEvent: base, ToolUse: ToolUse{ID: ev.ID, Name: ev.Name, Input: rawInput(ev.Input)}}, true
	case EventTypeAgentThreadContextCompacted:
		return AgentThreadContextCompactedEvent{BaseEvent: base, Summary: contentText(ev.Content)}, true
	default:
		return nil, false
	}
}

func translateSessionEvent(base BaseEvent, ev *maEvent) (Event, bool) {
	switch EventType(ev.Type) {
	case EventTypeSessionStatusRunning:
		return SessionStatusRunningEvent{BaseEvent: base, Status: SessionStatus{State: SessionStateRunning, UpdatedAt: ev.ProcessedAt}}, true
	case EventTypeSessionStatusIdle:
		return SessionStatusIdleEvent{BaseEvent: base, Status: SessionStatus{State: SessionStateIdle, StopReason: StopReason{Type: ev.StopReason.Type, EventIDs: eventIDs(ev.StopReason.EventIDs)}, UpdatedAt: ev.ProcessedAt, Message: ev.ErrorMessage()}}, true
	case EventTypeSessionStatusRescheduled:
		return SessionStatusRescheduledEvent{BaseEvent: base, Status: SessionStatus{State: SessionStateRescheduling, UpdatedAt: ev.ProcessedAt}}, true
	case EventTypeSessionStatusTerminated:
		return SessionStatusTerminatedEvent{BaseEvent: base, Status: SessionStatus{State: SessionStateTerminated, UpdatedAt: ev.ProcessedAt}}, true
	case EventTypeSessionError:
		return SessionErrorEvent{BaseEvent: base, Message: ev.ErrorMessage(), Code: ev.ErrorCode()}, true
	case EventTypeSpanModelRequestStart:
		return SpanModelRequestStartEvent{BaseEvent: base}, true
	case EventTypeSpanModelRequestEnd:
		return SpanModelRequestEndEvent{BaseEvent: base, Usage: Usage{
			InputTokens:              ev.ModelUsage.InputTokens,
			OutputTokens:             ev.ModelUsage.OutputTokens,
			CacheCreationInputTokens: ev.ModelUsage.CacheCreationInputTokens,
			CacheReadInputTokens:     ev.ModelUsage.CacheReadInputTokens,
			CostUSD:                  ev.ModelUsage.CostUSD,
		}}, true
	default:
		return nil, false
	}
}

func (e *maEvent) ErrorMessage() string {
	if len(e.Error) == 0 {
		return ""
	}
	var shaped struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(e.Error, &shaped); err != nil {
		return string(e.Error)
	}
	return firstNonEmpty(shaped.Message, shaped.Code, shaped.Type, string(e.Error))
}

func (e *maEvent) ErrorCode() string {
	if len(e.Error) == 0 {
		return ""
	}
	var shaped struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(e.Error, &shaped); err != nil {
		return ""
	}
	return firstNonEmpty(shaped.Code, shaped.Type)
}

func contentText(content []maContent) string {
	var parts []string
	for i := range content {
		if content[i].Text != "" {
			parts = append(parts, content[i].Text)
		}
	}
	return strings.Join(parts, "\n")
}

func contentTextOrRaw(content []maContent) any {
	if text := contentText(content); text != "" {
		return text
	}
	return content
}

func toContentBlocks(content []maContent) []ContentBlock {
	out := make([]ContentBlock, 0, len(content))
	for i := range content {
		out = append(out, ContentBlock{
			Type:      content[i].Type,
			Text:      content[i].Text,
			Name:      content[i].Name,
			ID:        content[i].ID,
			Input:     rawInput(content[i].Input),
			ToolUseID: content[i].ToolUseID,
		})
	}
	return out
}

func rawInput(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

func toolError(isError bool) string {
	if isError {
		return "tool result reported error"
	}
	return ""
}

func eventIDs(ids []string) []EventID {
	out := make([]EventID, 0, len(ids))
	for _, id := range ids {
		out = append(out, EventID(id))
	}
	return out
}

func durationMillis(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

// seenSet deduplicates event IDs across the primary stream and reconnect
// backfill. The backfill replays events from the beginning (the pinned Go SDK
// does not expose an `after` cursor on the event list endpoint), so the set
// must retain every ID observed during the session — an LRU eviction would let
// early events get re-applied on a late reconnect, double-counting usage.
type seenSet struct {
	set map[string]struct{}
}

func newSeenSet(hint int) *seenSet {
	if hint <= 0 {
		hint = defaultSessionSeenLimit
	}
	return &seenSet{set: make(map[string]struct{}, hint)}
}

func (s *seenSet) Has(id string) bool {
	_, ok := s.set[id]
	return ok
}

func (s *seenSet) Push(id string) {
	if id == "" {
		return
	}
	s.set[id] = struct{}{}
}
