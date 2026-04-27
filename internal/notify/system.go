package notify

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

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
	// AppleScript string literals support \" and \\ escapes; osascriptEscape
	// produces a body that is safe to embed verbatim with %q. The %q here is
	// used as a Go-style quoter for the AppleScript syntax — the rules
	// happen to coincide for printable ASCII.
	body := osascriptEscape(systemBody(n))
	title := osascriptEscape("Orchestra")
	script := fmt.Sprintf(`display notification %q with title %q`, body, title)
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify: osascript: %w", err)
	}
	return nil
}

type notifySendNotifier struct{}

func (notifySendNotifier) notify(ctx context.Context, n *Notification) error {
	cmd := exec.CommandContext(ctx, "notify-send", "Orchestra", systemBody(n))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify: notify-send: %w", err)
	}
	return nil
}

type windowsNoopNotifier struct{}

func (windowsNoopNotifier) notify(context.Context, *Notification) error { return nil }

// osascriptEscape neutralizes characters the AppleScript display notification
// command interprets — backslash and double-quote inside the string literal.
// AppleScript here is invoked via `osascript -e` so backticks, dollar signs,
// and the like never reach a shell parser.
func osascriptEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

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
