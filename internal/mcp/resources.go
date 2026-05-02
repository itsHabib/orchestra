package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Resource URIs. The runs list lives at a fixed URI; the per-run resources
// use templates so the chat-side LLM can reach them via uri-templating.
const (
	ResourceRunsURI                = "orchestra://runs"
	ResourceRunTemplateURI         = "orchestra://runs/{run_id}"
	ResourceRunMessagesTemplateURI = "orchestra://runs/{run_id}/messages"
	resourceRunsPrefix             = "orchestra://runs/"
	resourceRunMessagesURISuffix   = "/messages"
)

// registerResources attaches the orchestra:// resources to the SDK server.
// Each handler returns JSON in a single TextResourceContents — the SDK does
// not auto-marshal Go values for resources the way it does for typed tools.
func (s *Server) registerResources() {
	s.mcp.AddResource(&mcp.Resource{
		URI:         ResourceRunsURI,
		Name:        "runs",
		Description: "All MCP-managed orchestra runs as a JSON array (same shape as list_runs without filters).",
		MIMEType:    "application/json",
	}, s.readRunsResource)

	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: ResourceRunTemplateURI,
		Name:        "run",
		Description: "One MCP-managed orchestra run, same shape as get_run.",
		MIMEType:    "application/json",
	}, s.readRunResource)

	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: ResourceRunMessagesTemplateURI,
		Name:        "run-messages",
		Description: "All messages on a run's bus, newest-first.",
		MIMEType:    "application/json",
	}, s.readRunMessagesResource)
}

func (s *Server) readRunsResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	entries, err := s.registry.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list registry: %w", err)
	}
	views := make([]RunView, 0, len(entries))
	for i := range entries {
		views = append(views, s.buildRunView(ctx, &entries[i]))
	}
	return jsonResource(req.Params.URI, ListRunsResult{Runs: views})
}

func (s *Server) readRunResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	runID, err := parseRunURI(req.Params.URI)
	if err != nil {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	entry, ok, err := s.registry.Get(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	if !ok {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	view := s.buildRunView(ctx, &entry)
	return jsonResource(req.Params.URI, view)
}

func (s *Server) readRunMessagesResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	runID, err := parseRunMessagesURI(req.Params.URI)
	if err != nil {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	bus, msgsDir, err := s.busForRun(ctx, runID)
	if err != nil {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	raw, err := readAllInboxes(bus, msgsDir)
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	views := toMessageViews(raw, runID, time.Time{})
	return jsonResource(req.Params.URI, ReadMessagesResult{Messages: views})
}

// parseRunURI extracts the run id from orchestra://runs/{run_id}. Rejects the
// /messages variant — that is parseRunMessagesURI's job.
func parseRunURI(uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, resourceRunsPrefix)
	if !ok {
		return "", fmt.Errorf("uri %q does not match orchestra://runs/{run_id}", uri)
	}
	if rest == "" || strings.Contains(rest, "/") {
		return "", fmt.Errorf("uri %q is not a single-run resource", uri)
	}
	return rest, nil
}

// parseRunMessagesURI extracts the run id from orchestra://runs/{run_id}/messages.
func parseRunMessagesURI(uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, resourceRunsPrefix)
	if !ok {
		return "", fmt.Errorf("uri %q does not match orchestra://runs/{run_id}/messages", uri)
	}
	runID, ok := strings.CutSuffix(rest, resourceRunMessagesURISuffix)
	if !ok {
		return "", fmt.Errorf("uri %q is missing /messages suffix", uri)
	}
	if runID == "" || strings.Contains(runID, "/") {
		return "", fmt.Errorf("uri %q has unexpected run id segment", uri)
	}
	return runID, nil
}

// jsonResource serializes payload as a single TextResourceContents entry
// tagged with the resolved URI. Resource handlers return raw JSON because the
// SDK does not auto-marshal typed values for resource reads the way it does
// for tool outputs.
func jsonResource(uri string, payload any) (*mcp.ReadResourceResult, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", uri, err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(data),
		}},
	}, nil
}
