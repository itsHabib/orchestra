package orchestra

import (
	"io"
	"sync"

	olog "github.com/itsHabib/orchestra/internal/log"
)

// printEventState carries the per-writer Logger that PrintEvent uses to
// render colored output. Sharing the Logger across calls keeps team color
// assignments stable for the duration of a process — first team to emit
// gets cyan, second gets green, etc.
type printEventState struct {
	mu      sync.Mutex
	loggers map[io.Writer]*olog.Logger
}

var printEventLoggers = &printEventState{
	loggers: make(map[io.Writer]*olog.Logger),
}

// loggerFor returns (and lazily constructs) the Logger associated with w.
// PrintEvent uses one Logger per writer so output written to different
// destinations doesn't share team-color state.
func (s *printEventState) loggerFor(w io.Writer) *olog.Logger {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.loggers[w]; ok {
		return l
	}
	l := olog.NewWithWriter(w)
	s.loggers[w] = l
	return l
}

// PrintEvent renders ev to w in the colored, human-friendly format the
// CLI uses. Safe to call from multiple goroutines simultaneously: output
// to the same writer is serialized under an internal mutex so partial
// lines do not interleave.
//
// PrintEvent is the recommended way to give SDK callers the same console
// look-and-feel as the CLI without exposing internal/log:
//
//	for ev := range h.Events() {
//	    orchestra.PrintEvent(os.Stdout, ev)
//	}
//
// or, for one-shot Run callers:
//
//	orchestra.Run(ctx, cfg,
//	    orchestra.WithEventHandler(func(ev orchestra.Event) {
//	        orchestra.PrintEvent(os.Stdout, ev)
//	    }),
//	)
//
// Several event kinds render nothing so that the CLI output stays
// byte-identical to pre-PR-2: EventTierComplete and EventRunComplete are
// silent because the CLI's summary printer takes over there;
// EventToolCall and EventToolResult are silent because the previous CLI
// did not surface tool invocations either. Library callers that want
// every kind rendered can write their own handler — all event fields are
// populated.
//
// Experimental.
//
//nolint:gocritic // Event-by-value matches the rest of the SDK surface.
func PrintEvent(w io.Writer, ev Event) {
	if w == nil {
		return
	}
	l := printEventLoggers.loggerFor(w)
	switch ev.Kind {
	case EventTierStart:
		// Re-derive the team list from Message rather than carrying a
		// dedicated slice on Event — keeps the Event shape flat.
		l.TierStart(ev.Tier, splitTeamList(ev.Message))
	case EventTeamStart, EventTeamMessage, EventTeamComplete, EventTeamFailed:
		l.TeamMsg(ev.Team, "%s", ev.Message)
	case EventToolCall, EventToolResult:
		// Silent — matches today's CLI which did not render tool
		// invocations.
	case EventTierComplete, EventRunComplete:
		// Silent — the CLI's printSummary takes over.
	case EventInfo:
		l.Info("%s", ev.Message)
	case EventWarn:
		l.Warn("%s", ev.Message)
	case EventError:
		l.Error("%s", ev.Message)
	case EventDropped:
		l.Dropped(ev.DropCount)
	}
}

// splitTeamList parses the comma-separated team list emitted on
// EventTierStart back into the slice the Logger.TierStart formatter
// expects. Returns nil for an empty string so the formatter prints "[]"
// rather than "[ ]".
func splitTeamList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, trimSpaces(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, trimSpaces(s[start:]))
	return out
}

// trimSpaces returns s without leading or trailing ASCII spaces.
func trimSpaces(s string) string {
	for s != "" && s[0] == ' ' {
		s = s[1:]
	}
	for s != "" && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}
