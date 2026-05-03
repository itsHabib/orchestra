package spawner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/itsHabib/orchestra/internal/machost"
	"github.com/itsHabib/orchestra/internal/store"
)

// Steering sentinel errors. Stable across releases — out-of-process callers
// (CLI commands, future SDK consumers) can errors.Is against these. They are
// not returned by SendUserMessage / SendUserInterrupt themselves; the CLI
// layer wraps a state-lookup failure with the matching sentinel before
// reaching the helper.
var (
	// ErrNoActiveRun reports that the workspace has no active orchestra run.
	// Surfaces as the user-facing "no active orchestra run in <workspace>".
	ErrNoActiveRun = errors.New("no active orchestra run")

	// ErrTeamNotFound reports that the named team is not in the active run's
	// state.json team map.
	ErrTeamNotFound = errors.New("team not found in active run")

	// ErrTeamNotRunning reports that the named team exists but its status is
	// not "running" (so it has no live MA session to steer).
	ErrTeamNotRunning = errors.New("team is not running")

	// ErrNoSessionRecorded reports that the team is running but state.json
	// records no SessionID for it (e.g. transient state during start-up).
	ErrNoSessionRecorded = errors.New("team has no recorded session id")

	// ErrLocalBackend reports that steering was requested for a workspace
	// running under backend: local. Local steering is tracked as P1.9-E.
	ErrLocalBackend = errors.New("steering is only supported under backend: managed_agents (local-backend steering tracked as P1.9-E)")
)

// TeamSession is the per-team steerability snapshot returned by
// ListTeamSessions. The CLI's `orchestra sessions ls` and the future
// pkg/orchestra.ActiveSessions both render it.
type TeamSession struct {
	Team        string
	Status      string
	Steerable   bool
	SessionID   string
	AgentID     string
	LastEventID string
	LastEventAt time.Time
}

// SteerableSessionID returns the MA session id for `team` after the same
// gating chain cmd/steering.go and internal/mcp/tools.go both need: backend
// must be managed_agents, the team must exist, it must be in status
// "running", and a SessionID must be recorded. Each gate maps to a distinct
// sentinel error for callers to errors.Is against. Pure function so the
// disk read is the caller's choice.
func SteerableSessionID(state *store.RunState, team string) (string, error) {
	if state == nil {
		return "", ErrNoActiveRun
	}
	if state.Backend != "" && state.Backend != "managed_agents" {
		return "", ErrLocalBackend
	}
	ts, ok := state.Agents[team]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrTeamNotFound, team)
	}
	if ts.Status != "running" {
		return "", fmt.Errorf("%w: %q is %q", ErrTeamNotRunning, team, ts.Status)
	}
	if ts.SessionID == "" {
		return "", fmt.Errorf("%w: %q", ErrNoSessionRecorded, team)
	}
	return ts.SessionID, nil
}

