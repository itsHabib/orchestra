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
// checked via signal 0, the canonical "is this pid alive" probe.
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
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
