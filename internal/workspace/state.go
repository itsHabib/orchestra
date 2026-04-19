package workspace

import "github.com/itsHabib/orchestra/pkg/store"

// State aliases the store run-state document for legacy workspace callers.
type State = store.RunState

// TeamState aliases the store team-state document for legacy workspace callers.
type TeamState = store.TeamState

// RepositoryArtifact aliases the managed-agent repository artifact type.
type RepositoryArtifact = store.RepositoryArtifact

// LockMode aliases the store run-lock mode.
type LockMode = store.LockMode

const (
	// LockExclusive aliases the store exclusive run lock mode.
	LockExclusive = store.LockExclusive

	// LockShared aliases the store shared run lock mode.
	LockShared = store.LockShared
)
