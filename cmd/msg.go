package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/spf13/cobra"
)

// defaultSteerRetryAttempts is the retry budget orchestra msg uses when the
// caller does not pass --no-retry. Matches the spawner's default
// session-event retry shape so behavior is consistent with the initial-prompt
// send path.
const defaultSteerRetryAttempts = 4

var (
	msgWorkspaceFlag string
	msgTeamFlag      string
	msgMessageFlag   string
	msgNoRetryFlag   bool
)

var msgCmd = &cobra.Command{
	Use:   "msg",
	Short: "Send a user.message into a running team's managed-agents session",
	Long: "Deliver a user.message to the named team's MA session in the " +
		"workspace's active run. Defaults to at-least-once: 5xx/429 errors " +
		"are retried, so the agent may observe the same message twice in " +
		"the rare 5xx-then-success case. Pass --no-retry for at-most-once.",
	Run: func(cmd *cobra.Command, _ []string) {
		if err := runMsg(cmd.Context()); err != nil {
			fmt.Fprintf(os.Stderr, "msg: %v\n", err)
			os.Exit(1)
		}
	},
}

func runMsg(ctx context.Context) error {
	if msgMessageFlag == "" {
		return errors.New("--message is required")
	}
	sessionID, err := resolveSteerableTeam(ctx, msgWorkspaceFlag, msgTeamFlag)
	if err != nil {
		return err
	}
	sessions, err := steeringSessionEventsFactory(ctx)
	if err != nil {
		return fmt.Errorf("session events client: %w", err)
	}
	retries := defaultSteerRetryAttempts
	if msgNoRetryFlag {
		retries = 0
	}
	if err := spawner.SendUserMessage(ctx, sessions, sessionID, msgMessageFlag, retries); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, "ok")
	return nil
}

func init() {
	msgCmd.Flags().StringVar(&msgWorkspaceFlag, "workspace", workspaceDir, "Path to workspace directory")
	msgCmd.Flags().StringVar(&msgTeamFlag, "team", "", "Team name from orchestra.yaml")
	msgCmd.Flags().StringVar(&msgMessageFlag, "message", "", "Text to deliver to the team")
	msgCmd.Flags().BoolVar(&msgNoRetryFlag, "no-retry", false, "Disable retry on 429/5xx (at-most-once delivery)")
	_ = msgCmd.MarkFlagRequired("team")
	_ = msgCmd.MarkFlagRequired("message")
}
