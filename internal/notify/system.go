package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// systemNotifyTimeout caps how long a single platform notifier shellout may
// run. osascript on a locked screen and a stuck D-Bus on Linux can both hang
// indefinitely; the dispatch loop is the same goroutine that reads MA events,
// so a hung notifier would stall the entire team. Three seconds is generous
// enough for healthy invocations and tight enough that a hang turns into a
// logged failure rather than a wedged session.
const systemNotifyTimeout = 3 * time.Second

// SystemNotifier shells out to the platform's notification surface:
//
//	macOS:   osascript -e 'display notification ...'
//	Linux:   notify-send Orchestra '...'
//	Windows: no-op (no good built-in we can rely on without a popup library)
//
// All errors are wrapped and returned; the caller (the Compose fan-out) logs
// and ignores them so a missing notify-send binary or a sandboxed osascript
// never blocks the run.
type SystemNotifier struct {
	// notifier overrides the default platform-detected notifier. Test-only.
	notifier systemNotifierImpl
}

// NewSystem returns a SystemNotifier that picks an implementation based on
// runtime.GOOS at construction time. The platform detection runs once; the
// returned value is safe for concurrent use.
func NewSystem() *SystemNotifier {
	return &SystemNotifier{notifier: pickSystemNotifier()}
}

// Notify delivers n through the platform notifier.
func (s *SystemNotifier) Notify(ctx context.Context, n *Notification) error {
	if s.notifier == nil || n == nil {
		return nil
	}
	return s.notifier.notify(ctx, n)
}

type systemNotifierImpl interface {
	notify(ctx context.Context, n *Notification) error
}

func pickSystemNotifier() systemNotifierImpl {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("osascript"); err == nil {
			return osascriptNotifier{}
		}
	case "linux":
		if _, err := exec.LookPath("notify-send"); err == nil {
			return notifySendNotifier{}
		}
	}
	return windowsNoopNotifier{}
}

type osascriptNotifier struct{}

func (osascriptNotifier) notify(ctx context.Context, n *Notification) error {
	return runBoundedCommand(ctx, "osascript", "osascript", "-e", buildOsascript(systemBody(n), "Orchestra"))
}

// buildOsascript renders the AppleScript fragment passed to `osascript -e`.
// AppleScript string literals support `\"` and `\\` — the same escape shape
// Go's %q produces for printable ASCII — so %q alone is the right escape
// transform. An earlier intermediate quote/backslash doubler ran *before* %q
// and mangled bodies containing quotes (`\"world\"` ended up as
// `\\\"world\\\"`). Helper extracted so tests can pin the exact string
// without spawning osascript.
func buildOsascript(body, title string) string {
	return fmt.Sprintf(`display notification %q with title %q`, body, title)
}

type notifySendNotifier struct{}

func (notifySendNotifier) notify(ctx context.Context, n *Notification) error {
	return runBoundedCommand(ctx, "notify-send", "notify-send", "Orchestra", systemBody(n))
}

// runBoundedCommand caps a notifier shellout at systemNotifyTimeout. A
// timeout deadline derived from ctx (without cancellation) means the
// notification is treated as best-effort even if the engine's parent context
// has been canceled mid-team — we still want a fast, time-boxed attempt to
// display the notification rather than skipping it outright.
func runBoundedCommand(ctx context.Context, label, name string, args ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, systemNotifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, name, args...)
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("notify: %s: timed out after %s", label, systemNotifyTimeout)
	}
	return fmt.Errorf("notify: %s: %w", label, err)
}

type windowsNoopNotifier struct{}

func (windowsNoopNotifier) notify(context.Context, *Notification) error { return nil }

// systemBody renders the notification body for OS notifiers. Kept short
// because system notification panels typically truncate aggressively.
func systemBody(n *Notification) string {
	if n == nil {
		return ""
	}
	id := n.RunID
	if id == "" {
		id = "-"
	}
	return fmt.Sprintf("%s/%s: %s — %s", id, n.Team, n.Status, n.Summary)
}
