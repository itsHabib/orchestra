package cmd

import (
	"context"
	"fmt"

	"github.com/itsHabib/orchestra/internal/injection"
	"github.com/itsHabib/orchestra/pkg/spawner"
)

func (r *orchestrationRun) startCoordinator(ctx context.Context, tiers [][]string) (*spawner.CoordinatorHandle, func(), error) {
	if !r.cfg.Coordinator.Enabled {
		return nil, nil, nil
	}

	coordPrompt := injection.BuildCoordinatorPrompt(r.cfg, tiers, r.bus.Path(), r.participants)
	coordLogWriter, err := r.ws.LogWriter("coordinator")
	if err != nil {
		return nil, nil, fmt.Errorf("opening coordinator log: %w", err)
	}

	coordHandle, err := spawner.SpawnBackground(ctx, &spawner.SpawnOpts{
		TeamName:       "coordinator",
		Prompt:         coordPrompt,
		Model:          r.cfg.Coordinator.Model,
		MaxTurns:       r.cfg.Coordinator.MaxTurns,
		PermissionMode: r.cfg.Defaults.PermissionMode,
		TimeoutMinutes: r.cfg.Defaults.TimeoutMinutes * len(tiers),
		LogWriter:      coordLogWriter,
		ProgressFunc:   func(team, msg string) { r.logger.TeamMsg(team, "%s", msg) },
	})
	if err != nil {
		_ = coordLogWriter.Close()
		r.logger.Warn("Coordinator spawn failed (continuing without): %s", err)
		return nil, nil, nil
	}

	r.logger.Success("Coordinator agent spawned")
	return coordHandle, func() { _ = coordLogWriter.Close() }, nil
}

func (r *orchestrationRun) stopCoordinator(coordHandle *spawner.CoordinatorHandle) error {
	if coordHandle == nil {
		return nil
	}

	r.logger.Info("Signaling coordinator to stop...")
	coordHandle.Cancel()
	coordResult, coordErr := coordHandle.Wait()
	if coordErr != nil {
		r.logger.Warn("Coordinator exited with error: %s", coordErr)
		return nil
	}
	if coordResult == nil {
		return nil
	}
	r.logger.TeamMsg("coordinator", "Done (cost: $%.2f, turns: %d)", coordResult.CostUSD, coordResult.NumTurns)
	if err := r.ws.WriteResult(coordResult); err != nil {
		return fmt.Errorf("writing coordinator result: %w", err)
	}
	return nil
}
