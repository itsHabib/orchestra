package cmd

import (
	"fmt"
	"os"
	"time"

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

		fmt.Printf("\n  Project: %s\n\n", state.Project)
		fmt.Printf("  %-16s │ %-8s │ %-8s │ %-10s\n", "Team", "Status", "Cost", "Duration")
		fmt.Printf("  ────────────────┼──────────┼──────────┼────────────\n")

		var totalCost float64
		for _, entry := range reg.Teams {
			ts := state.Teams[entry.Name]
			cost := ""
			dur := ""
			if ts.CostUSD > 0 {
				cost = fmt.Sprintf("$%.2f", ts.CostUSD)
				totalCost += ts.CostUSD
			}
			if ts.DurationMs > 0 {
				d := time.Duration(ts.DurationMs) * time.Millisecond
				dur = fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
			}
			fmt.Printf("  %-16s │ %-8s │ %-8s │ %s\n", entry.Name, ts.Status, cost, dur)
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
