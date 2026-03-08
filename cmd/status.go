package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	olog "github.com/michaelhabib/orchestra/internal/log"
	"github.com/michaelhabib/orchestra/internal/workspace"
	"github.com/spf13/cobra"
)

var workspaceFlag string

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print team status from a workspace",
	Run: func(cmd *cobra.Command, args []string) {
		logger := olog.New()

		ws, err := workspace.Open(workspaceFlag)
		if err != nil {
			logger.Error("Failed to open workspace: %s", err)
			os.Exit(1)
		}

		state, err := ws.ReadState()
		if err != nil {
			logger.Error("Failed to read state: %s", err)
			os.Exit(1)
		}

		reg, err := ws.ReadRegistry()
		if err != nil {
			logger.Error("Failed to read registry: %s", err)
			os.Exit(1)
		}

		// Count teams by status (use registry status as source of truth for running)
		counts := map[string]int{}
		statusMap := make(map[string]string) // team name → effective status
		for _, entry := range reg.Teams {
			// Registry is more accurate for running teams (state only updates after spawn returns)
			st := entry.Status
			if st == "" {
				if ts, ok := state.Teams[entry.Name]; ok {
					st = ts.Status
				}
			}
			if st == "" {
				st = "pending"
			}
			counts[st]++
			statusMap[entry.Name] = st
		}
		total := len(reg.Teams)

		// Summary line
		bold := color.New(color.Bold)
		fmt.Println()
		bold.Printf("  %s\n", state.Project)

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
		fmt.Printf("  %-16s │ %-8s │ %-8s │ %-10s\n", "Team", "Status", "Cost", "Duration")
		fmt.Printf("  ────────────────┼──────────┼──────────┼────────────\n")

		now := time.Now()
		var totalCost float64
		for _, entry := range reg.Teams {
			ts := state.Teams[entry.Name]
			st := statusMap[entry.Name]

			cost := ""
			dur := ""

			if ts.CostUSD > 0 {
				cost = fmt.Sprintf("$%.2f", ts.CostUSD)
				totalCost += ts.CostUSD
			}

			switch {
			case ts.DurationMs > 0:
				d := time.Duration(ts.DurationMs) * time.Millisecond
				dur = fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
			case st == "running" && !entry.StartedAt.IsZero():
				d := now.Sub(entry.StartedAt).Round(time.Second)
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

			fmt.Printf("  %-16s │ %-17s │ %-8s │ %s\n", entry.Name, stStr, cost, dur)
		}

		fmt.Printf("  ────────────────┼──────────┼──────────┼────────────\n")
		if totalCost > 0 {
			fmt.Printf("  %-16s │          │ $%-7.2f │\n", "Total", totalCost)
		}
		fmt.Println()
	},
}

func init() {
	statusCmd.Flags().StringVar(&workspaceFlag, "workspace", ".orchestra", "Path to workspace directory")
}
