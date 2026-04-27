package notify

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// TerminalNotifier writes a `\a[NOTIFY] ...` line to a TTY. When the writer
// is not a terminal (CI logs, pipes, redirects to a file), Notify is a no-op
// — the bell character escaping into a log file is harmless but the design
// (§9.1) gates the behavior on an interactive terminal.
type TerminalNotifier struct {
	out   io.Writer
	isTTY bool
}

// NewTerminal returns a TerminalNotifier targeting f. The TTY check happens
// once at construction time; the writer is assumed stable for the life of the
// run. Pass os.Stderr to keep notifications off the result-of-run stdout.
func NewTerminal(f *os.File) *TerminalNotifier {
	tty := false
	if f != nil {
		fd := f.Fd()
		tty = isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
	}
	return &TerminalNotifier{out: f, isTTY: tty}
}

// newTerminalForTest constructs a TerminalNotifier with an explicit TTY flag.
// Helper for unit tests that pipe to a buffer.
func newTerminalForTest(out io.Writer, isTTY bool) *TerminalNotifier {
	return &TerminalNotifier{out: out, isTTY: isTTY}
}

// Notify writes the notification line if the writer is a TTY.
func (t *TerminalNotifier) Notify(_ context.Context, n *Notification) error {
	if n == nil || !t.isTTY || t.out == nil {
		return nil
	}
	if _, err := fmt.Fprint(t.out, FormatTerminalLine(n)); err != nil {
		return fmt.Errorf("notify: terminal write: %w", err)
	}
	return nil
}

// FormatTerminalLine renders the bell + one-line message used by the terminal
// notifier. Exposed so tests (and any future renderer that wants the same
// format) don't have to duplicate the format string.
func FormatTerminalLine(n *Notification) string {
	if n == nil {
		return ""
	}
	id := n.RunID
	if id == "" {
		id = "-"
	}
	return fmt.Sprintf("\a[NOTIFY] %s/%s: %s — %s\n", id, n.Team, n.Status, n.Summary)
}
