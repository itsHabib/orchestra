package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itsHabib/orchestra/internal/artifacts"
)

func TestHandleGetArtifacts_RequiresRunID(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	res, _, err := srv.handleGetArtifacts(context.Background(), nil, GetArtifactsArgs{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on missing run_id")
	}
}

func TestHandleGetArtifacts_RunNotFound(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	res, _, err := srv.handleGetArtifacts(context.Background(), nil, GetArtifactsArgs{RunID: "ghost"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on unknown run")
	}
}

// TestHandleGetArtifacts_EmptyWorkspace covers the freshly-spawned-run case:
// the registry has the entry, but the run hasn't produced any artifacts yet.
// The handler must return an empty list, not error.
func TestHandleGetArtifacts_EmptyWorkspace(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	wsDir := filepath.Join(t.TempDir(), "ws")
	if err := srv.Registry().Put(context.Background(), &Entry{RunID: "r", WorkspaceDir: wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	res, out, err := srv.handleGetArtifacts(context.Background(), nil, GetArtifactsArgs{RunID: "r"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if len(out.Artifacts) != 0 {
		t.Fatalf("Artifacts: want empty, got %v", out.Artifacts)
	}
}

func TestHandleGetArtifacts_ListsAndFilters(t *testing.T) {
	t.Parallel()
	fx := seedRunForArtifacts(t)
	seedThreeArtifacts(t, fx.runID, fx.wsDir)
	t.Run("all", func(t *testing.T) { assertGetAll(t, fx.srv, fx.runID) })
	t.Run("agent filter", func(t *testing.T) { assertGetAgentFilter(t, fx.srv, fx.runID) })
	t.Run("phase filter", func(t *testing.T) { assertGetPhaseFilter(t, fx.srv, fx.runID) })
	t.Run("agent+phase filter", func(t *testing.T) { assertGetAgentAndPhaseFilter(t, fx.srv, fx.runID) })
}

func seedThreeArtifacts(t *testing.T, runID, wsDir string) {
	t.Helper()
	store := artifacts.NewFileStore(artifactsRoot(wsDir))
	ctx := context.Background()
	seed := []struct {
		agent, key, phase, content string
	}{
		{"alpha", "a1", "design", `"a1"`},
		{"alpha", "a2", "review", `"a2"`},
		{"beta", "b1", "design", `"b1"`},
	}
	for _, s := range seed {
		if _, err := store.Put(ctx, runID, s.agent, s.key, s.phase, artifacts.Artifact{
			Type:    artifacts.TypeText,
			Content: json.RawMessage(s.content),
		}); err != nil {
			t.Fatalf("seed Put %s/%s: %v", s.agent, s.key, err)
		}
	}
}

func assertGetAll(t *testing.T, srv *Server, runID string) {
	t.Helper()
	res, out, err := srv.handleGetArtifacts(context.Background(), nil, GetArtifactsArgs{RunID: runID})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError: %s", resultText(res))
	}
	if len(out.Artifacts) != 3 {
		t.Fatalf("count = %d, want 3", len(out.Artifacts))
	}
	want := []struct{ agent, key string }{
		{"alpha", "a1"}, {"alpha", "a2"}, {"beta", "b1"},
	}
	for i, w := range want {
		if out.Artifacts[i].Agent != w.agent || out.Artifacts[i].Key != w.key {
			t.Errorf("[%d] = %s/%s, want %s/%s", i, out.Artifacts[i].Agent, out.Artifacts[i].Key, w.agent, w.key)
		}
	}
}

func assertGetAgentFilter(t *testing.T, srv *Server, runID string) {
	t.Helper()
	_, out, err := srv.handleGetArtifacts(context.Background(), nil, GetArtifactsArgs{RunID: runID, Agent: "alpha"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(out.Artifacts) != 2 {
		t.Fatalf("count = %d, want 2", len(out.Artifacts))
	}
	if out.Artifacts[0].Agent != "alpha" || out.Artifacts[1].Agent != "alpha" {
		t.Errorf("agents = %v, want both alpha", out.Artifacts)
	}
}

func assertGetPhaseFilter(t *testing.T, srv *Server, runID string) {
	t.Helper()
	_, out, err := srv.handleGetArtifacts(context.Background(), nil, GetArtifactsArgs{RunID: runID, Phase: "design"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(out.Artifacts) != 2 {
		t.Fatalf("count = %d, want 2", len(out.Artifacts))
	}
	for i := range out.Artifacts {
		if out.Artifacts[i].Phase != "design" {
			t.Errorf("[%d] phase = %q, want design", i, out.Artifacts[i].Phase)
		}
	}
}

func assertGetAgentAndPhaseFilter(t *testing.T, srv *Server, runID string) {
	t.Helper()
	_, out, err := srv.handleGetArtifacts(context.Background(), nil, GetArtifactsArgs{RunID: runID, Agent: "alpha", Phase: "review"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(out.Artifacts) != 1 || out.Artifacts[0].Key != "a2" {
		t.Fatalf("Artifacts = %+v, want [a2]", out.Artifacts)
	}
}

func TestHandleReadArtifact_Validates(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	cases := []struct {
		name string
		args ReadArtifactArgs
		want string
	}{
		{"missing run_id", ReadArtifactArgs{}, "run_id is required"},
		{"missing agent", ReadArtifactArgs{RunID: "r"}, "agent is required"},
		{"missing key", ReadArtifactArgs{RunID: "r", Agent: "a"}, "key is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _, err := srv.handleReadArtifact(context.Background(), nil, tc.args)
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if !res.IsError {
				t.Fatalf("want IsError")
			}
			if !strings.Contains(resultText(res), tc.want) {
				t.Fatalf("text = %q, want contains %q", resultText(res), tc.want)
			}
		})
	}
}

func TestHandleReadArtifact_NotFound(t *testing.T) {
	t.Parallel()
	fx := seedRunForArtifacts(t)
	res, _, err := fx.srv.handleReadArtifact(context.Background(), nil, ReadArtifactArgs{
		RunID: fx.runID, Agent: "alpha", Key: "missing",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError on missing key")
	}
	if !strings.Contains(resultText(res), "not found") {
		t.Fatalf("text = %q, want contains 'not found'", resultText(res))
	}
}

func TestHandleReadArtifact_HappyPath(t *testing.T) {
	t.Parallel()
	fx := seedRunForArtifacts(t)
	store := artifacts.NewFileStore(artifactsRoot(fx.wsDir))
	ctx := context.Background()

	if _, err := store.Put(ctx, fx.runID, "alpha", "design_doc", "design", artifacts.Artifact{
		Type:    artifacts.TypeText,
		Content: json.RawMessage(`"the doc"`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.Put(ctx, fx.runID, "alpha", "verdict", "review", artifacts.Artifact{
		Type:    artifacts.TypeJSON,
		Content: json.RawMessage(`{"k":"v"}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, out, err := fx.srv.handleReadArtifact(ctx, nil, ReadArtifactArgs{
		RunID: fx.runID, Agent: "alpha", Key: "design_doc",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError: %s", resultText(res))
	}
	if out.Type != "text" {
		t.Errorf("Type = %q, want text", out.Type)
	}
	if out.Phase != "design" {
		t.Errorf("Phase = %q, want design", out.Phase)
	}
	if string(out.Content) != `"the doc"` {
		t.Errorf("Content = %s, want %q", out.Content, `"the doc"`)
	}

	res2, out2, err := fx.srv.handleReadArtifact(ctx, nil, ReadArtifactArgs{
		RunID: fx.runID, Agent: "alpha", Key: "verdict",
	})
	if err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	if res2.IsError {
		t.Fatalf("read verdict IsError: %s", resultText(res2))
	}
	if out2.Type != "json" {
		t.Errorf("Type = %q, want json", out2.Type)
	}
	if string(out2.Content) != `{"k":"v"}` {
		t.Errorf("Content = %s, want %q", out2.Content, `{"k":"v"}`)
	}
}

// artifactFixture bundles the test server + run identity for a registered
// run. The artifact directory under wsDir is NOT seeded — callers populate
// it via [artifacts.FileStore] rooted at [artifactsRoot] so the store path
// matches what the engine writes at runtime.
type artifactFixture struct {
	srv   *Server
	runID string
	wsDir string
}

func seedRunForArtifacts(t *testing.T) artifactFixture {
	t.Helper()
	fx := artifactFixture{
		srv:   newTestServer(t, &stubSpawner{}, stateReaderFn(nil)),
		runID: "run_test",
		wsDir: filepath.Join(t.TempDir(), "run"),
	}
	if err := fx.srv.Registry().Put(context.Background(), &Entry{RunID: fx.runID, WorkspaceDir: fx.wsDir}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return fx
}
