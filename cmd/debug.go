package cmd

import "github.com/spf13/cobra"

var debugCmd = &cobra.Command{
	Use:   "debug",
	Short: "Inspect low-level Orchestra internals",
}

func init() {
	debugCmd.AddCommand(agentsCmd)
}
