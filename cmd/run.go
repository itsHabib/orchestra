package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/itsHabib/orchestra/pkg/orchestra"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <config.yaml>",
	Short: "Full orchestration: init, DAG, spawn tiers, collect, summary",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		res, err := orchestra.LoadConfig(args[0])
		if err != nil {
			orchestra.PrintEvent(os.Stdout, orchestra.Event{
				Kind:    orchestra.EventError,
				Tier:    -1,
				Message: fmt.Sprintf("Config error: %s", err),
				At:      time.Now(),
			})
			os.Exit(1)
		}
		for _, w := range res.Warnings {
			orchestra.PrintEvent(os.Stdout, orchestra.Event{
				Kind:    orchestra.EventWarn,
				Tier:    -1,
				Message: w.String(),
				At:      time.Now(),
			})
		}
		if !res.Valid() {
			orchestra.PrintEvent(os.Stdout, orchestra.Event{
				Kind:    orchestra.EventError,
				Tier:    -1,
				Message: fmt.Sprintf("Config error: %s", res.Err()),
				At:      time.Now(),
			})
			os.Exit(1)
		}

		wallStart := time.Now()
		// Cancel the run on SIGINT (Ctrl-C) or SIGTERM (cancel_run /
		// `kill <pid>`) so the engine can flip running agents to
		// "canceled" before exiting. Without this, MCP-side cancel_run
		// would just kill the subprocess, leaving state.json with
		// agents stuck at "running" forever.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		result, err := orchestra.Run(ctx, res.Config,
			orchestra.WithWorkspaceDir(workspaceDir),
			orchestra.WithEventHandler(func(ev orchestra.Event) {
				orchestra.PrintEvent(os.Stdout, ev)
			}),
		)
		if err != nil {
			if errors.Is(err, orchestra.ErrRunInProgress) {
				orchestra.PrintEvent(os.Stdout, orchestra.Event{
					Kind:    orchestra.EventError,
					Tier:    -1,
					Message: "Another orchestra run is already in progress for this workspace",
					At:      time.Now(),
				})
				os.Exit(1)
			}
			orchestra.PrintEvent(os.Stdout, orchestra.Event{
				Kind:    orchestra.EventError,
				Tier:    -1,
				Message: fmt.Sprintf("Orchestration failed: %s", err),
				At:      time.Now(),
			})
			os.Exit(1)
		}
		printSummary(os.Stdout, result, time.Since(wallStart))
	},
}
