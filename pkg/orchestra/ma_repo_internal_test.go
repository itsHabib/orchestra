package orchestra

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/event"
	"github.com/itsHabib/orchestra/internal/ghhost"
	runsvc "github.com/itsHabib/orchestra/internal/run"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/memstore"
	"github.com/itsHabib/orchestra/internal/workspace"
)

const repoURL = "https://github.com/octo/repo"

func newRepoCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Name: "p",
		Backend: config.Backend{
			Kind: "managed_agents",
			ManagedAgents: &config.ManagedAgentsBackend{
				Repository: &config.RepositorySpec{URL: repoURL},
			},
		},
		Agents: []config.Agent{
			{Name: "alpha", Lead: config.Lead{Role: "A"}, Tasks: []config.Task{{Summary: "x", Details: "d", Verify: "v"}}},
			{Name: "beta", Lead: config.Lead{Role: "B"}, DependsOn: []string{"alpha"}, Tasks: []config.Task{{Summary: "y", Details: "d", Verify: "v"}}},
		},
	}
	cfg.ResolveDefaults()
	return cfg
}

func TestBuildSessionResources_TextOnlyTeamReturnsNil(t *testing.T) {
	r := &orchestrationRun{cfg: &config.Config{Name: "p", Backend: config.Backend{Kind: "managed_agents"}}}
	team := &config.Agent{Name: "alpha"}
	got, err := r.buildSessionResources(team, &store.RunState{})
	if err != nil || got != nil {
		t.Fatalf("text-only team should return (nil, nil), got %v / %v", got, err)
	}
}

func TestBuildSessionResources_Tier0SingleResource(t *testing.T) {
	cfg := newRepoCfg(t)
	r := &orchestrationRun{cfg: cfg, ghPAT: "secret"}

	got, err := r.buildSessionResources(&cfg.Agents[0], &store.RunState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d resources, want 1: %+v", len(got), got)
	}
	rr := got[0]
	if rr.Type != "github_repository" || rr.URL != repoURL || rr.MountPath != config.DefaultRepoMountPath || rr.AuthorizationToken != "secret" {
		t.Fatalf("tier-0 resource shape: %+v", rr)
	}
	if rr.Checkout == nil || rr.Checkout.Type != "branch" || rr.Checkout.Name != config.DefaultRepoDefaultBranch {
		t.Fatalf("tier-0 checkout: %+v", rr.Checkout)
	}
}

func TestBuildSessionResources_TierNFanIn(t *testing.T) {
	cfg := newRepoCfg(t)
	// Add a third team that depends on both alpha and beta.
	cfg.Agents = append(cfg.Agents, config.Agent{
		Name:      "gamma",
		Lead:      config.Lead{Role: "G"},
		DependsOn: []string{"alpha", "beta"},
		Tasks:     []config.Task{{Summary: "z", Details: "d", Verify: "v"}},
	})
	state := &store.RunState{
		Agents: map[string]store.AgentState{
			"alpha": {RepositoryArtifacts: []store.RepositoryArtifact{{URL: repoURL, Branch: "orchestra/alpha-r"}}},
			"beta":  {RepositoryArtifacts: []store.RepositoryArtifact{{URL: repoURL, Branch: "orchestra/beta-r"}}},
		},
	}
	r := &orchestrationRun{cfg: cfg, ghPAT: "secret"}

	got, err := r.buildSessionResources(&cfg.Agents[2], state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d resources, want 3 (working copy + 2 upstreams): %+v", len(got), got)
	}
	if got[1].MountPath != "/workspace/upstream/alpha" || got[1].Checkout.Name != "orchestra/alpha-r" {
		t.Fatalf("upstream alpha resource: %+v", got[1])
	}
	if got[2].MountPath != "/workspace/upstream/beta" || got[2].Checkout.Name != "orchestra/beta-r" {
		t.Fatalf("upstream beta resource: %+v", got[2])
	}
}

