package agents

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"
	"github.com/itsHabib/orchestra/pkg/store"
)

const (
	managedAgentsBackend = "managed_agents"

	orchestraMetadataProject = "orchestra_project"
	orchestraMetadataRole    = "orchestra_role"
	orchestraMetadataVersion = "orchestra_version"
	orchestraVersionV2       = "v2"

	cacheKeySeparator = "__"

	defaultMaxListPages     = 10
	defaultAgentLockTimeout = 90 * time.Second
	defaultListWorkers      = 5
	defaultListPageLimit    = 100
)

// MAClient is the subset of the Anthropic Managed Agents API this service uses.
type MAClient interface {
	New(context.Context, anthropic.BetaAgentNewParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error)
	Get(context.Context, string, anthropic.BetaAgentGetParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error)
	Update(context.Context, string, anthropic.BetaAgentUpdateParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error)
	List(context.Context, anthropic.BetaAgentListParams, ...option.RequestOption) (*pagination.PageCursor[anthropic.BetaManagedAgentsAgent], error)
}

// Config controls Managed Agents cache and API scan behavior.
type Config struct {
	MaxListPages     int
	AgentLockTimeout time.Duration
	ListWorkers      int
	ListPageLimit    int
}

// Option customizes a Service.
type Option func(*Service)

// Clock returns the current time. It is replaceable in tests.
type Clock func() time.Time

// Service owns Managed Agents plus local agent-cache choreography.
type Service struct {
	store  store.Store
	ma     MAClient
	logger *slog.Logger
	cfg    Config
	clock  Clock
}

// Summary is one cached agent record annotated with live Managed Agents status.
type Summary struct {
	Record store.AgentRecord
	Status Status
	Err    error
}

// Status is the live Managed Agents state of an agent ID.
type Status string

const (
	// StatusActive means the Managed Agent exists and is not archived.
	StatusActive Status = "active"
	// StatusArchived means the Managed Agent exists but is archived.
	StatusArchived Status = "archived"
	// StatusMissing means the cached Managed Agent ID returned 404.
	StatusMissing Status = "missing"
	// StatusUnreachable means the Managed Agents API lookup failed.
	StatusUnreachable Status = "unreachable"
)

// Orphan is an orchestra-tagged Managed Agent not known to the caller.
type Orphan struct {
	Key     string
	AgentID string
	Version int
	Status  Status
}

// PruneOpts controls stale cache deletion.
type PruneOpts struct {
	Apply   bool
	MaxAge  time.Duration
	Protect func(key, agentID string) bool
	// Now fixes the reference time used for staleness selection and for
	// PruneReport.Now. Zero falls back to the service clock.
	Now time.Time
}

// PruneReport describes a prune evaluation and any applied deletions.
type PruneReport struct {
	Considered []store.AgentRecord
	Stale      []Summary
	Deleted    []string
	Now        time.Time
	MaxAge     time.Duration
}

// AgentSpec describes the backend agent resource to create or reuse.
type AgentSpec struct {
	Project      string
	Role         string
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

// AgentHandle identifies a backend agent resource.
type AgentHandle struct {
	ID       string
	Backend  string
	Name     string
	Version  int
	Model    string
	Metadata map[string]string
}

// WithLogger sets the logger used for cache decisions.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Service) {
		s.logger = logger
	}
}

// WithConfig overrides Managed Agents cache defaults.
func WithConfig(cfg Config) Option {
	return func(s *Service) {
		s.cfg = cfg
	}
}

// WithClock sets the clock used for cache timestamps.
func WithClock(clock Clock) Option {
	return func(s *Service) {
		s.clock = clock
	}
}

// WithWorkers overrides the default status-list worker count.
func WithWorkers(workers int) Option {
	return func(s *Service) {
		s.cfg.ListWorkers = workers
	}
}

func defaultLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
