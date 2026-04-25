package filestore

import (
	"context"

	"github.com/itsHabib/orchestra/internal/store"
)

// ReadActiveRunState reads state.json for the workspace without acquiring the
// run lock. The file is atomically written by SaveRunState (write-temp +
// os.Rename), so a snapshot is always consistent; the data may be stale but is
// never torn. Returns store.ErrNotFound when no active run exists.
//
// Use this from out-of-process callers (CLI commands invoked separately from
// `orchestra run`). Callers that need a coherent multi-step view should
// continue to acquire the run lock and use LoadRunState on a held FileStore.
func ReadActiveRunState(ctx context.Context, workspaceDir string) (*store.RunState, error) {
	return New(workspaceDir).LoadRunState(ctx)
}