func TestBuildSessionResources_SkipsUpstreamWithoutArtifact(t *testing.T) {
	cfg := newRepoCfg(t)
	state := &store.RunState{
		Agents: map[string]store.AgentState{
			"alpha": {Status: "done"}, // no RepositoryArtifacts recorded — skip
		},
	}
	r := &orchestrationRun{cfg: cfg, ghPAT: "secret"}
	got, err := r.buildSessionResources(&cfg.Agents[1], state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d resources, want 1 (only own working copy): %+v", len(got), got)
	}
}

func TestBuildSessionResources_PATMissingErrors(t *testing.T) {
	cfg := newRepoCfg(t)
	r := &orchestrationRun{cfg: cfg} // ghPAT empty
	if _, err := r.buildSessionResources(&cfg.Agents[0], &store.RunState{}); err == nil {
		t.Fatal("expected error when PAT is unavailable")
	}
}

func TestArtifactPublishSpec_BuildsBranchAndUpstreams(t *testing.T) {
	cfg := newRepoCfg(t)
	state := &store.RunState{
		RunID: "run-42",
		Agents: map[string]store.AgentState{
			"alpha": {RepositoryArtifacts: []store.RepositoryArtifact{{Branch: "orchestra/alpha-run-42"}}},
		},
	}
	r := &orchestrationRun{cfg: cfg, ghPAT: "secret"}
	spec := r.artifactPublishSpec(&cfg.Agents[1], state)

	if spec == nil {
		t.Fatal("expected non-nil spec")
	}
	if spec.MountPath != config.DefaultRepoMountPath {
		t.Fatalf("MountPath %q", spec.MountPath)
	}
	if spec.BranchName != "orchestra/beta-run-42" {
		t.Fatalf("BranchName %q", spec.BranchName)
	}
	if len(spec.UpstreamMounts) != 1 || spec.UpstreamMounts[0].Branch != "orchestra/alpha-run-42" {
		t.Fatalf("UpstreamMounts: %+v", spec.UpstreamMounts)
	}
}

func TestArtifactPublishSpec_NilWhenNoRepo(t *testing.T) {
	r := &orchestrationRun{cfg: &config.Config{Name: "p", Backend: config.Backend{Kind: "managed_agents"}}}
	spec := r.artifactPublishSpec(&config.Agent{Name: "alpha"}, &store.RunState{RunID: "r"})
	if spec != nil {
		t.Fatalf("expected nil for text-only team, got %+v", spec)
	}
}

func TestResolveTeamArtifact_RecordsBranch(t *testing.T) {
	srv := httptest.NewServer(escapedPathRouter(t, map[string]http.HandlerFunc{
		"/repos/octo/repo/branches/orchestra%2Falpha-run-x": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"name":"orchestra/alpha-run-x","commit":{"sha":"head1"}}`))
		},
		"/repos/octo/repo/compare/main...orchestra%2Falpha-run-x": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"merge_base_commit":{"sha":"baseSha"}}`))
		},
	}))
	defer srv.Close()

	r := newRepoTestRun(t, "run-x", srv)
	if err := r.resolveTeamArtifact(context.Background(), &r.cfg.Agents[0]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	state, err := r.runService.Store().LoadRunState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	arts := state.Agents["alpha"].RepositoryArtifacts
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(arts))
	}
	if arts[0].CommitSHA != "head1" || arts[0].BaseSHA != "baseSha" || arts[0].Branch != "orchestra/alpha-run-x" {
		t.Fatalf("artifact %+v", arts[0])
	}
}

