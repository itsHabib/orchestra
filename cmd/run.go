package cmd

import (
	"context"
	"errors"
	"os"
	"time"

	olog "github.com/itsHabib/orchestra/internal/log"
	"github.com/itsHabib/orchestra/pkg/orchestra"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <config.yaml>",
	Short: "Full orchestration: init, DAG, spawn tiers, collect, summary",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		logger := olog.New()

		cfg, warnings, err := orchestra.LoadConfig(args[0])
		if err != nil {
			logger.Error("Config error: %s", err)
			os.Exit(1)
		}
		for _, w := range warnings {
			logger.Warn("%s", w)
		}

		wallStart := time.Now()
		result, err := orchestra.Run(context.Background(), cfg,
			orchestra.WithLogger(logger),
			orchestra.WithWorkspaceDir(workspaceDir),
		)
		if err != nil {
			if errors.Is(err, orchestra.ErrRunInProgress) {
				logger.Error("Another orchestra run is already in progress for this workspace")
				os.Exit(1)
			}
			logger.Error("Orchestration failed: %s", err)
			os.Exit(1)
		}
		printSummary(logger, result, time.Since(wallStart))
	},
}
