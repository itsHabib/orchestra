package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
)

// Tool names. Workflow-first per the project memory: list_runs / get_run.
const (
	ToolListRuns = "list_runs"
	ToolGetRun   = "get_run"
)

// ListRunsArgs is the list_runs tool input.
type ListRunsArgs struct {
	ActiveOnly bool `json:"active_only,omitempty" jsonschema:"when true, drop runs whose derived status is done"`
}

// ListRunsResult is the list_runs tool output. Wrapping the slice in a struct
// keeps the MCP structuredContent shape an object so the chat-side LLM does
// not have to special-case top-level arrays.
type ListRunsResult struct {
	Runs []RunView `json:"runs"`
}

// GetRunArgs is the get_run tool input.
type GetRunArgs struct {
	RunID string `json:"run_id" jsonschema:"run id returned by run"`
}

// StateReader returns the current state.json snapshot for a workspace dir.
// Production callers wire DefaultStateReader; tests pass a stub.
type StateReader func(ctx context.Context, workspaceDir string) (*store.RunState, error)

// DefaultStateReader is the production StateReader. It calls into filestore
// without acquiring the run lock.
func DefaultStateReader(ctx context.Context, workspaceDir string) (*store.RunState, error) {
	return filestore.ReadActiveRunState(ctx, workspaceDir)
}

// registerTools attaches the v1 generic tool surface to the underlying SDK
// server. Direct registration over a slice would lose the typed In/Out shape
// AddTool's generics give us, so handlers register one at a time.
func (s *Server) registerTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolListRuns,
		Description: "List MCP-managed orchestra runs with their derived top-level " +
			"status and per-team signal_completion outcomes. Pass active_only=true to " +
			"drop runs whose status is done.",
	}, s.handleListRuns)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolGetRun,
		Description: "Return one run's current status and per-team breakdown. " +
			"run_id is the value returned by the run tool.",
	}, s.handleGetRun)
}

func (s *Server) handleListRuns(ctx context.Context, _ *mcp.CallToolRequest, args ListRunsArgs) (*mcp.CallToolResult, ListRunsResult, error) {
	entries, err := s.registry.List(ctx)
	if err != nil {
		return errResult("list registry: %v", err), ListRunsResult{}, nil
	}
	views := make([]RunView, 0, len(entries))
	for i := range entries {
		v := s.buildRunView(ctx, &entries[i])
		if args.ActiveOnly && v.Status == RunStatusDone {
			continue
		}
		views = append(views, v)
	}
	return textResult(fmt.Sprintf("%d run(s)", len(views))), ListRunsResult{Runs: views}, nil
}

func (s *Server) handleGetRun(ctx context.Context, _ *mcp.CallToolRequest, args GetRunArgs) (*mcp.CallToolResult, RunView, error) {
	if args.RunID == "" {
		return errResult("run_id is required"), RunView{}, nil
	}
	entry, ok, err := s.registry.Get(ctx, args.RunID)
	if err != nil {
		return errResult("read registry: %v", err), RunView{}, nil
	}
	if !ok {
		return errResult("run %q not found", args.RunID), RunView{}, nil
	}
	view := s.buildRunView(ctx, &entry)
	return textResult(fmt.Sprintf("run %s: %s", view.RunID, view.Status)), view, nil
}

// buildRunView fuses one registry entry with its on-disk state.json. State-
// read errors are reported via RunView.StateError rather than failing the
// whole list — a freshly-spawned run that hasn't written state.json yet
// should appear in the list with status="running" and no team rows, not
// disappear entirely.
func (s *Server) buildRunView(ctx context.Context, e *Entry) RunView {
	v := RunView{
		RunID:        e.RunID,
		StartedAt:    e.StartedAt,
		WorkspaceDir: e.WorkspaceDir,
		RepoURL:      e.RepoURL,
		DocPaths:     append([]string(nil), e.DocPaths...),
		PID:          e.PID,
		Status:       RunStatusRunning,
	}
	state, err := s.stateReader(ctx, stateDir(e.WorkspaceDir))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return v
		}
		v.StateError = err.Error()
		return v
	}
	v.Teams = teamViews(state)
	v.Status = deriveStatus(v.Teams)
	return v
}

func teamViews(state *store.RunState) []TeamView {
	if state == nil {
		return nil
	}
	views := make([]TeamView, 0, len(state.Teams))
	for name := range state.Teams {
		ts := state.Teams[name]
		views = append(views, TeamView{
			Name:          name,
			Status:        ts.Status,
			SignalStatus:  ts.SignalStatus,
			SignalSummary: ts.SignalSummary,
			SignalPRURL:   ts.SignalPRURL,
			SignalReason:  ts.SignalReason,
			CostUSD:       ts.CostUSD,
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

// deriveStatus implements the status fold: failed > blocked > done > running.
// Empty team list (state.json missing or unwritten) defaults to running so
// freshly-spawned runs show up correctly.
//
// "all done" gates strictly on SignalStatus == "done", not on the engine's
// Status field. This is by design: signal_completion is the canonical end-of-
// team signal — the host treats Status="done" without a matching signal as
// "the engine archived the session for an unrelated reason" (timeout, MA-side
// error, etc.) which is closer to incomplete than to done. Such runs stay at
// "running" until the engine flips a team to Status="failed", which the case
// above promotes to RunStatusFailed.
func deriveStatus(teams []TeamView) string {
	if len(teams) == 0 {
		return RunStatusRunning
	}
	allDone := true
	for _, t := range teams {
		switch {
		case t.Status == "failed":
			return RunStatusFailed
		case t.SignalStatus == "blocked":
			return RunStatusBlocked
		case t.SignalStatus != "done":
			allDone = false
		}
	}
	if allDone {
		return RunStatusDone
	}
	return RunStatusRunning
}

// errResult builds a tool-level error CallToolResult. The error is reported to
// the chat-side LLM as IsError=true with the formatted text in Content; the
// outer protocol error stays nil so the SDK does not surface it as a JSON-RPC
// transport error.
func errResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// textResult builds a success CallToolResult with a one-line text fallback for
// clients that ignore structuredContent. The structured payload travels as the
// typed Out value the SDK auto-marshals.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}
