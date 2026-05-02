package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/messaging"
)

func TestParseRunURI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		uri     string
		runID   string
		wantErr bool
	}{
		{"orchestra://runs/abc", "abc", false},
		{"orchestra://runs/abc/messages", "", true},
		{"orchestra://runs/", "", true},
		{"orchestra://other/abc", "", true},
	}
	for _, tc := range cases {
		runID, err := parseRunURI(tc.uri)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error", tc.uri)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.uri, err)
		}
		if runID != tc.runID {
			t.Fatalf("%s: got %q, want %q", tc.uri, runID, tc.runID)
		}
	}
}

func TestParseRunMessagesURI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		uri     string
		runID   string
		wantErr bool
	}{
		{"orchestra://runs/abc/messages", "abc", false},
		{"orchestra://runs/abc", "", true},
		{"orchestra://runs//messages", "", true},
		{"orchestra://other/abc/messages", "", true},
	}
	for _, tc := range cases {
		runID, err := parseRunMessagesURI(tc.uri)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error", tc.uri)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.uri, err)
		}
		if runID != tc.runID {
			t.Fatalf("%s: got %q, want %q", tc.uri, runID, tc.runID)
		}
	}
}

func TestReadRunsResource_EmptyRegistry(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, err := srv.readRunsResource(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: ResourceRunsURI},
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out ListRunsResult
	if err := decodeResource(res, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Runs) != 0 {
		t.Fatalf("runs: %d, want 0", len(out.Runs))
	}
}

func TestReadRunResource_HappyPath(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	seedRun(t, srv, nil)

	res, err := srv.readRunResource(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "orchestra://runs/alpha"},
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var view RunView
	if err := decodeResource(res, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.RunID != "alpha" {
		t.Fatalf("run id: got %q, want %q", view.RunID, "alpha")
	}
}

func TestReadRunResource_UnknownReturnsResourceNotFound(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	_, err := srv.readRunResource(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "orchestra://runs/ghost"},
	})
	if err == nil {
		t.Fatalf("expected error for unknown run")
	}
	if !isResourceNotFound(err) {
		t.Fatalf("error not classified as ResourceNotFound: %v", err)
	}
}

func TestReadRunMessagesResource_NewestFirst(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	bus := seedRun(t, srv, []string{"design"})

	earlier := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	if err := bus.Send(&messaging.Message{
		ID: "1", Sender: "0-human", Recipient: "2-design",
		Type: messaging.MsgCorrection, Content: "old", Timestamp: earlier,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := bus.Send(&messaging.Message{
		ID: "2", Sender: "0-human", Recipient: "2-design",
		Type: messaging.MsgCorrection, Content: "new", Timestamp: later,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	res, err := srv.readRunMessagesResource(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "orchestra://runs/alpha/messages"},
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out ReadMessagesResult
	if err := decodeResource(res, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages: %d", len(out.Messages))
	}
	if out.Messages[0].ID != "2" {
		t.Fatalf("expected newest-first; got %v then %v", out.Messages[0].ID, out.Messages[1].ID)
	}
}

func TestRegisterResources_AdvertisesAll(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientSession := connectInProcess(ctx, t, srv)

	resources, err := clientSession.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources.Resources) != 1 || resources.Resources[0].URI != ResourceRunsURI {
		t.Fatalf("resources: %+v", resources.Resources)
	}

	templates, err := clientSession.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	gotTemplates := make(map[string]bool)
	for _, tmpl := range templates.ResourceTemplates {
		gotTemplates[tmpl.URITemplate] = true
	}
	for _, want := range []string{ResourceRunTemplateURI, ResourceRunMessagesTemplateURI} {
		if !gotTemplates[want] {
			t.Fatalf("missing resource template %q in %v", want, gotTemplates)
		}
	}
}

func decodeResource(r *mcp.ReadResourceResult, dst any) error {
	if r == nil || len(r.Contents) == 0 {
		return errResourceEmpty
	}
	return json.Unmarshal([]byte(r.Contents[0].Text), dst)
}

var errResourceEmpty = errors.New("resource result has no contents")

// isResourceNotFound checks whether err carries the SDK's resource-not-found
// status code (-32002). ResourceNotFoundError wraps a *jsonrpc.Error; that
// type is the SDK-public alias the test can pull the code off without
// dipping into internal packages.
func isResourceNotFound(err error) bool {
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Code == mcp.CodeResourceNotFound
}
