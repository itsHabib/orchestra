package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/spf13/cobra"
)

var (
	interruptWorkspaceFlag string
	interruptTeamFlag      string
)

var interruptCmd = &cobra.Command{
	Use:   "interrupt",
	Short: "Send a user.interrupt to a running team's managed-agents session",
	Long: "Deliver a user.interrupt to the named team's MA session in the " +
		"workspace's active run. Always uses at-most-once delivery (no " +
		"retries) — duplicate interrupts could double-cancel a recovery " +
		"cycle.",
	Run: func(cmd *cobra.Command, _ []string) {
		if err := runInterrupt(cmd.Context()); err != nil {
			fmt.Fprintf(os.Stderr, "interrupt: %v\n", err)
			os.Exit(1)
		}
	},
}

func runInterrupt(ctx context.Context) error {
	sessionID, err := resolveSteerableTeam(ctx, interruptWorkspaceFlag, interruptTeamFlag)
	if err != nil {
		return err
	}
	sessions, err := steeringSessionEventsFactory(ctx)
	if err != nil {
		return fmt.Errorf("session events client: %w", err)
	}
	if err := spawner.SendUserInterrupt(ctx, sessions, sessionID, 0); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, "ok")
	return nil
}

func init() {
	interruptCmd.Flags().StringVar(&interruptWorkspaceFlag, "workspace", workspaceDir, "Path to workspace directory")
	interruptCmd.Flags().StringVar(&interruptTeamFlag, "team", "", "Team name from orchestra.yaml")
	_ = interruptCmd.MarkFlagRequired("team")
}
