//go:build windows

package mcp

import (
	"errors"
	"syscall"
	"time"
)

// signalCancel delivers a CTRL_BREAK_EVENT to the orchestra subprocess's
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
// elapses. Uses OpenProcess + WaitForSingleObject so the wait actually
// returns the moment the subprocess exits rather than sleeping the
// whole drain window. Falls back to a short polled sleep when
// OpenProcess fails so a stale handle path can't deadlock the cancel
// tool.
func waitForExit(pid int, timeout time.Duration) {
	if pid <= 0 {
		return
	}
	const (
		synchronize         = 0x00100000
		processQueryLimited = 0x00001000
		waitObject0         = 0x00000000
		// WAIT_TIMEOUT and WAIT_FAILED are the constants Windows
		// returns from WaitForSingleObject; named here so the branch
		// below reads top-down without magic numbers.
		waitTimeout = 0x00000102
		waitFailed  = 0xFFFFFFFF
	)

	dll, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		fallbackSleep(timeout)
		return
	}
	defer func() { _ = dll.Release() }()
	openProcess, err := dll.FindProc("OpenProcess")
	if err != nil {
		fallbackSleep(timeout)
		return
	}
	waitForSingleObject, err := dll.FindProc("WaitForSingleObject")
	if err != nil {
		fallbackSleep(timeout)
		return
	}
	closeHandle, err := dll.FindProc("CloseHandle")
	if err != nil {
		fallbackSleep(timeout)
		return
	}

	handle, _, _ := openProcess.Call(uintptr(synchronize|processQueryLimited), 0, uintptr(pid))
	if handle == 0 {
		// Common when the process has already exited or never existed.
		// Either way, nothing to wait on — return immediately so the
		// caller doesn't burn the drain budget.
		return
	}
	defer func() { _, _, _ = closeHandle.Call(handle) }()

	ms := uint32(timeout / time.Millisecond)
	r1, _, _ := waitForSingleObject.Call(handle, uintptr(ms))
	switch r1 {
	case waitObject0, waitTimeout, waitFailed:
		// waitObject0 = process exited; waitTimeout = drain elapsed;
		// waitFailed = handle already closed. All three are terminal
		// for our purposes — no further wait needed.
		return
	default:
		return
	}
}

// fallbackSleep is the last-resort drain path used when the kernel32
// helpers can't be loaded. Matches the cap promised by [cancelDrainTimeout]
// without a real exit probe.
func fallbackSleep(timeout time.Duration) {
	time.Sleep(timeout)
}
