package cmd

import (
	"fmt"
	"strconv"
	"time"

	"github.com/fatih/color"
	olog "github.com/itsHabib/orchestra/internal/log"
	"github.com/itsHabib/orchestra/pkg/orchestra"
)

func printSummary(logger *olog.Logger, result *orchestra.Result, wallClock time.Duration) {
	if result == nil {
		logger.Warn("Summary unavailable: no result")
		return
	}

	bold := color.New(color.Bold)
	fmt.Println()
	_, _ = bold.Println("═══════════════════════════════════════════════════")
	_, _ = bold.Printf("  Orchestra: %s — Complete\n", result.Project)
	_, _ = bold.Println("═══════════════════════════════════════════════════")
	fmt.Println()

	fmt.Printf("  %-16s │ %-8s │ %-12s │ %-6s │ %-10s\n", "Team", "Status", "Tokens", "Turns", "Duration")
	fmt.Printf("  ────────────────┼──────────┼──────────────┼────────┼────────────\n")

	totals := printTeamRows(result)

	fmt.Printf("  ────────────────┼──────────┼──────────────┼────────┼────────────\n")
	fmt.Printf("  %-16s │          │ %s→%-7s │ %-6d │\n", "Total", fmtTokens(totals.in), fmtTokens(totals.out), totals.turns)
	fmt.Println()

	wc := wallClock.Round(time.Second)
	fmt.Printf("  Wall clock: %dm %02ds\n", int(wc.Minutes()), int(wc.Seconds())%60)
	fmt.Println()
}

type tokenTotals struct {
	in    int64
	out   int64
	turns int
}

func printTeamRows(result *orchestra.Result) tokenTotals {
	var totals tokenTotals
	for _, tier := range result.Tiers {
		for _, name := range tier {
			team, ok := result.Teams[name]
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
			fmt.Printf("  %-16s │ %-8s │ %-12s │ %-6s │ %s\n", name, team.Status, tokens, turns, dur)
		}
	}
	return totals
}
