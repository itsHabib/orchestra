package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/fatih/color"
	olog "github.com/itsHabib/orchestra/internal/log"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/spf13/cobra"
)

var workspaceFlag string

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print team status from a workspace",
	Run: func(_ *cobra.Command, _ []string) {
		logger := olog.New()

		runService := newRunService(workspaceFlag, logger)
		state, err := runService.Snapshot(context.Background())
		switch {
		case runsvc.IsNotFound(err):
			fmt.Println()
			fmt.Println("  No active run.")
			fmt.Println()
			return
		case err != nil:
			logger.Error("Failed to read state: %s", err)
			os.Exit(1)
		}

		counts := map[string]int{}
		names := make([]string, 0, len(state.Agents))
		statusMap := make(map[string]string)
		for name, ts := range state.Agents {
			names = append(names, name)
			st := ts.Status
			if st == "" {
				st = "pending"
			}
			counts[st]++
			statusMap[name] = st
		}
		sort.Strings(names)
		total := len(names)

		// Summary line
		bold := color.New(color.Bold)
		fmt.Println()
		_, _ = bold.Printf("  %s\n", state.Project)

		var parts []string
		if n := counts["done"]; n > 0 {
			parts = append(parts, color.GreenString("%d done", n))
		}
		if n := counts["running"]; n > 0 {
			parts = append(parts, color.YellowString("%d running", n))
		}
		if n := counts["pending"]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d pending", n))
		}
		if n := counts["failed"]; n > 0 {
			parts = append(parts, color.RedString("%d failed", n))
		}
		fmt.Printf("  Teams: %d total — %s\n\n", total, strings.Join(parts, " | "))

		// Table
		fmt.Printf("  %-16s │ %-8s │ %-12s │ %-10s\n", "Team", "Status", "Tokens", "Duration")
		fmt.Printf("  ────────────────┼──────────┼──────────────┼────────────\n")

		now := time.Now()
		var totalIn, totalOut int64
		for _, name := range names {
			ts := state.Agents[name]
			st := statusMap[name]

			tokens := ""
			dur := ""

			if ts.InputTokens > 0 || ts.OutputTokens > 0 {
				tokens = fmt.Sprintf("%s→%s", fmtTokens(ts.InputTokens), fmtTokens(ts.OutputTokens))
				totalIn += ts.InputTokens
				totalOut += ts.OutputTokens
			}

			switch {
			case ts.DurationMs > 0:
				d := time.Duration(ts.DurationMs) * time.Millisecond
				dur = fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
			case st == "running" && !ts.StartedAt.IsZero():
				d := now.Sub(ts.StartedAt).Round(time.Second)
				dur = fmt.Sprintf("%dm %02ds ⏱", int(d.Minutes()), int(d.Seconds())%60)
			}

			// Colorize status
			stStr := st
			switch st {
			case "done":
				stStr = color.GreenString(st)
			case "running":
				stStr = color.YellowString(st)
			case "failed":
				stStr = color.RedString(st)
			}

			fmt.Printf("  %-16s │ %-17s │ %-12s │ %s\n", name, stStr, tokens, dur)
		}

		fmt.Printf("  ────────────────┼──────────┼──────────────┼────────────\n")
		if totalIn > 0 || totalOut > 0 {
			fmt.Printf("  %-16s │          │ %s→%-7s │\n", "Total", fmtTokens(totalIn), fmtTokens(totalOut))
		}
		fmt.Println()
	},
}

func init() {
	statusCmd.Flags().StringVar(&workspaceFlag, "workspace", workspaceDir, "Path to workspace directory")
}
