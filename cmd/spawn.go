package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/injection"
	olog "github.com/itsHabib/orchestra/internal/log"
	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/spf13/cobra"
)

var (
	agentFlag string
	teamFlag  string // deprecated: alias for --agent through v3.x
)

var spawnCmd = &cobra.Command{
	Use:   "spawn <config.yaml>",
	Short: "Spawn a single agent",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		logger := olog.New()

		res, err := config.Load(args[0])
		if err != nil {
			logger.Error("Validation failed: %s", err)
			os.Exit(1)
		}
		// spawn is a debug entrypoint for a single agent; intentionally skips
		// the res.Warnings render loop the other commands do (pre-P2.5
		// behavior preserved per design doc NF6 byte-identical output).
		if !res.Valid() {
			logger.Error("Validation failed: %s", res.Err())
			os.Exit(1)
		}
		cfg := res.Config

		name := agentFlag
		if name == "" {
			name = teamFlag
		}
		if name == "" {
			logger.Error("--agent (or deprecated --team) is required")
			os.Exit(1)
		}
		agent := cfg.AgentByName(name)
		if agent == nil {
			logger.Error("Agent %q not found in config", name)
			os.Exit(1)
		}

		runService := newRunService(workspaceDir, logger)
		state, releaseRunLock, lockErr := runService.SharedSnapshot(context.Background())
		if lockErr != nil {
			logger.Error("Failed to acquire run lock: %s", lockErr)
			os.Exit(1)
		}
		defer releaseRunLock()

		prompt := injection.BuildPrompt(agent, cfg.Name, state, cfg, nil, injection.Capabilities{})

		model := agent.Lead.Model
		if model == "" {
			model = cfg.Defaults.Model
		}

		logger.TeamMsg(agent.Name, "Spawning %s (model: %s)", agent.Lead.Role, model)

		result, err := spawner.Spawn(context.Background(), &spawner.SpawnOpts{
			TeamName:       agent.Name,
			Prompt:         prompt,
			Model:          model,
			MaxTurns:       cfg.Defaults.MaxTurns,
			PermissionMode: cfg.Defaults.PermissionMode,
			TimeoutMinutes: cfg.Defaults.TimeoutMinutes,
		})
		if err != nil {
			logger.Error("Spawn failed: %s", err)
			os.Exit(1)
		}

		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
	},
}

func init() {
	spawnCmd.Flags().StringVar(&agentFlag, "agent", "", "Agent name to spawn (required)")
	spawnCmd.Flags().StringVar(&teamFlag, "team", "", "Deprecated: use --agent")
}
