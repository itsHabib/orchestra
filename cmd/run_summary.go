package cmd

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/fatih/color"
	"github.com/itsHabib/orchestra/internal/config"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/itsHabib/orchestra/internal/workspace"
)

func printSummary(ctx context.Context, runService *runsvc.Service, ws *workspace.Workspace, cfg *config.Config, wallClock time.Duration) {
	state, err := runService.Snapshot(ctx)
	if err != nil {
		return
	}

	bold := color.New(color.Bold)
	fmt.Println()
	_, _ = bold.Println("═══════════════════════════════════════════════════")
	_, _ = bold.Printf("  Orchestra: %s — Complete\n", state.Project)
	_, _ = bold.Println("═══════════════════════════════════════════════════")
	fmt.Println()

	fmt.Printf("  %-16s │ %-8s │ %-12s │ %-6s │ %-10s\n", "Team", "Status", "Tokens", "Turns", "Duration")
	fmt.Printf("  ────────────────┼──────────┼──────────────┼────────┼────────────\n")

	var totalIn, totalOut int64
	var totalTurns int
	for _, team := range cfg.Teams {
		ts := state.Teams[team.Name]
		tokens := ""
		turns := ""
		dur := ""
		if ts.InputTokens > 0 || ts.OutputTokens > 0 {
			tokens = fmt.Sprintf("%s→%s", fmtTokens(ts.InputTokens), fmtTokens(ts.OutputTokens))
			totalIn += ts.InputTokens
			totalOut += ts.OutputTokens
		}
		if res, err := ws.ReadResult(team.Name); err == nil {
			turns = strconv.Itoa(res.NumTurns)
			totalTurns += res.NumTurns
		}
		if ts.DurationMs > 0 {
			d := time.Duration(ts.DurationMs) * time.Millisecond
			dur = fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
		}
		fmt.Printf("  %-16s │ %-8s │ %-12s │ %-6s │ %s\n", team.Name, ts.Status, tokens, turns, dur)
	}

	fmt.Printf("  ────────────────┼──────────┼──────────────┼────────┼────────────\n")
	fmt.Printf("  %-16s │          │ %s→%-7s │ %-6d │\n", "Total", fmtTokens(totalIn), fmtTokens(totalOut), totalTurns)
	fmt.Println()

	wc := wallClock.Round(time.Second)
	fmt.Printf("  Wall clock: %dm %02ds\n", int(wc.Minutes()), int(wc.Seconds())%60)
	fmt.Printf("  Results:    %s/results/\n", ws.Path)
	fmt.Printf("  Logs:       %s/logs/\n", ws.Path)
	fmt.Println()
}
