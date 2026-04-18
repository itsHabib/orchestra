package log

import (
	"fmt"

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

// Logger provides colored, team-prefixed terminal logging.
type Logger struct {
	colorIdx int
	colors   map[string]*color.Color
}

// New creates a new Logger.
func New() *Logger {
	return &Logger{
		colors: make(map[string]*color.Color),
	}
}

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
	c := l.colorFor(team)
	prefix := c.Sprintf("[%s]", team)
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s\n", prefix, msg)
}

// TierStart prints a tier header.
func (l *Logger) TierStart(tierIdx int, teams []string) {
	bold := color.New(color.Bold)
	_, _ = bold.Printf("\n━━━ Tier %d: %v ━━━\n", tierIdx, teams)
}

// Info prints a general info message.
func (l *Logger) Info(format string, args ...any) {
	fmt.Printf("  %s\n", fmt.Sprintf(format, args...))
}

// Warn prints a warning message.
func (l *Logger) Warn(format string, args ...any) {
	yellow := color.New(color.FgYellow)
	_, _ = yellow.Printf("  ⚠ %s\n", fmt.Sprintf(format, args...))
}

// Error prints an error message.
func (l *Logger) Error(format string, args ...any) {
	red := color.New(color.FgRed)
	_, _ = red.Printf("  ✗ %s\n", fmt.Sprintf(format, args...))
}

// Success prints a success message.
func (l *Logger) Success(format string, args ...any) {
	green := color.New(color.FgGreen)
	_, _ = green.Printf("  ✓ %s\n", fmt.Sprintf(format, args...))
}
