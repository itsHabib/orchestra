package spawner

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/pagination"
	"github.com/itsHabib/orchestra/pkg/store"
	"github.com/itsHabib/orchestra/pkg/store/memstore"
)

func TestSpecHash_Deterministic(t *testing.T) {
	spec := hashTestSpec()
	want := specHash(spec)
	for range 1000 {
		if got := specHash(spec); got != want {
			t.Fatalf("hash changed: got %s want %s", got, want)
		}
	}
}

func TestSpecHash_ChangesOnEachField(t *testing.T) {
	base := hashTestSpec()
	baseHash := specHash(base)

	cases := map[string]AgentSpec{
		"model":         withAgentMutation(base, func(s *AgentSpec) { s.Model = "claude-opus-4-7" }),
		"system":        withAgentMutation(base, func(s *AgentSpec) { s.SystemPrompt = "different" }),
		"tool_name":     withAgentMutation(base, func(s *AgentSpec) { s.Tools[0].Name = "read" }),
		"mcp_url":       withAgentMutation(base, func(s *AgentSpec) { s.MCPServers[0].URL = "https://example.net/mcp" }),
		"skill_version": withAgentMutation(base, func(s *AgentSpec) { s.Skills[0].Version = "2" }),
	}
	for name, spec := range cases {
		if got := specHash(spec); got == baseHash {
			t.Fatalf("%s mutation did not change hash", name)
		}
	}
}

func TestSpecHash_MetadataIgnored(t *testing.T) {
	base := hashTestSpec()
	mutated := base
	mutated.Metadata = map[string]string{"owner": "someone-else"}
	if got, want := specHash(mutated), specHash(base); got != want {
		t.Fatalf("top-level metadata should be ignored: got %s want %s", got, want)
	}
}

func TestSpecHash_SliceOrderMatters(t *testing.T) {
	base := hashTestSpec()
	reordered := base
	reordered.Tools = []Tool{base.Tools[1], base.Tools[0]}
	if got, want := specHash(reordered), specHash(base); got == want {
		t.Fatal("reordered tools should change hash")
	}
}

func TestSpecHash_MapOrderIgnored(t *testing.T) {
	left := AgentSpec{
		Model: "claude-sonnet-4-6",
		Tools: []Tool{{
			Name:        "custom_one",
			Type:        "custom",
			InputSchema: map[string]any{"b": "two", "a": "one"},
		}},
	}
	right := AgentSpec{
		Model: "claude-sonnet-4-6",
		Tools: []Tool{{
			Name:        "custom_one",
			Type:        "custom",
			InputSchema: map[string]any{"a": "one", "b": "two"},
		}},
	}
	if got, want := specHash(left), specHash(right); got != want {
		t.Fatalf("map order should not affect hash: got %s want %s", got, want)
	}
}

func TestSpecHash_NormalizesPrompt(t *testing.T) {
	nfc := AgentSpec{Model: "claude-sonnet-4-6", SystemPrompt: "caf\u00e9\na"}
	nfd := AgentSpec{Model: "claude-sonnet-4-6", SystemPrompt: "cafe\u0301\r\na"}
	if got, want := specHash(nfd), specHash(nfc); got != want {
		t.Fatalf("prompt normalization mismatch: got %s want %s", got, want)
	}
}

