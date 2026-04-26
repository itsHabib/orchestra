// Package log provides the colored, team-prefixed terminal logging used by
// orchestra's CLI surface. The pkg/orchestra SDK no longer exports this
// package — pkg/orchestra.PrintEvent wraps it for callers who want the same
// look-and-feel.
package log

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/fatih/color"
)

var teamColors = []*color.Color{
	color.New(color.FgCyan),
	color.New(color.FgGreen),
	color.New(color.FgYellow),
	color.New(color.FgMagenta),
	color.New(color.FgBlue),
	color.New(color.FgRed),
	color.New(color.FgHiCyan),
	color.New(color.FgHiGreen),
}

// Logger provides colored, team-prefixed terminal logging. Safe for
// concurrent use across the goroutines spawned per team in a tier.
//
// All output is written to the configured io.Writer (stdout by default).
// Concurrent writers are serialized under the Logger's mutex so partial
// lines do not interleave.
type Logger struct {
	mu       sync.Mutex
	w        io.Writer
	colorIdx int
	colors   map[string]*color.Color
}

// New creates a Logger that writes to os.Stdout.
func New() *Logger {
	return NewWithWriter(os.Stdout)
}

// NewWithWriter creates a Logger that writes to w. Passing nil yields a
// Logger that writes to os.Stdout.
func NewWithWriter(w io.Writer) *Logger {
	if w == nil {
		w = os.Stdout
	}
	return &Logger{
		w:      w,
		colors: make(map[string]*color.Color),
	}
}

// colorFor returns a stable color for team. The mapping is built lazily as
// teams first appear so the Logger needs no setup.
func (l *Logger) colorFor(team string) *color.Color {
	if c, ok := l.colors[team]; ok {
		return c
	}
	c := teamColors[l.colorIdx%len(teamColors)]
	l.colorIdx++
	l.colors[team] = c
	return c
}

// TeamMsg prints a team-prefixed message.
func (l *Logger) TeamMsg(team, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.colorFor(team)
	prefix := c.Sprintf("[%s]", team)
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(l.w, "%s %s\n", prefix, msg)
}

// TierStart prints a tier header.
func (l *Logger) TierStart(tierIdx int, teams []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bold := color.New(color.Bold)
	_, _ = bold.Fprintf(l.w, "\n━━━ Tier %d: %v ━━━\n", tierIdx, teams)
}

// Info prints a general info message.
func (l *Logger) Info(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.w, "  %s\n", fmt.Sprintf(format, args...))
}

// Warn prints a warning message.
func (l *Logger) Warn(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	yellow := color.New(color.FgYellow)
	_, _ = yellow.Fprintf(l.w, "  ⚠ %s\n", fmt.Sprintf(format, args...))
}

// Error prints an error message.
func (l *Logger) Error(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	red := color.New(color.FgRed)
	_, _ = red.Fprintf(l.w, "  ✗ %s\n", fmt.Sprintf(format, args...))
}

// Success prints a success message.
func (l *Logger) Success(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	green := color.New(color.FgGreen)
	_, _ = green.Fprintf(l.w, "  ✓ %s\n", fmt.Sprintf(format, args...))
}

// Dropped prints the synthetic "events were dropped" indicator. The
// formatting matches what PrintEvent uses for the EventDropped kind so the
// CLI surface stays cohesive.
func (l *Logger) Dropped(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	yellow := color.New(color.FgYellow)
	_, _ = yellow.Fprintf(l.w, "  (dropped %d events)\n", n)
}
