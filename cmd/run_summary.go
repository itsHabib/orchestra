package cmd

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/fatih/color"
	"github.com/itsHabib/orchestra/pkg/orchestra"
)

func printSummary(w io.Writer, result *orchestra.Result, wallClock time.Duration) {
	if result == nil {
		yellow := color.New(color.FgYellow)
		_, _ = yellow.Fprintf(w, "  ⚠ Summary unavailable: no result\n")
		return
	}

	bold := color.New(color.Bold)
	_, _ = fmt.Fprintln(w)
	_, _ = bold.Fprintln(w, "═══════════════════════════════════════════════════")
	_, _ = bold.Fprintf(w, "  Orchestra: %s — Complete\n", result.Project)
	_, _ = bold.Fprintln(w, "═══════════════════════════════════════════════════")
	_, _ = fmt.Fprintln(w)

	_, _ = fmt.Fprintf(w, "  %-16s │ %-8s │ %-12s │ %-6s │ %-10s\n", "Team", "Status", "Tokens", "Turns", "Duration")
	_, _ = fmt.Fprintf(w, "  ────────────────┼──────────┼──────────────┼────────┼────────────\n")

	totals := printTeamRows(w, result)

	_, _ = fmt.Fprintf(w, "  ────────────────┼──────────┼──────────────┼────────┼────────────\n")
	_, _ = fmt.Fprintf(w, "  %-16s │          │ %s→%-7s │ %-6d │\n", "Total", fmtTokens(totals.in), fmtTokens(totals.out), totals.turns)
	_, _ = fmt.Fprintln(w)

	wc := wallClock.Round(time.Second)
	_, _ = fmt.Fprintf(w, "  Wall clock: %dm %02ds\n", int(wc.Minutes()), int(wc.Seconds())%60)
	_, _ = fmt.Fprintln(w)
}

type tokenTotals struct {
	in    int64
	out   int64
	turns int
}

func printTeamRows(w io.Writer, result *orchestra.Result) tokenTotals {
	var totals tokenTotals
	for _, tier := range result.Tiers {
		for _, name := range tier {
			team, ok := result.Agents[name]
			if !ok {
				continue
			}
			tokens := ""
			turns := ""
			dur := ""
			if team.InputTokens > 0 || team.OutputTokens > 0 {
				tokens = fmt.Sprintf("%s→%s", fmtTokens(team.InputTokens), fmtTokens(team.OutputTokens))
				totals.in += team.InputTokens
				totals.out += team.OutputTokens
			}
			if team.NumTurns > 0 {
				turns = strconv.Itoa(team.NumTurns)
				totals.turns += team.NumTurns
			}
			if team.DurationMs > 0 {
				d := time.Duration(team.DurationMs) * time.Millisecond
				dur = fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
			}
			_, _ = fmt.Fprintf(w, "  %-16s │ %-8s │ %-12s │ %-6s │ %s\n", name, team.Status, tokens, turns, dur)
		}
	}
	return totals
}
