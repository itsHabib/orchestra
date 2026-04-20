package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

const defaultRunsListLimit = 20

var (
	runsWorkspaceFlag string
	runsListLimit     int
	runsPruneApply    bool
	runsPruneOlder    time.Duration
	runsReconcile     bool
)

var runsCmd = &cobra.Command{
	Use:   "runs",
	Short: "Inspect workflow runs",
}

var runsLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List recent workflow runs",
	Run: func(_ *cobra.Command, _ []string) {
		records, err := loadRunRecords(runsWorkspaceFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runs ls: %v\n", err)
			os.Exit(1)
		}
		printRunRows(records, runsListLimit, time.Now().UTC())
	},
}

var runsShowCmd = &cobra.Command{
	Use:   "show <run-id>",
	Short: "Show teams and backend resources for a workflow run",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		records, err := loadRunRecords(runsWorkspaceFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runs show: %v\n", err)
			os.Exit(1)
		}
		record, ok := findRunRecord(records, args[0])
		if !ok {
			fmt.Fprintf(os.Stderr, "runs show: run %q not found\n", args[0])
			os.Exit(1)
		}
		printRunDetail(record, time.Now().UTC())
	},
}

var runsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Prune stale cached resources associated with workflow runs",
	Run: func(cmd *cobra.Command, _ []string) {
		if err := runRunsPrune(cmd.Context()); err != nil {
			fmt.Fprintf(os.Stderr, "runs prune: %v\n", err)
			os.Exit(1)
		}
	},
}

func init() {
	runsCmd.PersistentFlags().StringVar(&runsWorkspaceFlag, "workspace", workspaceDir, "Path to workspace directory")

	runsLsCmd.Flags().IntVar(&runsListLimit, "limit", defaultRunsListLimit, "Maximum runs to list; 0 lists all")

	runsPruneCmd.Flags().BoolVar(&runsPruneApply, "apply", false, "Delete stale cache records")
	runsPruneCmd.Flags().DurationVar(&runsPruneOlder, "older-than", defaultAgentPruneAge, "Consider cache records stale after this idle duration")
	runsPruneCmd.Flags().BoolVar(&runsReconcile, "reconcile", false, "Also list orchestra-tagged agents not referenced by any known run")

	runsCmd.AddCommand(runsLsCmd)
	runsCmd.AddCommand(runsShowCmd)
	runsCmd.AddCommand(runsPruneCmd)
}
