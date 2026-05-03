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
	RunID string `json:"run_id" jsonschema:"run id returned by list_runs or by the run tool"`
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
// AddTool's generics give us, so handlers register one at a time. Each is
// wrapped with recoverHandler so a handler panic surfaces as a tool error
// instead of taking down the long-lived MCP server (and orphaning the run
// subprocesses tracked in its in-process registry).
func (s *Server) registerTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolListRuns,
		Description: "List MCP-managed orchestra runs with their derived top-level " +
			"status and per-team signal_completion outcomes. Pass active_only=true to " +
			"drop runs whose status is done.",
	}, recoverHandler(s.handleListRuns))

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolGetRun,
		Description: "Return one run's current status and per-team breakdown. " +
			"run_id is a value seen in list_runs or returned by the run tool.",
	}, recoverHandler(s.handleGetRun))

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolRun,
		Description: "Spawn an orchestra run subprocess. Provide exactly one of inline_dag " +
			"(simplified team list — name, role, prompt, deps) or config_path (absolute " +
			"path to an existing orchestra.yaml). Returns the run id; poll list_runs / " +
			"get_run to observe progress and signal_completion outcomes.",
	}, recoverHandler(s.handleRun))

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolSendMessage,
		Description: "Send a message into a run's file-based bus. recipient is a team name, " +
			"\"coordinator\", \"human\", or \"broadcast\". Works for both local and managed " +
			"agents backends.",
	}, recoverHandler(s.handleSendMessage))

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolReadMessages,
		Description: "Read messages from a run's file-based bus, newest-first. With recipient " +
			"set, narrows to that single inbox; without it, aggregates across every inbox. " +
			"since is an RFC3339 timestamp filter.",
	}, recoverHandler(s.handleReadMessages))
}

// recoverHandler wraps a typed tool handler with a deferred recover so a
// panic translates to a tool-level error result rather than tearing down the
// server. The MCP server is long-lived and owns the run registry; without
// this guard, any handler panic would orphan the running subprocesses we
// track. Mirrors the safety the old mark3labs server.WithRecovery() option
// provided; the official SDK does not recover handler panics on its own.
func recoverHandler[In, Out any](h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var (
			res  *mcp.CallToolResult
			zero Out
			out  = zero
			err  error
		)
		func() {
			defer func() {
				if r := recover(); r != nil {
					res = errResult("tool panic: %v", r)
					out = zero
					err = nil
				}
			}()
			res, out, err = h(ctx, req, in)
		}()
		return res, out, err
	}
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
// should appear in the list with status="running" and an empty agents array,
// not disappear entirely.
//
// Agents is initialized to an empty slice so it serializes to "agents": []
// for freshly-spawned runs whose state.json is still missing. A null
// serialization would be indistinguishable from "no agent data yet" vs. "the
// engine declared zero agents" and forces clients into special-case handling.
//
// Teams is populated with the same slice as Agents to keep v2 clients
// working through v3.0. The MCP server is the only mirror; consumers should
// migrate to Agents.
func (s *Server) buildRunView(ctx context.Context, e *Entry) RunView {
	v := RunView{
		RunID:        e.RunID,
		StartedAt:    e.StartedAt,
		WorkspaceDir: e.WorkspaceDir,
		RepoURL:      e.RepoURL,
		DocPaths:     append([]string(nil), e.DocPaths...),
		PID:          e.PID,
		Status:       RunStatusRunning,
		Agents:       []AgentView{},
	}
	state, err := s.stateReader(ctx, stateDir(e.WorkspaceDir))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			v.Teams = v.Agents
			return v
		}
		v.StateError = err.Error()
		v.Teams = v.Agents
		return v
	}
	v.Agents = agentViews(state)
	v.Status = deriveStatus(v.Agents)
	v.Phase = state.Phase
	v.PhaseIters = state.PhaseIters
	v.LastError = state.LastError
	v.Teams = v.Agents
	return v
}

func agentViews(state *store.RunState) []AgentView {
	if state == nil {
		return nil
	}
	views := make([]AgentView, 0, len(state.Agents))
	for name := range state.Agents {
		ts := state.Agents[name]
		views = append(views, AgentView{
			Name:          name,
			Status:        ts.Status,
			SignalStatus:  ts.SignalStatus,
			SignalSummary: ts.SignalSummary,
			SignalPRURL:   ts.SignalPRURL,
			SignalReason:  ts.SignalReason,
			CostUSD:       ts.CostUSD,
			LastTool:      ts.LastTool,
			LastEventAt:   ts.LastEventAt,
			LastError:     ts.LastError,
			ResultSummary: ts.ResultSummary,
			Artifacts:     append([]string(nil), ts.Artifacts...),
			Tokens: TokenView{
				InputTokens:              ts.InputTokens,
				OutputTokens:             ts.OutputTokens,
				CacheCreationInputTokens: ts.CacheCreationInputTokens,
				CacheReadInputTokens:     ts.CacheReadInputTokens,
			},
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
//
// failed short-circuits because it always wins the priority fold; blocked is
// tracked as a flag instead so a later-iterated failed team is not masked by
// an earlier-iterated blocked one.
func deriveStatus(agents []AgentView) string {
	if len(agents) == 0 {
		return RunStatusRunning
	}
	blocked := false
	allDone := true
	for i := range agents {
		a := &agents[i]
		if a.Status == "failed" {
			return RunStatusFailed
		}
		if a.SignalStatus == "blocked" {
			blocked = true
		}
		if a.SignalStatus != "done" {
			allDone = false
		}
	}
	switch {
	case blocked:
		return RunStatusBlocked
	case allDone:
		return RunStatusDone
	default:
		return RunStatusRunning
	}
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
