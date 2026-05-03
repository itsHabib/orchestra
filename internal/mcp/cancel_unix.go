//go:build !windows

package mcp

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// signalCancel delivers SIGTERM to the orchestra subprocess so the
// engine's signal handler can flip running agents to "canceled" before
// exiting. SIGTERM (not SIGINT) matches what `kill <pid>` and most
// process managers send for graceful shutdown.
func signalCancel(pid int) error {
	if pid <= 0 {
		return errors.New("cancel: invalid pid")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// waitForExit polls until the orchestra subprocess exits or the timeout
// elapses. POSIX [os.FindProcess] always succeeds, so reachability is
// checked via signal 0 — the canonical "is this pid alive" probe.
//
// Distinguishes "process gone" (ESRCH) from "alive but no perm to
// signal it" (EPERM): both make Signal(0) return non-nil, but only
// ESRCH means the drain is complete. Treating EPERM as "exited" used
// to short-circuit the wait when the orchestra subprocess ran under a
// different uid (rare but possible under setuid bits or sudo wrappers).
func waitForExit(pid int, timeout time.Duration) {
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		err = proc.Signal(syscall.Signal(0))
		switch {
		case err == nil:
			// Still alive — wait and probe again.
		case errors.Is(err, syscall.ESRCH), errors.Is(err, os.ErrProcessDone):
			// Reaped: ESRCH ("no such process") or the Go-runtime
			// stand-in returned after Wait already collected the exit.
			return
		case errors.Is(err, syscall.EPERM):
			// Alive, just out of reach. Keep waiting until the deadline
			// — eventually the drain budget elapses or the process
			// genuinely exits.
		default:
			// Any other error: treat as gone rather than spin
			// indefinitely. This matches the "best-effort drain"
			// contract documented on cancelDrainTimeout.
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
