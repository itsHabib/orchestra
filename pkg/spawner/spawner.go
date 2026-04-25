package spawner

import (
	"context"
	"errors"
	"io"
	"time"

	agentservice "github.com/itsHabib/orchestra/internal/agents"
	"github.com/itsHabib/orchestra/pkg/store"
)

// ErrUnsupported is returned when a backend cannot support a spawner operation.
var ErrUnsupported = errors.New("spawner operation unsupported")

// Spawner creates and manages backend-specific agent runtimes.
type Spawner interface {
	// EnsureAgent creates or reuses a backend agent. For local subprocess
	// execution this returns a synthetic handle.
	EnsureAgent(ctx context.Context, spec AgentSpec) (AgentHandle, error)

	// EnsureEnvironment creates or reuses a container/env template. For local
	// subprocess execution this returns a synthetic handle.
	EnsureEnvironment(ctx context.Context, spec EnvSpec) (EnvHandle, error)

	// StartSession starts a new session. Initial prompt input is sent via
	// PendingSession.Stream followed by Session.Send.
	StartSession(ctx context.Context, req StartSessionRequest) (*PendingSession, error)

	// ResumeSession reconnects to an existing backend session by ID.
	ResumeSession(ctx context.Context, sessionID string) (*Session, error)
}

// AgentSpec describes the backend agent resource to create or reuse.
type AgentSpec = agentservice.AgentSpec

// Tool describes a backend-native tool made available to an agent.
type Tool = agentservice.Tool

// MCPServer describes an MCP server attached to an agent.
type MCPServer = agentservice.MCPServer

// Skill describes an Anthropic-hosted skill attached to an agent.
type Skill = agentservice.Skill

// EnvSpec describes a backend environment resource.
type EnvSpec struct {
	Project    string
	Name       string
	Packages   PackageSpec
	Networking NetworkSpec
	Metadata   map[string]string
}

// PackageSpec describes packages requested for a backend environment.
type PackageSpec struct {
	APT   []string
	Cargo []string
	Gem   []string
	Go    []string
	NPM   []string
	Pip   []string
}

// NetworkSpec describes network access for a backend environment.
type NetworkSpec struct {
	Type                 string
	AllowedHosts         []string
	AllowMCPServers      bool
	AllowPackageManagers bool
}

// AgentHandle identifies a backend agent resource.
type AgentHandle = agentservice.AgentHandle

// EnvHandle identifies a backend environment resource.
type EnvHandle struct {
	ID       string
	Backend  string
	Name     string
	Metadata map[string]string
}

// StartSessionRequest describes a backend session to start.
type StartSessionRequest struct {
	Agent     AgentHandle
	Env       EnvHandle
	VaultIDs  []string
	Resources []ResourceRef
	Metadata  map[string]string

	// The remaining fields are host-side lifecycle sinks used by the managed
	// agents translator. They are intentionally not sent to the backend.
	TeamName      string
	LogWriter     io.Writer
	Store         store.Store
	SummaryWriter func(teamName, text string) error
}

// ResourceRef describes a file or repository resource attached to a session.
type ResourceRef struct {
	Type string // "file" | "github_repository"

	// Type == "file"
	FileID string

	// Type == "github_repository"
	URL                string
	Checkout           *RepoCheckout
	AuthorizationToken string // in-memory only; never persisted to config or state

	MountPath string
}

// RepoCheckout describes the repository revision to mount.
type RepoCheckout struct {
	Type string // "branch" | "commit"
	Name string // branch name, when Type == "branch"
	SHA  string // commit SHA, when Type == "commit"
}

// SessionState is the backend-visible lifecycle state of a session.
type SessionState string

const (
	// SessionStateIdle indicates the session is waiting for input or finished a turn.
	SessionStateIdle SessionState = "idle"
	// SessionStateRunning indicates the session is processing work.
	SessionStateRunning SessionState = "running"
	// SessionStateRescheduling indicates the backend is moving the session.
	SessionStateRescheduling SessionState = "rescheduling"
	// SessionStateTerminated indicates the session has ended.
	SessionStateTerminated SessionState = "terminated"
)

// StopReason explains why a session became idle or terminated.
type StopReason struct {
	Type     string
	Message  string
	EventIDs []EventID
}

// SessionStatus is a snapshot of backend session state.
type SessionStatus struct {
	ID         string
	State      SessionState
	StopReason StopReason
	UpdatedAt  time.Time
	Message    string
}

// Usage captures token and cost counters reported by a backend session.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	CostUSD                  float64
}

// FileRef identifies a file produced by a session.
type FileRef struct {
	ID        string
	Name      string
	Path      string
	MIMEType  string
	SizeBytes int64
	CreatedAt time.Time
	Metadata  map[string]string
}
