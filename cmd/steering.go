package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
)

// steeringSessionEventsFactory is the seam tests use to inject a fake events
// client. Production callers leave it pointing at spawner.SessionEventsClient,
// which constructs an SDK-backed client via machost.NewClient.
var steeringSessionEventsFactory = spawner.SessionEventsClient

// loadActiveRunState reads state.json without acquiring the run lock and
// remaps the store-level not-found sentinel into the steering-flavored
// "no active run in <workspace>" error so all three CLI commands surface the
// same message.
func loadActiveRunState(ctx context.Context, workspace string) (*store.RunState, error) {
	state, err := filestore.ReadActiveRunState(ctx, workspace)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("%w in %s", spawner.ErrNoActiveRun, workspace)
	case err != nil:
		return nil, err
	}
	return state, nil
}

// resolveSteerableTeam runs the lookup chain: load run state, then defer to
// spawner.SteerableSessionID for the backend / team / status / session-id
// gates. internal/mcp/tools.go calls SteerableSessionID directly off a state
// it loaded itself; this CLI helper exists because the CLI's loadActive-
// RunState wraps the not-found sentinel into a user-facing message before
// the gate runs.
//
// The status check is best-effort under TOCTOU: the team may transition
// between this read and the MA send. Send-time errors surface MA's actual
// response so the user can react.
func resolveSteerableTeam(ctx context.Context, workspace, team string) (string, error) {
	if team == "" {
		return "", fmt.Errorf("%w: --team is required", store.ErrInvalidArgument)
	}
	state, err := loadActiveRunState(ctx, workspace)
	if err != nil {
		return "", err
	}
	return spawner.SteerableSessionID(state, team)
}
