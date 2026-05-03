package workspace

import "github.com/itsHabib/orchestra/internal/store"

// State aliases the store run-state document for legacy workspace callers.
type State = store.RunState

// AgentState aliases the store agent-state document for legacy workspace callers.
type AgentState = store.AgentState

// RepositoryArtifact aliases the managed-agent repository artifact type.
type RepositoryArtifact = store.RepositoryArtifact
