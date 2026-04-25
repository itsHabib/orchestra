package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	agentservice "github.com/itsHabib/orchestra/internal/agents"
	"github.com/spf13/cobra"
)

const (
	defaultAgentPruneAge = 30 * 24 * time.Hour
)

var (
	agentsWorkspaceFlag string
	agentsPruneApply    bool
	agentsPruneOlder    time.Duration
	agentsReconcile     bool
)

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Inspect the user-scoped Managed Agents cache",
}

var agentsLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List cached Managed Agents",
	Run: func(cmd *cobra.Command, _ []string) {
		ctx := cmd.Context()
		rows, err := loadAgentRows(ctx, agentsWorkspaceFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agents ls: %v\n", err)
			os.Exit(1)
		}
		printAgentRows(rows)
	},
}

var agentsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Prune stale Managed Agents cache records",
	Run: func(cmd *cobra.Command, _ []string) {
		ctx := cmd.Context()
		if err := runAgentsPrune(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "agents prune: %v\n", err)
			os.Exit(1)
		}
	},
}

func init() {
	agentsCmd.PersistentFlags().StringVar(&agentsWorkspaceFlag, "workspace", workspaceDir, "Path to workspace directory")

	agentsPruneCmd.Flags().BoolVar(&agentsPruneApply, "apply", false, "Delete stale cache records")
	agentsPruneCmd.Flags().DurationVar(&agentsPruneOlder, "older-than", defaultAgentPruneAge, "Prune records not used within this duration")
	agentsPruneCmd.Flags().BoolVar(&agentsReconcile, "reconcile", false, "Also list orchestra-tagged agents missing from the cache")

	agentsCmd.AddCommand(agentsLsCmd)
	agentsCmd.AddCommand(agentsPruneCmd)
}

type agentRow = agentservice.Summary
type orphanAgent = agentservice.Orphan

func loadAgentRows(ctx context.Context, workspace string) ([]agentRow, error) {
	_, svc, err := newAgentService(workspace)
	if err != nil {
		return nil, err
	}
	summaries, err := svc.List(ctx)
	if err != nil {
		return nil, err
	}
	return summaries, nil
}

func printAgentRows(rows []agentRow) {
	fmt.Printf("%-32s  %-28s  %-7s  %-16s  %s\n", "KEY", "AGENT ID", "VERSION", "LAST USED", "MA STATUS")
	for i := range rows {
		row := &rows[i]
		status := string(row.Status)
		if row.Err != nil {
			status = status + " (" + compactError(row.Err) + ")"
		}
		fmt.Printf("%-32s  %-28s  %-7d  %-16s  %s\n",
			row.Record.Key,
			row.Record.AgentID,
			row.Record.Version,
			formatCacheTime(row.Record.LastUsed),
			status,
		)
	}
}

func runAgentsPrune(ctx context.Context) error {
	st, svc, err := newAgentService(agentsWorkspaceFlag)
	if err != nil {
		return err
	}
	report, err := svc.Prune(ctx, agentservice.PruneOpts{
		Apply:  agentsPruneApply,
		MaxAge: agentsPruneOlder,
	})
	if err != nil {
		return err
	}
	printStaleAgentReport(report, agentsPruneApply, "No stale cached agents.", "")

	if agentsReconcile {
		records, err := st.ListAgents(ctx)
		if err != nil {
			return err
		}
		orphaned, err := svc.Orphans(ctx, excludeCacheRecords(records))
		if err != nil {
			return err
		}
		printOrphanAgents(orphaned)
	}
	return nil
}

func printStaleAgentReport(
	report *agentservice.PruneReport,
	apply bool,
	emptyMessage string,
	entryPrefix string,
) {
	if len(report.Stale) == 0 {
		fmt.Println(emptyMessage)
		return
	}

	action := "Would delete"
	if apply {
		action = "Deleted"
	}
	for i := range report.Stale {
		row := &report.Stale[i]
		reason := agentservice.StaleReason(row, report.Now, report.MaxAge)
		fmt.Printf("%s %s%s (%s)\n", action, entryPrefix, row.Record.Key, reason)
	}
	if !apply {
		fmt.Println("Dry run only. Re-run with --apply to delete these cache records.")
	}
}

func printOrphanAgents(orphaned []orphanAgent) {
	if len(orphaned) == 0 {
		fmt.Println("No orchestra-tagged MA agents are missing from the cache.")
		return
	}
	fmt.Println("Orchestra-tagged MA agents missing from cache:")
	fmt.Printf("%-32s  %-28s  %-7s  %s\n", "KEY", "AGENT ID", "VERSION", "MA STATUS")
	for _, agent := range orphaned {
		fmt.Printf("%-32s  %-28s  %-7d  %s\n", agent.Key, agent.AgentID, agent.Version, agent.Status)
	}
}

func formatCacheTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func compactError(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(msg) <= 80 {
		return msg
	}
	return msg[:77] + "..."
}
