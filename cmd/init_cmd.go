package cmd

import (
	"os"

	"github.com/michaelhabib/orchestra/internal/config"
	olog "github.com/michaelhabib/orchestra/internal/log"
	"github.com/michaelhabib/orchestra/internal/workspace"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init <config.yaml>",
	Short: "Initialize a .orchestra/ workspace from config",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		logger := olog.New()

		cfg, warnings, err := config.Load(args[0])
		if err != nil {
			logger.Error("Validation failed: %s", err)
			os.Exit(1)
		}

		for _, w := range warnings {
			logger.Warn("%s", w)
		}

		ws, err := workspace.Init(cfg)
		if err != nil {
			logger.Error("Failed to init workspace: %s", err)
			os.Exit(1)
		}

		logger.Success("Workspace created at %s", ws.Path)
	},
}
