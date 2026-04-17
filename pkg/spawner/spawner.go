package spawner

import (
	"context"
	"errors"
	"io"
	"time"
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
	// Session.Send.
	StartSession(ctx context.Context, req StartSessionRequest) (Session, error)

	// ResumeSession reconnects to an existing backend session by ID.
	ResumeSession(ctx context.Context, sessionID string) (Session, error)
}

// AgentSpec describes the backend agent resource to create or reuse.
type AgentSpec struct {
	Name         string
	Model        string
	SystemPrompt string
	Tools        []Tool
	MCPServers   []MCPServer
	Skills       []Skill
	Metadata     map[string]string
}

// Tool describes a backend-native tool made available to an agent.
type Tool struct {
	Name        string
	Type        string
	Description string
	InputSchema any
	Metadata    map[string]string
}

// MCPServer describes an MCP server attached to an agent.
type MCPServer struct {
	Name     string
	Type     string
	URL      string
	Metadata map[string]string
}

// Skill describes a backend-native skill attached to an agent.
type Skill struct {
	Name     string
	Version  string
	Metadata map[string]string
}

// EnvSpec describes a backend environment resource.
type EnvSpec struct {
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
type AgentHandle struct {
	ID       string
	Backend  string
	Name     string
	Version  int
	Model    string
	Metadata map[string]string
}

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

// Session is a backend runtime for a single agent invocation.
type Session interface {
	ID() string
	Status(ctx context.Context) (SessionStatus, error)
	Usage(ctx context.Context) (Usage, error)

	Send(ctx context.Context, event UserEvent) error
	Events(ctx context.Context) (<-chan Event, error)
	History(ctx context.Context, after EventID) ([]Event, error)

	ListProducedFiles(ctx context.Context) ([]FileRef, error)
	DownloadFile(ctx context.Context, ref FileRef, w io.Writer) error

	Interrupt(ctx context.Context) error
	Cancel(ctx context.Context) error
}

// SessionState is the backend-visible lifecycle state of a session.
type SessionState string

const (
	SessionStateIdle         SessionState = "idle"
	SessionStateRunning      SessionState = "running"
	SessionStateRescheduling SessionState = "rescheduling"
	SessionStateTerminated   SessionState = "terminated"
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
