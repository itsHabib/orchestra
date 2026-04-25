package workspace

import "github.com/itsHabib/orchestra/internal/store"

// State aliases the store run-state document for legacy workspace callers.
type State = store.RunState

// TeamState aliases the store team-state document for legacy workspace callers.
type TeamState = store.TeamState

// RepositoryArtifact aliases the managed-agent repository artifact type.
type RepositoryArtifact = store.RepositoryArtifact
