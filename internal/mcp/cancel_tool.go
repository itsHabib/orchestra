package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/fsutil"
	"github.com/itsHabib/orchestra/internal/store"
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

// cancellationFileName is the workspace-local file the MCP server
// writes when handling cancel_run. The engine reads it on signal
// receipt (in pkg/orchestra) and merges the reason into the run's
// Cancellation field under its own exclusive run lock — the dedicated
// file avoids the cross-process race that came with writing to
// state.json directly.
const cancellationFileName = "cancellation.json"

// CancelRunArgs is the cancel_run MCP tool input.
type CancelRunArgs struct {
	RunID  string `json:"run_id" jsonschema:"run id from list_runs / get_run / run"`
	Reason string `json:"reason,omitempty" jsonschema:"optional human-readable reason recorded with the cancellation"`
}

// CancelRunResult is the cancel_run MCP tool output. The handler returns
// CancelledAt unset for a no-op (run already terminal) so callers can
// distinguish "actually canceled this run" from "already done."
//
// SignalError is non-empty when the tool wrote the cancellation file
// but the OS-level signal (SIGTERM / CTRL_BREAK_EVENT) did not deliver
// — usually a stale PID or a permission issue. Returning the run as
// "canceled" without surfacing the failure would lie to the caller; the
// engine may still be running.
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

	// Record the cancel request to a dedicated file BEFORE signaling so
	// the engine's signal handler can read the reason after SIGTERM
	// lands. Using a separate file (cancellation.json) instead of
	// touching state.json avoids the cross-process read-modify-write
	// race the round-2 review caught — the engine concurrently writes
	// state.json and an MCP-side RMW could clobber its in-flight saves.
	if err := writeCancellationFile(entry.WorkspaceDir, args.Reason, time.Now().UTC()); err != nil {
		return errResult("write cancellation file: %v", err), CancelRunResult{}, nil
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
			// already exited (the cancellation file still lands, so a
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

// runIsTerminal reports whether every agent in the snapshot has
// reached a state that won't transition further on its own. Mirrors
// [deriveStatus]'s [isTerminalStatus] helper so cancel_run's idempotent
// short-circuit matches what list_runs / get_run report — without this
// alignment, cancel_run would still signal a run that observers
// already see as "done" because every agent has SignalStatus="done"
// while the engine hasn't flipped the per-agent Status to "done" yet.
func runIsTerminal(state *store.RunState) bool {
	if state == nil || len(state.Agents) == 0 {
		return false
	}
	for name := range state.Agents {
		ts := state.Agents[name]
		if !isTerminalStatus(ts.Status, ts.SignalStatus) {
			return false
		}
	}
	return true
}

// CancellationFile is the on-disk shape of the cancel-request payload
// the MCP server writes and the engine reads on signal receipt.
type CancellationFile struct {
	RequestedAt time.Time `json:"requested_at"`
	Reason      string    `json:"reason,omitempty"`
}

// writeCancellationFile drops the cancel request into a dedicated
// file under <workspace>/.orchestra/cancellation.json. Atomic write via
// fsutil so a concurrent reader either sees the prior contents or the
// new contents (never a partial). The engine reads this file in the
// cancellation hook and merges the reason into RunState.Cancellation
// under its existing exclusive run lock.
//
// Best-effort behavior when the workspace directory is missing
// (engine hasn't seeded yet): create the dir, write the file. The
// SIGTERM still arrives; the engine reads cancellation.json on next
// state-write attempt.
func writeCancellationFile(workspaceDir, reason string, requestedAt time.Time) error {
	dir := stateDir(workspaceDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("prepare workspace: %w", err)
	}
	payload := CancellationFile{
		RequestedAt: requestedAt,
		Reason:      reason,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cancellation: %w", err)
	}
	return fsutil.AtomicWrite(filepath.Join(dir, cancellationFileName), data)
}

// readCancellationFile loads the cancel request from
// <workspace>/.orchestra/cancellation.json. Returns (nil, nil) when the
// file is absent — that's the normal state for a healthy run, not an
// error. The engine consults this in the ctx-cancel hook so a
// deliberate cancel reads back the reason the operator supplied.
func readCancellationFile(workspaceDir string) (*CancellationFile, error) {
	path := filepath.Join(stateDir(workspaceDir), cancellationFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cancellation: %w", err)
	}
	var cf CancellationFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse cancellation: %w", err)
	}
	return &cf, nil
}
