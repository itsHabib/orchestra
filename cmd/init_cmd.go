package cmd

import (
	"context"
	"os"

	"github.com/itsHabib/orchestra/internal/config"
	olog "github.com/itsHabib/orchestra/internal/log"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/itsHabib/orchestra/internal/store/filestore"
	"github.com/itsHabib/orchestra/internal/workspace"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init <config.yaml>",
	Short: "Initialize a .orchestra/ workspace from config",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		logger := olog.New()

		res, err := config.Load(args[0])
		if err != nil {
			logger.Error("Validation failed: %s", err)
			os.Exit(1)
		}

		for _, w := range res.Warnings {
			logger.Warn("%s", w)
		}
		if !res.Valid() {
			logger.Error("Validation failed: %s", res.Err())
			os.Exit(1)
		}

		runService := newRunService(workspaceDir, logger)
		active, err := runService.Begin(context.Background(), res.Config)
		if err != nil {
			logger.Error("Failed to init workspace: %s", err)
			os.Exit(1)
		}
		if err := runService.End(active); err != nil {
			logger.Error("Failed to release workspace lock: %s", err)
			os.Exit(1)
		}

		logger.Success("Workspace created at %s", workspaceDir)
	},
}

// newRunService builds a run.Service backed by the filestore plus a workspace
// mirror at path. Registry mirror failures flow through logger.Warn.
func newRunService(path string, logger *olog.Logger) *runsvc.Service {
	return runsvc.New(
		filestore.New(path),
		runsvc.WithWorkspace(workspace.ForPath(path)),
		runsvc.WithWarn(logger.Warn),
	)
}
