package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

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
	if got, ok := out.Content.(string); !ok || got != "the doc" {
		t.Errorf("Content = %v (%T), want string %q", out.Content, out.Content, "the doc")
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
	got, ok := out2.Content.(map[string]any)
	if !ok {
		t.Fatalf("Content = %T, want map[string]any", out2.Content)
	}
	if got["k"] != "v" {
		t.Errorf("Content[k] = %v, want %q", got["k"], "v")
	}
}

// TestHandleReadArtifact_OutputValidatesAgainstInferredSchema locks in the
// Phase A dogfood §C4 fix: the SDK's reflection-based schema generator
// previously inferred `content` as `array of integer` (the underlying
// []byte of json.RawMessage), so every real JSON-array or JSON-object
// artifact failed post-marshal output validation. The fix declares
// Content as `any`, which the generator translates to an unrestricted
// schema. This test exercises the full marshal→resolve→validate loop the
// SDK runs internally for every tool result, against the three artifact
// shapes that exist in the wild: text (JSON string), JSON object, JSON
// array. If any of these regresses the integer-items shape, validation
// here will fail with the same error the chat-side LLM saw in production.
func TestHandleReadArtifact_OutputValidatesAgainstInferredSchema(t *testing.T) {
	t.Parallel()
	fx := seedRunForArtifacts(t)
	resolved := resolvedReadArtifactSchema(t)
	for _, s := range seedReadArtifactShapes(t, fx) {
		t.Run(s.key, func(t *testing.T) {
			assertReadArtifactValidates(t, fx, resolved, s.key)
		})
	}
}

// readArtifactSeed names one (key, phase, type, content) tuple to seed under
// the fixture's "alpha" agent. Kept as a flat tuple so the surrounding test
// stays inside gocognit's threshold.
type readArtifactSeed struct {
	key, phase string
	typ        artifacts.Type
	content    string
}

func seedReadArtifactShapes(t *testing.T, fx artifactFixture) []readArtifactSeed {
	t.Helper()
	store := artifacts.NewFileStore(artifactsRoot(fx.wsDir))
	seed := []readArtifactSeed{
		{"design_doc", "design", artifacts.TypeText, `"the doc"`},
		{"verdict", "review", artifacts.TypeJSON, `{"decision":"proceed","score":0.92}`},
		{"top_5_ideas", "design", artifacts.TypeJSON, `[{"title":"a","rank":1},{"title":"b","rank":2}]`},
	}
	ctx := context.Background()
	for _, s := range seed {
		if _, err := store.Put(ctx, fx.runID, "alpha", s.key, s.phase, artifacts.Artifact{
			Type:    s.typ,
			Content: json.RawMessage(s.content),
		}); err != nil {
			t.Fatalf("seed %s: %v", s.key, err)
		}
	}
	return seed
}

// resolvedReadArtifactSchema builds the inferred output schema the same way
// the SDK does at AddTool time, then resolves it for validation. If Content
// ever regresses to json.RawMessage (or anything else inferred as a typed
// array), this schema will gain `items: integer` and validation against a
// JSON-array artifact result will fail — the exact symptom the dogfood hit.
func resolvedReadArtifactSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	schema, err := jsonschema.For[ReadArtifactResult](nil)
	if err != nil {
		t.Fatalf("infer schema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}
	return resolved
}

func assertReadArtifactValidates(t *testing.T, fx artifactFixture, resolved *jsonschema.Resolved, key string) {
	t.Helper()
	res, out, err := fx.srv.handleReadArtifact(context.Background(), nil, ReadArtifactArgs{
		RunID: fx.runID, Agent: "alpha", Key: key,
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError: %s", resultText(res))
	}
	// Round-trip through JSON the same way the SDK does before validation:
	// marshal the typed Out, then validate the bytes.
	outBytes, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var instance any
	if err := json.Unmarshal(outBytes, &instance); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := resolved.Validate(instance); err != nil {
		t.Fatalf("output validation: %v\nschema rejected: %s", err, outBytes)
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