func TestResolveTeamArtifact_BranchNotFoundMarksFailed(t *testing.T) {
	srv := httptest.NewServer(escapedPathRouter(t, map[string]http.HandlerFunc{
		"/repos/octo/repo/branches/orchestra%2Falpha-run-x": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Branch not found"}`))
		},
	}))
	defer srv.Close()

	r := newRepoTestRun(t, "run-x", srv)
	if err := r.resolveTeamArtifact(context.Background(), &r.cfg.Agents[0]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	state, err := r.runService.Store().LoadRunState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ts := state.Agents["alpha"]
	if ts.Status != "failed" || !strings.Contains(ts.LastError, "no branch pushed") {
		t.Fatalf("expected failed/no-branch state, got %+v", ts)
	}
	if len(ts.RepositoryArtifacts) != 0 {
		t.Fatalf("no artifact should be recorded, got %+v", ts.RepositoryArtifacts)
	}
}

func TestResolveTeamArtifact_OpenPRPopulatesURL(t *testing.T) {
	srv := httptest.NewServer(escapedPathRouter(t, map[string]http.HandlerFunc{
		"/repos/octo/repo/branches/orchestra%2Falpha-run-x": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"name":"orchestra/alpha-run-x","commit":{"sha":"sha-head"}}`))
		},
		"/repos/octo/repo/compare/main...orchestra%2Falpha-run-x": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"merge_base_commit":{"sha":"sha-base"}}`))
		},
		"/repos/octo/repo/pulls": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/octo/repo/pull/9","number":9}`))
		},
	}))
	defer srv.Close()

	r := newRepoTestRun(t, "run-x", srv)
	r.cfg.Backend.ManagedAgents.OpenPullRequests = true
	if err := r.resolveTeamArtifact(context.Background(), &r.cfg.Agents[0]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	state, err := r.runService.Store().LoadRunState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	arts := state.Agents["alpha"].RepositoryArtifacts
	if len(arts) != 1 || arts[0].PullRequestURL != "https://github.com/octo/repo/pull/9" {
		t.Fatalf("expected PR url recorded, got %+v", arts)
	}
}

func TestResolveTeamArtifact_SkipsWhenClientNil(t *testing.T) {
	cfg := newRepoCfg(t)
	st := memstore.New()
	if err := st.SaveRunState(context.Background(), &store.RunState{RunID: "r", Agents: map[string]store.AgentState{"alpha": {}}}); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatal(err)
	}
	r := &orchestrationRun{cfg: cfg, runService: runsvc.New(st), ws: ws, emitter: event.NoopEmitter{}}
	if err := r.resolveTeamArtifact(context.Background(), &cfg.Agents[0]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// escapedPathRouter dispatches on r.URL.EscapedPath() (Go 1.22+ ServeMux
// matches on the escaped path, so branch names that contain "/" arrive as
// "%2F" path segments — using a literal-pattern mux makes routing fail).
func escapedPathRouter(t *testing.T, routes map[string]http.HandlerFunc) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := routes[r.URL.EscapedPath()]; ok {
			h(w, r)
			return
		}
		t.Logf("unrouted %s %s (escaped=%q)", r.Method, r.URL.RequestURI(), r.URL.EscapedPath())
		http.NotFound(w, r)
	})
}

func newRepoTestRun(t *testing.T, runID string, srv *httptest.Server) *orchestrationRun {
	t.Helper()
	cfg := newRepoCfg(t)
	st := memstore.New()
	teams := make(map[string]store.AgentState)
	for i := range cfg.Agents {
		teams[cfg.Agents[i].Name] = store.AgentState{}
	}
	if err := st.SaveRunState(context.Background(), &store.RunState{RunID: runID, Agents: teams}); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Ensure(filepath.Join(t.TempDir(), ".orchestra"))
	if err != nil {
		t.Fatal(err)
	}
	pat := "test-pat"
	return &orchestrationRun{
		cfg:        cfg,
		ghClient:   ghhost.New(pat, ghhost.WithBase(srv.URL), ghhost.WithHTTPClient(srv.Client())),
		ghPAT:      pat,
		runService: runsvc.New(st),
		ws:         ws,
		emitter:    event.NoopEmitter{},
	}
}