func TestSpecHash_GoldenCases(t *testing.T) {
	cases := []struct {
		name string
		spec AgentSpec
		want string
	}{
		{
			name: "basic",
			spec: AgentSpec{Model: "claude-sonnet-4-6", SystemPrompt: "You are precise.\nKeep notes."},
			want: "sha256:4309a9b024856167f836228a9a5caa2bea04c7555c21d7e3d8112f2acb45ed07",
		},
		{
			name: "trailing_spaces",
			spec: AgentSpec{Model: "claude-opus-4-7", SystemPrompt: "Trail spaces matter.  "},
			want: "sha256:42b5197a07965e7f25a2b214a168ae9049bf895b494edd59eb1317d6abb66f16",
		},
		{
			name: "builtin_tool",
			spec: AgentSpec{Model: "claude-sonnet-4-6", Tools: []Tool{{Name: "bash"}}},
			want: "sha256:406e3a42a4c02865fd1e2a79665c9b82ab6b9679e6238c11317e0eedba54e263",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := specHash(tc.spec); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestEnsureAgent_FastPathCacheHit(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	spec := ensureTestAgentSpec()
	key, err := agentCacheKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	oldUsed := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	if err := st.PutAgent(ctx, key, &store.AgentRecord{
		Key:       key,
		Project:   spec.Project,
		Role:      spec.Role,
		AgentID:   "agent_cached",
		Version:   3,
		SpecHash:  specHash(spec),
		UpdatedAt: oldUsed,
		LastUsed:  oldUsed,
	}); err != nil {
		t.Fatal(err)
	}

	agents := newFakeAgentAPI()
	agents.agents["agent_cached"] = testMAAgent(spec, key, "agent_cached", 3)
	now := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)
	sp := newManagedAgentsSpawner(st, agents, newFakeEnvAPI(), withManagedAgentsClock(func() time.Time { return now }))

	handle, err := sp.EnsureAgent(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if handle.ID != "agent_cached" || handle.Version != 3 {
		t.Fatalf("unexpected handle: %+v", handle)
	}
	if agents.getCalls != 1 || agents.listCalls != 0 || agents.newCalls != 0 || agents.updateCalls != 0 {
		t.Fatalf("unexpected api calls: get=%d list=%d new=%d update=%d", agents.getCalls, agents.listCalls, agents.newCalls, agents.updateCalls)
	}
	rec, _, err := st.GetAgent(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.LastUsed.Equal(now) {
		t.Fatalf("LastUsed not touched: got %s want %s", rec.LastUsed, now)
	}
}

func TestEnsureAgent_SpecDriftUpdates(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	spec := ensureTestAgentSpec()
	key, err := agentCacheKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutAgent(ctx, key, &store.AgentRecord{
		Key:      key,
		Project:  spec.Project,
		Role:     spec.Role,
		AgentID:  "agent_cached",
		Version:  1,
		SpecHash: "sha256:old",
	}); err != nil {
		t.Fatal(err)
	}

	agents := newFakeAgentAPI()
	agents.agents["agent_cached"] = testMAAgent(withAgentMutation(spec, func(s *AgentSpec) { s.SystemPrompt = "old" }), key, "agent_cached", 1)
	sp := newManagedAgentsSpawner(st, agents, newFakeEnvAPI())

	handle, err := sp.EnsureAgent(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if handle.Version != 2 {
		t.Fatalf("expected updated version 2, got %d", handle.Version)
	}
	if agents.updateCalls != 1 || agents.newCalls != 0 {
		t.Fatalf("unexpected api calls: update=%d new=%d", agents.updateCalls, agents.newCalls)
	}
	rec, _, err := st.GetAgent(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SpecHash != specHash(spec) || rec.Version != 2 {
		t.Fatalf("cache not updated: %+v", rec)
	}
}

func TestEnsureAgent_404FallsThroughToAdopt(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	spec := ensureTestAgentSpec()
	key, err := agentCacheKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutAgent(ctx, key, &store.AgentRecord{
		Key:      key,
		Project:  spec.Project,
		Role:     spec.Role,
		AgentID:  "agent_stale",
		Version:  1,
		SpecHash: specHash(spec),
	}); err != nil {
		t.Fatal(err)
	}

	agents := newFakeAgentAPI()
	agents.getErr["agent_stale"] = &anthropic.Error{StatusCode: http.StatusNotFound}
	agents.listPages = [][]anthropic.BetaManagedAgentsAgent{{
		testMAAgent(spec, key, "agent_adopted", 1),
	}}
	sp := newManagedAgentsSpawner(st, agents, newFakeEnvAPI())

	handle, err := sp.EnsureAgent(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if handle.ID != "agent_adopted" {
		t.Fatalf("expected adopted agent, got %+v", handle)
	}
	if agents.newCalls != 0 || agents.updateCalls != 0 {
		t.Fatalf("adopt should not create or update: new=%d update=%d", agents.newCalls, agents.updateCalls)
	}
}

func TestEnsureAgent_ZeroMatchesCreates(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	spec := ensureTestAgentSpec()
	agents := newFakeAgentAPI()
	sp := newManagedAgentsSpawner(st, agents, newFakeEnvAPI())

	handle, err := sp.EnsureAgent(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if handle.ID == "" || agents.newCalls != 1 {
		t.Fatalf("expected one create, handle=%+v newCalls=%d", handle, agents.newCalls)
	}
	if agents.listCalls != 1 {
		t.Fatalf("expected one list call, got %d", agents.listCalls)
	}
}

func TestEnsureAgent_SameKeyTwoGoroutinesSingleCreate(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	spec := ensureTestAgentSpec()
	agents := newFakeAgentAPI()
	sp := newManagedAgentsSpawner(st, agents, newFakeEnvAPI())

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := sp.EnsureAgent(ctx, spec)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if agents.newCalls != 1 {
		t.Fatalf("expected one create, got %d", agents.newCalls)
	}
}

func TestEnsureAgent_ListBoundedByMaxPages(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	spec := ensureTestAgentSpec()
	agents := newFakeAgentAPI()
	agents.listPages = [][]anthropic.BetaManagedAgentsAgent{{}, {}, {}, {}}
	sp := newManagedAgentsSpawner(st, agents, newFakeEnvAPI(), WithManagedAgentsConfig(ManagedAgentsConfig{MaxListPages: 3}))

	if _, err := sp.EnsureAgent(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if agents.listCalls != 3 {
		t.Fatalf("expected list to stop at 3 pages, got %d", agents.listCalls)
	}
	if agents.newCalls != 1 {
		t.Fatalf("expected create after bounded scan, got %d", agents.newCalls)
	}
}

func TestEnsureEnvironment_DriftArchivesOldCreatesNew(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	spec := ensureTestEnvSpec()
	key, err := envCacheKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	old := testMAEnv(withEnvMutation(spec, func(s *EnvSpec) { s.Packages.Pip = []string{"old"} }), key, "env_old")
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

func hashTestSpec() AgentSpec {
	return AgentSpec{
		Project:      "chatbot",
		Role:         "backend",
		Model:        "claude-sonnet-4-6",
		SystemPrompt: "You are a backend lead.",
		Tools: []Tool{
			{Name: "bash"},
			{Name: "custom_one", Type: "custom", Description: "do one thing", InputSchema: map[string]any{"type": "object"}},
		},
		MCPServers: []MCPServer{{Name: "docs", Type: "url", URL: "https://example.com/mcp"}},
		Skills:     []Skill{{Name: "skill_123", Version: "1", Metadata: map[string]string{"type": "custom"}}},
		Metadata:   map[string]string{"owner": "ignored"},
	}
}

func ensureTestAgentSpec() AgentSpec {
	return AgentSpec{
		Project:      "chatbot",
		Role:         "backend",
		Model:        "claude-sonnet-4-6",
		SystemPrompt: "You are a backend lead.",
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

func withAgentMutation(spec AgentSpec, fn func(*AgentSpec)) AgentSpec {
	next := spec
	next.Tools = append([]Tool(nil), spec.Tools...)
	next.MCPServers = append([]MCPServer(nil), spec.MCPServers...)
	next.Skills = append([]Skill(nil), spec.Skills...)
	next.Metadata = cloneStringMap(spec.Metadata)
	fn(&next)
	return next
}

func withEnvMutation(spec EnvSpec, fn func(*EnvSpec)) EnvSpec {
	next := spec
	next.Packages.Pip = append([]string(nil), spec.Packages.Pip...)
	next.Packages.NPM = append([]string(nil), spec.Packages.NPM...)
	next.Networking.AllowedHosts = append([]string(nil), spec.Networking.AllowedHosts...)
	next.Metadata = cloneStringMap(spec.Metadata)
	fn(&next)
	return next
}

func testMAAgent(spec AgentSpec, key string, id string, version int64) anthropic.BetaManagedAgentsAgent {
	return anthropic.BetaManagedAgentsAgent{
		ID:        id,
		Name:      key,
		Model:     anthropic.BetaManagedAgentsModelConfig{ID: spec.Model},
		System:    spec.SystemPrompt,
		Metadata:  agentMetadata(&spec),
		Version:   version,
		UpdatedAt: time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC),
	}
}

func testMAEnv(spec EnvSpec, key string, id string) anthropic.BetaEnvironment {
	return anthropic.BetaEnvironment{
		ID:       id,
		Name:     key,
		Metadata: envMetadata(&spec),
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
func (f *fakeAgentAPI) List(_ context.Context, _ anthropic.BetaAgentListParams, _ ...option.RequestOption) (*pagination.PageCursor[anthropic.BetaManagedAgentsAgent], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	idx := f.listCalls - 1
	if len(f.listPages) == 0 {
		return &pagination.PageCursor[anthropic.BetaManagedAgentsAgent]{}, nil
	}
	if idx >= len(f.listPages) {
		idx = len(f.listPages) - 1
	}
	next := ""
	if f.listCalls < len(f.listPages) {
		next = "next"
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
func (f *fakeEnvAPI) List(_ context.Context, _ anthropic.BetaEnvironmentListParams, _ ...option.RequestOption) (*pagination.PageCursor[anthropic.BetaEnvironment], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	idx := f.listCalls - 1
	if len(f.listPages) == 0 {
		return &pagination.PageCursor[anthropic.BetaEnvironment]{}, nil
	}
	if idx >= len(f.listPages) {
		idx = len(f.listPages) - 1
	}
	next := ""
	if f.listCalls < len(f.listPages) {
		next = "next"
	}
	return &pagination.PageCursor[anthropic.BetaEnvironment]{
		Data:     f.listPages[idx],
		NextPage: next,
	}, nil
}
