package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/pkg/orchestra"
	"github.com/spf13/cobra"
)

var (
	sessionsWorkspaceFlag string
	sessionsLsAllFlag     bool
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Inspect managed-agents sessions in the active run",
}

var sessionsLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List teams in the active run with their managed-agents session info",
	Long: "By default lists only steerable rows (status=running with a " +
		"recorded session id) — the rows `orchestra msg` and `orchestra " +
		"interrupt` can target. Pass --all to include pending / done / " +
		"failed / terminated rows for inspection. Exits 0 with an empty " +
		"table when no active run exists.",
	Run: func(cmd *cobra.Command, _ []string) {
		if err := runSessionsLs(cmd.Context(), os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "sessions ls: %v\n", err)
			os.Exit(1)
		}
	},
}

func runSessionsLs(ctx context.Context, out io.Writer) error {
	// Gate on backend before fetching rows so local-backend workspaces
	// surface the same ErrLocalBackend they always have. orchestra.ListSessions
	// returns an empty slice (no error) for that case to match the SDK
	// contract; the CLI applies the stricter check.
	state, err := loadActiveRunState(ctx, sessionsWorkspaceFlag)
	if err != nil {
		if errors.Is(err, spawner.ErrNoActiveRun) {
			printSessionsTable(out, nil)
			return nil
		}
		return err
	}
	if state.Backend != "" && state.Backend != "managed_agents" {
		return spawner.ErrLocalBackend
	}

	// orchestra.ListSessions takes only a workspace dir — the underlying
	// filestore read derives its own short-lived context per the SDK's
	// "safe to call any time" contract. The cmd ctx remains the cobra
	// invocation context, used elsewhere in the CLI.
	rows, err := orchestra.ListSessions(sessionsWorkspaceFlag) //nolint:contextcheck // SDK signature deliberately excludes ctx; helper bounds its own read.
	if err != nil {
		return err
	}
	if !sessionsLsAllFlag {
		filtered := rows[:0]
		for i := range rows {
			if rows[i].Steerable {
				filtered = append(filtered, rows[i])
			}
		}
		rows = filtered
	}
	printSessionsTable(out, rows)
	return nil
}

func printSessionsTable(w io.Writer, rows []orchestra.SessionInfo) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TEAM\tSTATUS\tSTEERABLE\tSESSION_ID\tAGENT_ID\tLAST_EVENT_ID\tLAST_EVENT_AT")
	for i := range rows {
		row := &rows[i]
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.Team,
			displayOrDash(row.TeamStatus),
			yesNo(row.Steerable),
			displayOrDash(row.SessionID),
			displayOrDash(row.AgentID),
			displayOrDash(row.LastEventID),
			displayTime(row.LastEventAt),
		)
	}
	_ = tw.Flush()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func displayOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func displayTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func init() {
	sessionsCmd.PersistentFlags().StringVar(&sessionsWorkspaceFlag, "workspace", workspaceDir, "Path to workspace directory")
	sessionsLsCmd.Flags().BoolVar(&sessionsLsAllFlag, "all", false, "Include non-steerable rows (pending / done / failed / terminated)")
	sessionsCmd.AddCommand(sessionsLsCmd)
}
