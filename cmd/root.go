package cmd

import (
	"github.com/spf13/cobra"
)

// workspaceDir is the on-disk directory that orchestra commands read and write.
const workspaceDir = ".orchestra"

var rootCmd = &cobra.Command{
	Use:   "orchestra",
	Short: "Multi-team project orchestration CLI",
	Long:  "Orchestra orchestrates large software projects across multiple teams using Claude Code agents.",
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(spawnCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(agentsCmd)
}
