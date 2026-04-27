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
//
// Agent-provided fields (team, status, summary) are sanitized: control
// characters are replaced with spaces and the summary is truncated. Without
// this, a malicious or buggy agent could splash multi-line content or
// terminal escape sequences into the user's TTY just by passing them as
// signal_completion's summary.
func FormatTerminalLine(n *Notification) string {
	if n == nil {
		return ""
	}
	id := n.RunID
	if id == "" {
		id = "-"
	}
	return fmt.Sprintf("\a[NOTIFY] %s/%s: %s — %s\n",
		sanitizeTerminalField(id, terminalIDMax),
		sanitizeTerminalField(n.Team, terminalTeamMax),
		sanitizeTerminalField(n.Status, terminalStatusMax),
		sanitizeTerminalField(n.Summary, terminalSummaryMax),
	)
}

// Bounds chosen to keep the [NOTIFY] line on one row of a typical 120-col
// terminal even after the format scaffold is added. Large enough to be
// useful, small enough to bound damage from a mis-shaped summary.
const (
	terminalIDMax      = 32
	terminalTeamMax    = 48
	terminalStatusMax  = 16
	terminalSummaryMax = 160
)

// sanitizeTerminalField replaces ASCII control characters (and the DEL byte)
// with a space so a hostile or buggy agent can't splash newlines, carriage
// returns, or ANSI CSI escape sequences across the TTY through the notify
// surface, then truncates to maxLen with a "..." suffix. Multi-byte UTF-8
// above 0x7f is passed through unchanged — emoji and non-ASCII summaries
// render fine in modern terminals; the attack surface is byte-level controls.
func sanitizeTerminalField(s string, maxLen int) string {
	cleaned := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			cleaned = append(cleaned, ' ')
			continue
		}
		cleaned = append(cleaned, c)
	}
	if maxLen <= 0 || len(cleaned) <= maxLen {
		return string(cleaned)
	}
	// Truncate to maxLen-3, then drop any partial UTF-8 trailer at the cut.
	cut := maxLen - 3
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && cleaned[cut]&0xc0 == 0x80 {
		cut--
	}
	return string(cleaned[:cut]) + "..."
}
