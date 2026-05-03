package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
)

// ToolCancelRun is the MCP tool name for [Server.handleCancelRun].
const ToolCancelRun = "cancel_run"

// cancelDrainTimeout caps how long [Server.handleCancelRun] waits for
// the orchestra subprocess to exit after the signal lands. 10s matches
// the kickoff doc's "wait up to 10s for cleanup" requirement and is
// generous enough for the engine to flip every running agent to
// "canceled" and write state.json before the registry entry is
// finalized.
const cancelDrainTimeout = 10 * time.Second

// CancelRunArgs is the cancel_run MCP tool input.
type CancelRunArgs struct {
	RunID  string `json:"run_id" jsonschema:"run id from list_runs / get_run / run"`
	Reason string `json:"reason,omitempty" jsonschema:"optional human-readable reason recorded with the cancellation"`
}

// CancelRunResult is the cancel_run MCP tool output. The handler returns
// CancelledAt unset for a no-op (run already terminal) so callers can
// distinguish "actually canceled this run" from "already done."
//
// SignalError is non-empty when the tool wrote the cancellation flag to
// state.json but the OS-level signal (SIGTERM / CTRL_BREAK_EVENT) did
// not deliver — usually a stale PID or a permission issue. Returning
// the run as "canceled" without surfacing the failure would lie to the
// caller; the engine may still be running.
type CancelRunResult struct {
	RunID       string    `json:"run_id"`
	CancelledAt time.Time `json:"canceled_at,omitempty"`
	AlreadyDone bool      `json:"already_done,omitempty"`
	SignalError string    `json:"signal_error,omitempty"`
}

func (s *Server) handleCancelRun(ctx context.Context, _ *mcp.CallToolRequest, args CancelRunArgs) (*mcp.CallToolResult, CancelRunResult, error) {
	if args.RunID == "" {
		return errResult("run_id is required"), CancelRunResult{}, nil
	}
	entry, ok, err := s.registry.Get(ctx, args.RunID)
	if err != nil {
		return errResult("read registry: %v", err), CancelRunResult{}, nil
	}
	if !ok {
		return errResult("run %q not found", args.RunID), CancelRunResult{}, nil
	}

	// Idempotent: terminal runs return AlreadyDone=true with no signal.
	state, stateErr := s.stateReader(ctx, stateDir(entry.WorkspaceDir))
	if stateErr == nil && state != nil && runIsTerminal(state) {
		return textResult(fmt.Sprintf("run %s already terminal", entry.RunID)),
			CancelRunResult{RunID: entry.RunID, AlreadyDone: true}, nil
	}

	// Record cancellation in state.json BEFORE signaling so the engine's
	// signal handler can read the reason after the SIGTERM lands. Cheap
	// to write twice (the engine no-ops when state.json is missing).
	if err := writeCancellationFlag(ctx, entry.WorkspaceDir, args.Reason, time.Now().UTC()); err != nil {
		return errResult("write cancellation flag: %v", err), CancelRunResult{}, nil
	}

	out := CancelRunResult{
		RunID:       entry.RunID,
		CancelledAt: time.Now().UTC(),
	}
	if entry.PID > 0 {
		err := signalCancel(entry.PID)
		switch {
		case err == nil, errors.Is(err, os.ErrProcessDone):
			// Either the signal landed cleanly or the subprocess had
			// already exited (the cancellation flag still lands, so a
			// future read still sees the deliberate-cancel signal).
		default:
			// A real signaling failure — stale PID, EPERM, etc. Surface
			// it on the result so callers know the engine may still be
			// running. The textResult below also reports the failure
			// for clients that ignore the structured payload.
			out.SignalError = err.Error()
		}
	}
	waitForExit(entry.PID, cancelDrainTimeout)
	msg := fmt.Sprintf("run %s canceled", entry.RunID)
	if out.SignalError != "" {
		msg = fmt.Sprintf("run %s canceled (signal failed: %s)", entry.RunID, out.SignalError)
	}
	return textResult(msg), out, nil
}

// runIsTerminal returns true when every agent in the snapshot has
// reached a terminal state (done, failed, canceled). Used to short-
// circuit cancel_run on already-finished runs.
func runIsTerminal(state *store.RunState) bool {
	if state == nil || len(state.Agents) == 0 {
		return false
	}
	for name := range state.Agents {
		ts := state.Agents[name]
		switch ts.Status {
		case "done", "failed", "canceled":
		default:
			return false
		}
	}
	return true
}

// writeCancellationFlag records the cancel_run request directly on
// state.json. Uses the filestore to share the atomic-write convention
// the engine uses for every other state mutation; falls back to a
// best-effort no-op when state.json doesn't exist yet (the engine has
// only just spawned and not seeded its state — the SIGTERM still lands).
func writeCancellationFlag(ctx context.Context, workspaceDir, reason string, requestedAt time.Time) error {
	fs := filestore.New(stateDir(workspaceDir))
	state, err := fs.LoadRunState(ctx)
	if errors.Is(err, store.ErrNotFound) {
		// State.json missing → engine hasn't seeded yet. The signal will
		// still arrive; the engine logs "no state to cancel" and exits.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state.json: %w", err)
	}
	state.Cancellation = &store.Cancellation{
		RequestedAt: requestedAt,
		Reason:      reason,
	}
	return fs.SaveRunState(ctx, state)
}
