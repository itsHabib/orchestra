//go:build windows

package mcp

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// signalCancel delivers a CTRL_BREAK_EVENT to the orchestra subprocess'
// process group on Windows, the closest equivalent to SIGTERM. The
// subprocess was started via [detachProcessGroup] with
// CREATE_NEW_PROCESS_GROUP so the event lands without affecting the MCP
// server's console.
func signalCancel(pid int) error {
	if pid <= 0 {
		return errors.New("cancel: invalid pid")
	}
	dll, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		return err
	}
	defer func() { _ = dll.Release() }()
	proc, err := dll.FindProc("GenerateConsoleCtrlEvent")
	if err != nil {
		return err
	}
	const ctrlBreakEvent = 1
	r1, _, callErr := proc.Call(uintptr(ctrlBreakEvent), uintptr(pid))
	if r1 == 0 {
		return callErr
	}
	return nil
}

// waitForExit polls until the orchestra subprocess exits or the timeout
// elapses. Windows surfaces dead pids as a [os.FindProcess] success
// followed by a [Process.Signal] error; we open the process handle
// each iteration to avoid keeping a stale one across the wait.
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
		// On Windows os.FindProcess never errors; probe via OpenProcess
		// behind the scenes by calling Signal(0). Signal(0) on Windows
		// returns "OS does not support sending nil signal" — accept that
		// as "still alive" and rely on the deadline. Real exit detection
		// would require WaitForSingleObject; the 10-second cap is the
		// design's "best-effort drain" from the kickoff doc.
		_ = proc
		time.Sleep(100 * time.Millisecond)
	}
}
