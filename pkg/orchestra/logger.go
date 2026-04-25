package orchestra

import (
	olog "github.com/itsHabib/orchestra/internal/log"
)

// Logger is the orchestration loop's logging dependency. Library callers
// typically pass [NewNoopLogger]; CLI callers pass [NewCLILogger]; UI
// integrations supply their own implementation.
//
// Experimental: the method set captures exactly what the engine emits today
// and may grow as more orchestration phases gain dedicated events.
type Logger interface {
	// TeamMsg prints a team-prefixed message during a tier.
	TeamMsg(team, format string, args ...any)
	// TierStart prints a tier header before the tier's teams begin.
	TierStart(tierIdx int, teams []string)
	// Info prints a general informational message.
	Info(format string, args ...any)
	// Warn prints a non-fatal warning.
	Warn(format string, args ...any)
	// Error prints an error message. Run still returns the underlying
	// error; this is just for human-visible output along the way.
	Error(format string, args ...any)
	// Success prints a success message (e.g. workspace initialized).
	Success(format string, args ...any)
}

// NewCLILogger returns the colored, mutex-guarded stdout logger that
// orchestra's CLI uses. The internal logger satisfies [Logger]; this
// constructor is the supported way for SDK callers to opt into the same
// human-friendly output without importing internal packages.
func NewCLILogger() Logger { return olog.New() }

// NewNoopLogger returns a [Logger] that discards all output. This is the
// default when no [WithLogger] option is supplied to [Run].
func NewNoopLogger() Logger { return noopLogger{} }

type noopLogger struct{}

func (noopLogger) TeamMsg(string, string, ...any) {}
func (noopLogger) TierStart(int, []string)        {}
func (noopLogger) Info(string, ...any)            {}
func (noopLogger) Warn(string, ...any)            {}
func (noopLogger) Error(string, ...any)           {}
func (noopLogger) Success(string, ...any)         {}
