package cmd

import (
	"github.com/spf13/cobra"
)

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
}