// ListTeamSessions builds the per-team steerability view from a loaded
// RunState. Pure function so callers (CLI, tests) can construct state
// in-memory and exercise the formatting / filter without touching the disk.
// Returns rows sorted by team name for stable output.
func ListTeamSessions(state *store.RunState) []TeamSession {
	if state == nil {
		return nil
	}
	out := make([]TeamSession, 0, len(state.Agents))
	for name := range state.Agents {
		ts := state.Agents[name]
		out = append(out, TeamSession{
			Team:        name,
			Status:      ts.Status,
			Steerable:   ts.Status == "running" && ts.SessionID != "",
			SessionID:   ts.SessionID,
			AgentID:     ts.AgentID,
			LastEventID: ts.LastEventID,
			LastEventAt: ts.LastEventAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Team < out[j].Team })
	return out
}

// SessionEventSender is the narrow interface steering needs: a single Send
// method matching the SDK's BetaSessionEventService.Send signature. The SDK
// adapter (sdkSessionEventsAPI), the broader spawner-internal
// managedSessionEventsAPI, and any test fake all satisfy it. Exported so
// out-of-process callers (the new CLI commands) can hold values returned
// by SessionEventsClient and pass them into SendUserMessage /
// SendUserInterrupt without depending on the full streaming interface.
type SessionEventSender interface {
	Send(context.Context, string, anthropic.BetaSessionEventSendParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSendSessionEvents, error)
}

// SessionEventsClient constructs a SessionEventSender authenticated via the
// host API key (machost.NewClient). CLI commands that need to send steering
// events from a process that did not construct a Spawner use this.
func SessionEventsClient(_ context.Context) (SessionEventSender, error) {
	client, err := machost.NewClient()
	if err != nil {
		return nil, err
	}
	return sdkSessionEventsAPI{events: &client.Beta.Sessions.Events}, nil
}

// SendUserMessage delivers a user.message event to sessionID. retryAttempts
// counts retries on transient (429 / 5xx / network) errors; total HTTP calls
// at most retryAttempts+1. Pass 0 for at-most-once semantics.
//
// Duplicate delivery is possible when retryAttempts > 0 and the server
// returns 5xx after the event was actually accepted; the agent may then see
// the same message twice. Acceptable for messages but not for interrupts —
// see SendUserInterrupt.
func SendUserMessage(ctx context.Context, sessions SessionEventSender, sessionID, text string, retryAttempts int) error {
	if sessions == nil {
		return fmt.Errorf("%w: nil session events client", store.ErrInvalidArgument)
	}
	if sessionID == "" {
		return fmt.Errorf("%w: empty session id", store.ErrInvalidArgument)
	}
	params, err := toSessionEventSendParams(&UserEvent{Type: UserEventTypeMessage, Message: text})
	if err != nil {
		return err
	}
	return retrySteering(ctx, "steer_user_message", retryAttempts, func(ctx context.Context) error {
		_, err := sessions.Send(ctx, sessionID, params)
		return err
	})
}

// SendUserInterrupt delivers a user.interrupt event to sessionID. Same
// retryAttempts semantics as SendUserMessage. Default callers pass 0
// because a duplicate interrupt could double-cancel a recovery cycle.
func SendUserInterrupt(ctx context.Context, sessions SessionEventSender, sessionID string, retryAttempts int) error {
	if sessions == nil {
		return fmt.Errorf("%w: nil session events client", store.ErrInvalidArgument)
	}
	if sessionID == "" {
		return fmt.Errorf("%w: empty session id", store.ErrInvalidArgument)
	}
	params, err := toSessionEventSendParams(&UserEvent{Type: UserEventTypeInterrupt})
	if err != nil {
		return err
	}
	return retrySteering(ctx, "steer_user_interrupt", retryAttempts, func(ctx context.Context) error {
		_, err := sessions.Send(ctx, sessionID, params)
		return err
	})
}

// retrySteering wraps retryMA so steering callers can request 0-retry
// (at-most-once) semantics — retryMA's maxAttempts<=0 fallback would
// otherwise silently install the default 5-attempt budget.
func retrySteering(ctx context.Context, op string, retryAttempts int, fn func(context.Context) error) error {
	if retryAttempts < 0 {
		retryAttempts = 0
	}
	maxAttempts := retryAttempts + 1
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return retryMA(ctx, logger, op, maxAttempts, defaultAPIRetryBase, defaultAPIRetryMax, fn)
}

// IsSteeringSentinel reports whether err wraps any of the steering sentinel
// errors. Convenience for callers that want a single check before falling
// back to a generic "send failed" message.
func IsSteeringSentinel(err error) bool {
	for _, sentinel := range []error{ErrNoActiveRun, ErrTeamNotFound, ErrTeamNotRunning, ErrNoSessionRecorded, ErrLocalBackend} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// Compile-time assertion that the SDK adapter satisfies the steering
// sender interface. Catches drift if the SDK shape changes underneath us.
var _ SessionEventSender = sdkSessionEventsAPI{}
