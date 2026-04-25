package agents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
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

func TestEnsureAgent_CreatesOnceForConcurrentCalls(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	ma := newFakeMAClient()
	svc := New(st, ma)
	spec := testAgentSpec()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.EnsureAgent(ctx, spec)
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
	if ma.newCalls != 1 {
		t.Fatalf("newCalls=%d, want 1", ma.newCalls)
	}
}

func TestGet_ClassifiesStatuses(t *testing.T) {
	ctx := context.Background()
	spec := testAgentSpec()
	active := testAgent(&spec, "p__backend", "agent-active", 1)
	archived := testAgent(&spec, "p__backend", "agent-archived", 1)
	archived.ArchivedAt = time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	transportErr := errors.New("network down")

	ma := newFakeMAClient()
	ma.agents[active.ID] = active
	ma.agents[archived.ID] = archived
	ma.getErr["agent-missing"] = &anthropic.Error{StatusCode: http.StatusNotFound}
	ma.getErr["agent-unreachable"] = transportErr
	svc := New(memstore.New(), ma)

	cases := []struct {
		id      string
		want    Status
		wantErr bool
	}{
		{"agent-active", StatusActive, false},
		{"agent-archived", StatusArchived, false},
		{"agent-missing", StatusMissing, false},
		{"agent-unreachable", StatusUnreachable, true},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got, err := svc.Get(ctx, tc.id)
			if got != tc.want {
				t.Fatalf("status=%s, want %s", got, tc.want)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestList_PreservesRecordOrderAndErrors(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	ma := newFakeMAClient()
	spec := testAgentSpec()
	for _, rec := range []store.AgentRecord{
		{Key: "p__a", Project: "p", Role: "a", AgentID: "agent-a"},
		{Key: "p__b", Project: "p", Role: "b", AgentID: "agent-b"},
		{Key: "p__c", Project: "p", Role: "c", AgentID: "agent-c"},
	} {
		if err := st.PutAgent(ctx, rec.Key, &rec); err != nil {
			t.Fatal(err)
		}
		agent := testAgent(&spec, rec.Key, rec.AgentID, 1)
		agent.Metadata[orchestraMetadataRole] = rec.Role
		ma.agents[agent.ID] = agent
	}
	ma.getErr["agent-b"] = errors.New("temporary outage")

	rows, err := New(st, ma, WithWorkers(2)).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotKeys := []string{rows[0].Record.Key, rows[1].Record.Key, rows[2].Record.Key}
	if want := []string{"p__a", "p__b", "p__c"}; !reflect.DeepEqual(gotKeys, want) {
		t.Fatalf("keys=%v, want %v", gotKeys, want)
	}
	if rows[1].Status != StatusUnreachable || rows[1].Err == nil {
		t.Fatalf("row[1]=%+v, want unreachable with preserved error", rows[1])
	}
}

func TestPrune_DryRunApplyAndProtect(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC)
	st := memstore.New()
	ma := newFakeMAClient()
	spec := testAgentSpec()
	records := []store.AgentRecord{
		{Key: "p__old", Project: "p", Role: "old", AgentID: "agent-old", LastUsed: now.Add(-60 * 24 * time.Hour)},
		{Key: "p__archived", Project: "p", Role: "archived", AgentID: "agent-archived", LastUsed: now},
		{Key: "p__missing", Project: "p", Role: "missing", AgentID: "agent-missing", LastUsed: now},
		{Key: "p__protected", Project: "p", Role: "protected", AgentID: "agent-protected", LastUsed: now.Add(-60 * 24 * time.Hour)},
	}
	for _, rec := range records {
		if err := st.PutAgent(ctx, rec.Key, &rec); err != nil {
			t.Fatal(err)
		}
		agent := testAgent(&spec, rec.Key, rec.AgentID, 1)
		agent.Metadata[orchestraMetadataRole] = rec.Role
		ma.agents[agent.ID] = agent
	}
	archived := ma.agents["agent-archived"]
	archived.ArchivedAt = now
	ma.agents["agent-archived"] = archived
	ma.getErr["agent-missing"] = &anthropic.Error{StatusCode: http.StatusNotFound}

	svc := New(st, ma, WithClock(func() time.Time { return now }))
	opts := PruneOpts{
		MaxAge: 30 * 24 * time.Hour,
		Protect: func(_ string, agentID string) bool {
			return agentID == "agent-protected"
		},
	}
	report, err := svc.Prune(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := staleKeys(report.Stale); !reflect.DeepEqual(got, []string{"p__archived", "p__missing", "p__old"}) {
		t.Fatalf("dry-run stale=%v", got)
	}
	if _, ok, _ := st.GetAgent(ctx, "p__old"); !ok {
		t.Fatal("dry-run deleted p__old")
	}

	opts.Apply = true
	report, err = svc.Prune(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Deleted; !reflect.DeepEqual(got, []string{"p__archived", "p__missing", "p__old"}) {
		t.Fatalf("deleted=%v", got)
	}
	if _, ok, _ := st.GetAgent(ctx, "p__protected"); !ok {
		t.Fatal("protected record was deleted")
	}
}

func TestOrphans_PaginatesAndExcludes(t *testing.T) {
	ctx := context.Background()
	ma := newFakeMAClient()
	alpha := testAgent(&AgentSpec{Project: "p", Role: "alpha", Model: "claude-sonnet-4-6"}, "p__alpha", "agent-alpha", 1)
	beta := testAgent(&AgentSpec{Project: "p", Role: "beta", Model: "claude-sonnet-4-6"}, "p__beta", "agent-beta", 2)
	beta.ArchivedAt = time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	untagged := testAgent(&AgentSpec{Project: "p", Role: "gamma", Model: "claude-sonnet-4-6"}, "p__gamma", "agent-gamma", 3)
	untagged.Metadata = nil
	ma.listPages = [][]anthropic.BetaManagedAgentsAgent{
		{beta, untagged},
		{alpha},
	}

	orphaned, err := New(memstore.New(), ma).Orphans(ctx, func(_ string, agentID string) bool {
		return agentID == "agent-alpha"
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) != 1 {
		t.Fatalf("len(orphaned)=%d, want 1: %+v", len(orphaned), orphaned)
	}
	if orphaned[0].Key != "p__beta" || orphaned[0].Status != StatusArchived {
		t.Fatalf("orphan=%+v, want archived beta", orphaned[0])
	}
	if ma.listCalls != 2 {
		t.Fatalf("listCalls=%d, want 2", ma.listCalls)
	}
}

func testAgentSpec() AgentSpec {
	return AgentSpec{
		Project:      "p",
		Role:         "backend",
		Model:        "claude-sonnet-4-6",
		SystemPrompt: "You are a backend lead.",
	}
}

func testAgent(spec *AgentSpec, key, id string, version int64) anthropic.BetaManagedAgentsAgent {
	return anthropic.BetaManagedAgentsAgent{
		ID:        id,
		Name:      key,
		Model:     anthropic.BetaManagedAgentsModelConfig{ID: spec.Model},
		System:    spec.SystemPrompt,
		Metadata:  agentMetadata(spec),
		Version:   version,
		UpdatedAt: time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC),
	}
}

func staleKeys(rows []Summary) []string {
	out := make([]string, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].Record.Key)
	}
	return out
}

type fakeMAClient struct {
	mu          sync.Mutex
	agents      map[string]anthropic.BetaManagedAgentsAgent
	getErr      map[string]error
	listPages   [][]anthropic.BetaManagedAgentsAgent
	getCalls    int
	listCalls   int
	newCalls    int
	updateCalls int
}

func newFakeMAClient() *fakeMAClient {
	return &fakeMAClient{
		agents: make(map[string]anthropic.BetaManagedAgentsAgent),
		getErr: make(map[string]error),
	}
}

//nolint:gocritic // fake API mirrors the SDK method shape used by production code.
func (f *fakeMAClient) New(_ context.Context, params anthropic.BetaAgentNewParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCalls++
	id := "agent-created"
	if f.newCalls > 1 {
		id = fmt.Sprintf("agent-created-%d", f.newCalls)
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

func (f *fakeMAClient) Get(_ context.Context, id string, _ anthropic.BetaAgentGetParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error) {
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
func (f *fakeMAClient) Update(_ context.Context, id string, params anthropic.BetaAgentUpdateParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsAgent, error) {
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
func (f *fakeMAClient) List(_ context.Context, params anthropic.BetaAgentListParams, _ ...option.RequestOption) (*pagination.PageCursor[anthropic.BetaManagedAgentsAgent], error) {
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
