package customtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/itsHabib/orchestra/internal/notify"
	"github.com/itsHabib/orchestra/internal/store"
)

// SignalCompletionTool is the host-side name of the sentinel tool. Exposed as
// a constant so config validation, engine wiring, and tests refer to the same
// string.
const SignalCompletionTool = "signal_completion"

// signalDone is the value of input.status that means the team finished
// successfully (PR open, reviews requested, CI green, comments addressed).
const signalDone = "done"

// signalBlocked is the value of input.status that means the team hit a hard
// block (genuine ambiguity, unresolvable review conflict, CI failure outside
// scope) and is waiting for human steering via orchestra msg.
const signalBlocked = "blocked"

// SignalCompletionHandler implements the sentinel tool from §7 of
// DESIGN-ship-feature-workflow. It is stateless — every Handle call gets a
// fresh RunContext with the live store + notifier — so a single instance can
// be registered once at engine startup and shared across all teams.
type SignalCompletionHandler struct{}

// NewSignalCompletion returns a ready-to-register SignalCompletionHandler.
func NewSignalCompletion() SignalCompletionHandler {
	return SignalCompletionHandler{}
}

// Tool returns the tool definition. The schema mirrors §7.1 of the design
// doc: required `status` enum + `summary`, optional `pr_url` (used when
// status=done) and `reason` (required when status=blocked, enforced by
// Handle rather than the schema since JSON Schema can't express "required
// when sibling=X" without anyOf gymnastics).
func (SignalCompletionHandler) Tool() Definition {
	return Definition{
		Name:        SignalCompletionTool,
		Description: "Called once per session when the team's work is fully done OR genuinely blocked. After calling this, stop emitting actions.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{
					"type":        "string",
					"enum":        []string{signalDone, signalBlocked},
					"description": "done when the PR is merge-ready; blocked when human input is required to proceed.",
				},
				"summary": map[string]any{
					"type":        "string",
					"description": "One-line summary of what was shipped (status=done) or why it's blocked (status=blocked).",
				},
				"pr_url": map[string]any{
					"type":        "string",
					"description": "Required when status=done. The merge-ready PR URL.",
				},
				"reason": map[string]any{
					"type":        "string",
					"description": "Required when status=blocked. A sentence the human can act on.",
				},
			},
			"required": []string{"status", "summary"},
		},
	}
}

// signalCompletionInput is the host-side decoded shape of a tool call.
type signalCompletionInput struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	PRURL   string `json:"pr_url,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// signalCompletionResult is the JSON we hand back to the agent. duplicate is
// true when the team had already signaled — the agent is expected to stop
// either way, so the field is informational.
type signalCompletionResult struct {
	OK        bool   `json:"ok"`
	Duplicate bool   `json:"duplicate"`
	Status    string `json:"status,omitempty"`
}

// Handle records the signal in the run state and fires a notification.
// Idempotent on a second call: if the team already has a SignalStatus set,
// the second call is a no-op and returns ok=true,duplicate=true (per §14
// Q10). A confused agent calling twice does not erase the original outcome
// or re-fire the notification.
func (SignalCompletionHandler) Handle(ctx context.Context, run *RunContext, team string, raw json.RawMessage) (json.RawMessage, error) {
	if run == nil {
		return nil, errors.New("signal_completion: nil run context")
	}
	if team == "" {
		return nil, errors.New("signal_completion: empty team")
	}
	in, err := parseSignalCompletionInput(raw)
	if err != nil {
		return nil, err
	}

	if run.Store == nil {
		return nil, errors.New("signal_completion: nil store")
	}
	now := run.Time()
	duplicate, err := writeSignalState(ctx, run.Store, team, &in, now)
	if err != nil {
		return nil, err
	}

	if !duplicate && run.Notifier != nil {
		if notifyErr := run.Notifier.Notify(ctx, &notify.Notification{
			Timestamp: now,
			RunID:     run.RunID,
			Team:      team,
			Status:    in.Status,
			Summary:   in.Summary,
			PRURL:     in.PRURL,
			Reason:    in.Reason,
		}); notifyErr != nil {
			// fanOut.Notify is documented to swallow per-sink failures — a
			// non-nil error here means the composite itself refused (rare:
			// e.g. a custom Notifier not built via Compose). Surface as an
			// is_error tool result so the agent learns the host could not
			// deliver the notification, but keep the state write — the
			// signal is recorded regardless.
			return nil, fmt.Errorf("signal_completion: notify: %w", notifyErr)
		}
	}

	out, err := json.Marshal(&signalCompletionResult{
		OK:        true,
		Duplicate: duplicate,
		Status:    in.Status,
	})
	if err != nil {
		return nil, fmt.Errorf("signal_completion: marshal result: %w", err)
	}
	return out, nil
}

func parseSignalCompletionInput(raw json.RawMessage) (signalCompletionInput, error) {
	var in signalCompletionInput
	if len(raw) == 0 {
		return in, errors.New("signal_completion: empty input")
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, fmt.Errorf("signal_completion: parse input: %w", err)
	}
	switch in.Status {
	case signalDone, signalBlocked:
	default:
		return in, fmt.Errorf("signal_completion: status must be %q or %q, got %q",
			signalDone, signalBlocked, in.Status)
	}
	if in.Summary == "" {
		return in, errors.New("signal_completion: summary is required")
	}
	if in.Status == signalBlocked && in.Reason == "" {
		return in, errors.New("signal_completion: reason is required when status=blocked")
	}
	return in, nil
}

// writeSignalState applies the input to the team state under the existing
// UpdateTeamState funnel. Returns duplicate=true when the team had already
// signaled; in that case no fields are mutated and the caller skips
// notification.
func writeSignalState(ctx context.Context, st store.Store, team string, in *signalCompletionInput, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var duplicate bool
	err := st.UpdateTeamState(ctx, team, func(ts *store.TeamState) {
		if ts.SignalStatus != "" {
			duplicate = true
			return
		}
		ts.SignalStatus = in.Status
		ts.SignalSummary = in.Summary
		ts.SignalPRURL = in.PRURL
		ts.SignalReason = in.Reason
		ts.SignalAt = now
	})
	if err != nil {
		return false, fmt.Errorf("signal_completion: update state: %w", err)
	}
	return duplicate, nil
}
