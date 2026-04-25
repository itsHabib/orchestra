package spawner

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/itsHabib/orchestra/pkg/store"
	"github.com/itsHabib/orchestra/pkg/store/memstore"
)

func TestEnsureAgent_DelegatesToService(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	agents := newFakeAgentAPI()
	sp := newManagedAgentsSpawner(st, agents, newFakeEnvAPI())

	spec := AgentSpec{
		Project:      "chatbot",
		Role:         "backend",
		Model:        "claude-sonnet-4-6",
		SystemPrompt: "You are a backend lead.",
	}
	handle, err := sp.EnsureAgent(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if handle.ID == "" {
		t.Fatal("expected non-empty handle ID")
	}
	if agents.newCalls != 1 {
		t.Fatalf("expected one create via service, got %d", agents.newCalls)
	}
}

func TestEnsureEnvironment_DriftArchivesOldCreatesNew(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	spec := ensureTestEnvSpec()
	key, err := envCacheKey(&spec)
	if err != nil {
		t.Fatal(err)
	}
	oldSpec := withEnvMutation(&spec, func(s *EnvSpec) { s.Packages.Pip = []string{"old"} })
	old := testMAEnv(&oldSpec, key, "env_old")
	if err := st.PutEnv(ctx, key, &store.EnvRecord{
		Key:      key,
		Project:  spec.Project,
		Name:     spec.Name,
		EnvID:    old.ID,
		SpecHash: "sha256:old",
	}); err != nil {
		t.Fatal(err)
	}

	envs := newFakeEnvAPI()
	envs.envs[old.ID] = old
	sp := newManagedAgentsSpawner(st, newFakeAgentAPI(), envs)

	handle, err := sp.EnsureEnvironment(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if envs.archiveCalls != 1 || envs.newCalls != 1 {
		t.Fatalf("expected archive+create, archive=%d new=%d", envs.archiveCalls, envs.newCalls)
	}
	if handle.ID == "env_old" {
		t.Fatalf("expected replacement env, got %+v", handle)
	}
}

func ensureTestEnvSpec() EnvSpec {
	return EnvSpec{
		Project: "chatbot",
		Name:    "default",
		Packages: PackageSpec{
			Pip: []string{"pytest"},
			NPM: []string{"typescript"},
		},
		Networking: NetworkSpec{
			Type:                 "limited",
			AllowedHosts:         []string{"github.com"},
			AllowPackageManagers: true,
		},
	}
}

func withEnvMutation(spec *EnvSpec, fn func(*EnvSpec)) EnvSpec {
	next := *spec
	next.Packages.Pip = append([]string(nil), spec.Packages.Pip...)
	next.Packages.NPM = append([]string(nil), spec.Packages.NPM...)
	next.Networking.AllowedHosts = append([]string(nil), spec.Networking.AllowedHosts...)
	next.Metadata = cloneStringMap(spec.Metadata)
	fn(&next)
	return next
}

func testMAEnv(spec *EnvSpec, key, id string) anthropic.BetaEnvironment {
	return anthropic.BetaEnvironment{
		ID:       id,
		Name:     key,
		Metadata: envMetadata(spec),
		Config: anthropic.BetaCloudConfig{
			Packages: anthropic.BetaPackages{
				Apt:   spec.Packages.APT,
				Cargo: spec.Packages.Cargo,
				Gem:   spec.Packages.Gem,
				Go:    spec.Packages.Go,
				Npm:   spec.Packages.NPM,
				Pip:   spec.Packages.Pip,
			},
			Networking: anthropic.BetaCloudConfigNetworkingUnion{
				Type:                 spec.Networking.Type,
				AllowedHosts:         spec.Networking.AllowedHosts,
				AllowMCPServers:      spec.Networking.AllowMCPServers,
				AllowPackageManagers: spec.Networking.AllowPackageManagers,
			},
		},
		UpdatedAt: "2026-04-19T12:00:00Z",
	}
}

type fakeAgentAPI struct {
	mu          sync.Mutex
	agents      map[string]anthropic.BetaManagedAgentsAgent
	getErr      map[string]error
	listPages   [][]anthropic.BetaManagedAgentsAgent
	getCalls    int
	listCalls   int
	newCalls    int
	updateCalls int
}

func newFakeAgentAPI() *fakeAgentAPI {
	return &fakeAgentAPI{
		agents: make(map[string]anthropic.BetaManagedAgentsAgent),
		getErr: make(map[string]error),
	}
}

//nolint:gocritic // fake API mirrors the SDK method shape used by production code.
func (f *fakeAgentAPI) New(_ context.Context, params anthropic.BetaAgentNewParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCalls++
	id := "agent_created"
	if f.newCalls > 1 {
		id = "agent_created_extra"
	}
	agent := anthropic.BetaManagedAgentsAgent{
		ID:        id,
		Name:      params.Name,
		Model:     anthropic.BetaManagedAgentsModelConfig{ID: params.Model.ID},
		System:    params.System.Value,
		Metadata:  cloneStringMap(params.Metadata),
		Version:   1,
		UpdatedAt: time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC),
	}
	f.agents[id] = agent
	return &agent, nil
}

func (f *fakeAgentAPI) Get(_ context.Context, id string, _ anthropic.BetaAgentGetParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if err := f.getErr[id]; err != nil {
		return nil, err
	}
	agent, ok := f.agents[id]
	if !ok {
		return nil, &anthropic.Error{StatusCode: http.StatusNotFound}
	}
	return &agent, nil
}

//nolint:gocritic // fake API mirrors the SDK method shape used by production code.
func (f *fakeAgentAPI) Update(_ context.Context, id string, params anthropic.BetaAgentUpdateParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	agent := f.agents[id]
	agent.Name = params.Name.Value
	agent.Model = anthropic.BetaManagedAgentsModelConfig{ID: params.Model.ID}
	agent.System = params.System.Value
	agent.Metadata = cloneStringMap(params.Metadata)
	agent.Version = params.Version + 1
	f.agents[id] = agent
	return &agent, nil
}

//nolint:gocritic // fake API mirrors the SDK method shape used by production code.
func (f *fakeAgentAPI) List(_ context.Context, params anthropic.BetaAgentListParams, _ ...option.RequestOption) (*pagination.PageCursor[anthropic.BetaManagedAgentsAgent], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if len(f.listPages) == 0 {
		return &pagination.PageCursor[anthropic.BetaManagedAgentsAgent]{}, nil
	}
	idx := fakePageIndex(params.Page)
	if idx >= len(f.listPages) {
		return &pagination.PageCursor[anthropic.BetaManagedAgentsAgent]{}, nil
	}
	next := ""
	if idx+1 < len(f.listPages) {
		next = fakeCursor(idx + 1)
	}
	return &pagination.PageCursor[anthropic.BetaManagedAgentsAgent]{
		Data:     f.listPages[idx],
		NextPage: next,
	}, nil
}

type fakeEnvAPI struct {
	mu           sync.Mutex
	envs         map[string]anthropic.BetaEnvironment
	listPages    [][]anthropic.BetaEnvironment
	getCalls     int
	listCalls    int
	newCalls     int
	archiveCalls int
}

func newFakeEnvAPI() *fakeEnvAPI {
	return &fakeEnvAPI{envs: make(map[string]anthropic.BetaEnvironment)}
}

//nolint:gocritic // fake API mirrors the SDK method shape used by production code.
func (f *fakeEnvAPI) New(_ context.Context, params anthropic.BetaEnvironmentNewParams, _ ...option.RequestOption) (*anthropic.BetaEnvironment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCalls++
	networkType := ""
	if v := params.Config.Networking.GetType(); v != nil {
		networkType = *v
	}
	allowMCPServers := false
	if v := params.Config.Networking.GetAllowMCPServers(); v != nil {
		allowMCPServers = *v
	}
	allowPackageManagers := false
	if v := params.Config.Networking.GetAllowPackageManagers(); v != nil {
		allowPackageManagers = *v
	}
	env := anthropic.BetaEnvironment{
		ID:       "env_created",
		Name:     params.Name,
		Metadata: cloneStringMap(params.Metadata),
		Config: anthropic.BetaCloudConfig{
			Packages: anthropic.BetaPackages{
				Apt:   params.Config.Packages.Apt,
				Cargo: params.Config.Packages.Cargo,
				Gem:   params.Config.Packages.Gem,
				Go:    params.Config.Packages.Go,
				Npm:   params.Config.Packages.Npm,
				Pip:   params.Config.Packages.Pip,
			},
			Networking: anthropic.BetaCloudConfigNetworkingUnion{
				Type:                 networkType,
				AllowedHosts:         params.Config.Networking.GetAllowedHosts(),
				AllowMCPServers:      allowMCPServers,
				AllowPackageManagers: allowPackageManagers,
			},
		},
		UpdatedAt: "2026-04-19T12:00:00Z",
	}
	f.envs[env.ID] = env
	return &env, nil
}

func (f *fakeEnvAPI) Get(_ context.Context, id string, _ anthropic.BetaEnvironmentGetParams, _ ...option.RequestOption) (*anthropic.BetaEnvironment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	env, ok := f.envs[id]
	if !ok {
		return nil, &anthropic.Error{StatusCode: http.StatusNotFound}
	}
	return &env, nil
}

func (f *fakeEnvAPI) Archive(_ context.Context, id string, _ anthropic.BetaEnvironmentArchiveParams, _ ...option.RequestOption) (*anthropic.BetaEnvironment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archiveCalls++
	env := f.envs[id]
	env.ArchivedAt = "2026-04-19T12:01:00Z"
	f.envs[id] = env
	return &env, nil
}

//nolint:gocritic // fake API mirrors the SDK method shape used by production code.
func (f *fakeEnvAPI) List(_ context.Context, params anthropic.BetaEnvironmentListParams, _ ...option.RequestOption) (*pagination.PageCursor[anthropic.BetaEnvironment], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if len(f.listPages) == 0 {
		return &pagination.PageCursor[anthropic.BetaEnvironment]{}, nil
	}
	idx := fakePageIndex(params.Page)
	if idx >= len(f.listPages) {
		return &pagination.PageCursor[anthropic.BetaEnvironment]{}, nil
	}
	next := ""
	if idx+1 < len(f.listPages) {
		next = fakeCursor(idx + 1)
	}
	return &pagination.PageCursor[anthropic.BetaEnvironment]{
		Data:     f.listPages[idx],
		NextPage: next,
	}, nil
}

// fakeCursor / fakePageIndex translate a page index into the cursor string
// the fake emits as NextPage. Production code threads this cursor back via
// params.Page — without this plumbing a broken cursor would go undetected.
func fakeCursor(idx int) string {
	return fmt.Sprintf("page-%d", idx)
}

func fakePageIndex(page param.Opt[string]) int {
	if !page.Valid() || page.Value == "" {
		return 0
	}
	var idx int
	if _, err := fmt.Sscanf(page.Value, "page-%d", &idx); err != nil {
		return 0
	}
	return idx
}
