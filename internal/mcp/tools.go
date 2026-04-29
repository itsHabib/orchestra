package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/itsHabib/orchestra/internal/recipes"
	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
)

// Tool names. Workflow-first per the project memory: ship_design_docs / list_
// jobs / get_status / unblock — not the MA-SDK BetaManagedAgents* shape.
const (
	ToolShipDesignDocs = "ship_design_docs"
	ToolListJobs       = "list_jobs"
	ToolGetStatus      = "get_status"
	ToolUnblock        = "unblock"
)

// Run-level status strings derived from each team's state. Mirrors the
// derivation in DESIGN-ship-feature-workflow §11.2 — the run is not a first-
// class persisted entity; this is a view.
const (
	RunStatusRunning = "running"
	RunStatusBlocked = "blocked"
	RunStatusFailed  = "failed"
	RunStatusDone    = "done"
)

// ShipDesignDocsArgs is the exact JSON shape ship_design_docs accepts. Field
// tags match DESIGN §8.2; optional fields fall back to recipes-package
// defaults when absent.
type ShipDesignDocsArgs struct {
	Paths            []string `json:"paths"`
	RepoURL          string   `json:"repo_url"`
	DefaultBranch    string   `json:"default_branch,omitempty"`
	Model            string   `json:"model,omitempty"`
	Concurrency      int      `json:"concurrency,omitempty"`
	OpenPullRequests *bool    `json:"open_pull_requests,omitempty"`
}

// UnblockArgs is the unblock tool input. All three fields are required;
// missing or empty values are rejected at the handler.
type UnblockArgs struct {
	RunID   string `json:"run_id"`
	Team    string `json:"team"`
	Message string `json:"message"`
}

// GetStatusArgs is the get_status tool input.
type GetStatusArgs struct {
	RunID string `json:"run_id"`
}

// ShipDesignDocsResult is the success payload returned by ship_design_docs.
type ShipDesignDocsResult struct {
	RunID        string    `json:"run_id"`
	WorkspaceDir string    `json:"workspace_dir"`
	StartedAt    time.Time `json:"started_at"`
	PID          int       `json:"pid,omitempty"`
}

// JobView is one entry in list_jobs / the body of get_status. Combines the
// MCP-side registry data with the per-run state.json snapshot.
type JobView struct {
	RunID        string     `json:"run_id"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	WorkspaceDir string     `json:"workspace_dir"`
	RepoURL      string     `json:"repo_url"`
	DocPaths     []string   `json:"doc_paths"`
	PID          int        `json:"pid,omitempty"`
	StateError   string     `json:"state_error,omitempty"`
	Teams        []TeamView `json:"teams"`
}

// TeamView is the per-team slice of a JobView. Carries the signal_completion
// outcome verbatim so the parent Claude can drive unblock decisions without
// a second round-trip.
type TeamView struct {
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	SignalStatus  string  `json:"signal_status,omitempty"`
	SignalSummary string  `json:"signal_summary,omitempty"`
	SignalPRURL   string  `json:"signal_pr_url,omitempty"`
	SignalReason  string  `json:"signal_reason,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
}

// UnblockResult is the success payload returned by unblock.
type UnblockResult struct {
	OK bool `json:"ok"`
}

// StateReader returns the current state.json snapshot for a workspace dir.
// Production callers wire filestore.ReadActiveRunState; tests pass a stub.
// The signature keeps the dependency on internal/store/filestore in tools.go
// rather than burying it inside the Server struct, which keeps the handler
// code one indirection from the actual disk read.
type StateReader func(ctx context.Context, workspaceDir string) (*store.RunState, error)

// Steerer relays a user.message into a session. Production callers wire
// SessionSteerer (which builds a fresh SDK client per call); tests pass a
// stub that records the args. Lock-free per DESIGN-v2 §11 — orchestra msg
// reads state.json without holding the run lock and the same applies here.
//
// The blocked → done recovery flow does not require any MCP-side state
// write here: internal/customtools/signal_completion.go allows
// signal_completion(done) to overwrite a prior blocked signal in the run
// subprocess (the only writer of state.json), so unblock just needs to
// land the user.message and the agent's eventual completion signal lands
// naturally.
type Steerer func(ctx context.Context, sessionID, message string) error

// ToolRegistration is the registration shape returned by Tools(). Decoupled
// from the mark3labs server type so callers can attach the same tool list to
// stdio and HTTP transports without recreating the definitions.
type ToolRegistration struct {
	Tool    mcptypes.Tool
	Handler server.ToolHandlerFunc
}

// pointerHandler is the internal handler signature: same as server.
// ToolHandlerFunc but takes a *CallToolRequest so the 80-byte struct is not
// copied on every dispatch (gocritic hugeParam, configured threshold).
type pointerHandler func(ctx context.Context, req *mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error)

// adapt wraps a pointer-shaped handler in the value-shaped signature the
// mark3labs library mandates. The library copies the request once into the
// adapter; the handler never copies again.
func adapt(h pointerHandler) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
		return h(ctx, &req)
	}
}

// Tools returns the tool registrations for the four MCP entry points. The
// handlers close over the dependencies passed in so the same Server value
// can register against multiple transports if ever needed.
//
// Caller is responsible for non-nil arguments — the handlers panic-via-error
// on a nil registry but defensively skipping nil checks here would let a mis-
// wired Server start up and fail every request, which is worse.
func (s *Server) Tools() []ToolRegistration {
	return []ToolRegistration{
		{Tool: shipDesignDocsTool(), Handler: adapt(s.handleShipDesignDocs)},
		{Tool: listJobsTool(), Handler: adapt(s.handleListJobs)},
		{Tool: getStatusTool(), Handler: adapt(s.handleGetStatus)},
		{Tool: unblockTool(), Handler: adapt(s.handleUnblock)},
	}
}

func shipDesignDocsTool() mcptypes.Tool {
	return mcptypes.NewTool(ToolShipDesignDocs,
		mcptypes.WithDescription(
			"Ship N design docs through the /ship-feature workflow: spawns one "+
				"orchestra run subprocess per call, with one team per doc. "+
				"Returns the run id; poll list_jobs / get_status to observe "+
				"progress and PR URLs as each team signals completion.",
		),
		mcptypes.WithArray("paths",
			mcptypes.Required(),
			mcptypes.Description("Repo-relative design doc paths. Each becomes one team."),
			mcptypes.WithStringItems(),
		),
		mcptypes.WithString("repo_url",
			mcptypes.Required(),
			mcptypes.Description("https GitHub URL the agents will clone and push to."),
		),
		mcptypes.WithString("default_branch",
			mcptypes.Description("Repository default branch. Defaults to \"main\"."),
		),
		mcptypes.WithString("model",
			mcptypes.Description("Model name passed to every team. Defaults to \"opus\"."),
		),
		mcptypes.WithNumber("concurrency",
			mcptypes.Description("Max parallel MA sessions. Defaults to 4."),
		),
		mcptypes.WithBoolean("open_pull_requests",
			mcptypes.Description("Whether the engine auto-opens PRs. Defaults to true."),
		),
	)
}

func listJobsTool() mcptypes.Tool {
	return mcptypes.NewTool(ToolListJobs,
		mcptypes.WithDescription(
			"List every MCP-managed run in the registry with its derived "+
				"top-level status and per-team signal_completion outcomes.",
		),
	)
}

func getStatusTool() mcptypes.Tool {
	return mcptypes.NewTool(ToolGetStatus,
		mcptypes.WithDescription(
			"Return one run's current status and per-team breakdown.",
		),
		mcptypes.WithString("run_id",
			mcptypes.Required(),
			mcptypes.Description("Run id returned by ship_design_docs."),
		),
	)
}

func unblockTool() mcptypes.Tool {
	return mcptypes.NewTool(ToolUnblock,
		mcptypes.WithDescription(
			"Send a user.message into a blocked team's MA session. Mirrors "+
				"`orchestra msg --team <team> --message <message>` against the "+
				"workspace registered for run_id.",
		),
		mcptypes.WithString("run_id",
			mcptypes.Required(),
			mcptypes.Description("Run id returned by ship_design_docs."),
		),
		mcptypes.WithString("team",
			mcptypes.Required(),
			mcptypes.Description("Team name from the run's state.json."),
		),
		mcptypes.WithString("message",
			mcptypes.Required(),
			mcptypes.Description("Free-form text the agent sees as a user.message event."),
		),
	)
}

func (s *Server) handleShipDesignDocs(ctx context.Context, req *mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
	var args ShipDesignDocsArgs
	if err := req.BindArguments(&args); err != nil {
		return mcptypes.NewToolResultErrorf("invalid arguments: %v", err), nil
	}
	params, err := args.toRecipeParams()
	if err != nil {
		return mcptypes.NewToolResultError(err.Error()), nil
	}
	cfg, err := recipes.ShipDesignDocs(params)
	if err != nil {
		return mcptypes.NewToolResultErrorf("recipe: %v", err), nil
	}

	runID := NewRunID(time.Now())
	entry, err := PrepareRun(s.workspaceRoot, runID, cfg, params.RepoURL, params.DocPaths)
	if err != nil {
		return mcptypes.NewToolResultErrorf("prepare run: %v", err), nil
	}
	proc, err := s.spawner.Start(ctx, entry)
	if err != nil {
		// Spawn failed: the workspace dir + yaml are on disk with no
		// running process attached and no registry entry. Reclaim the
		// directory so repeated failures do not pile up under the user
		// data dir.
		_ = os.RemoveAll(entry.WorkspaceDir)
		return mcptypes.NewToolResultErrorf("spawn run: %v", err), nil
	}
	if proc != nil {
		entry.PID = proc.Pid
	}
	if err := s.registry.Put(ctx, entry); err != nil {
		// Subprocess is already running but unregistered — list_jobs /
		// get_status / unblock cannot reach it. Best-effort: kill the
		// process and reclaim the workspace so the run stops doing work
		// no caller can observe. The original registry-write error is
		// returned to the caller so they know what happened.
		if proc != nil {
			_ = proc.Kill()
		}
		_ = os.RemoveAll(entry.WorkspaceDir)
		return mcptypes.NewToolResultErrorf("register run: %v", err), nil
	}

	res := ShipDesignDocsResult{
		RunID:        entry.RunID,
		WorkspaceDir: entry.WorkspaceDir,
		StartedAt:    entry.StartedAt,
		PID:          entry.PID,
	}
	return jsonResult(res, fmt.Sprintf("run %s started in %s", res.RunID, res.WorkspaceDir))
}

func (s *Server) handleListJobs(ctx context.Context, _ *mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
	entries, err := s.registry.List(ctx)
	if err != nil {
		return mcptypes.NewToolResultErrorf("list registry: %v", err), nil
	}
	views := make([]JobView, 0, len(entries))
	for i := range entries {
		views = append(views, s.buildJobView(ctx, &entries[i]))
	}
	return jsonResult(views, fmt.Sprintf("%d run(s)", len(views)))
}

func (s *Server) handleGetStatus(ctx context.Context, req *mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
	var args GetStatusArgs
	if err := req.BindArguments(&args); err != nil {
		return mcptypes.NewToolResultErrorf("invalid arguments: %v", err), nil
	}
	if args.RunID == "" {
		return mcptypes.NewToolResultError("run_id is required"), nil
	}
	entry, ok, err := s.registry.Get(ctx, args.RunID)
	if err != nil {
		return mcptypes.NewToolResultErrorf("read registry: %v", err), nil
	}
	if !ok {
		return mcptypes.NewToolResultErrorf("run %q not found", args.RunID), nil
	}
	view := s.buildJobView(ctx, &entry)
	return jsonResult(view, fmt.Sprintf("run %s: %s", view.RunID, view.Status))
}

func (s *Server) handleUnblock(ctx context.Context, req *mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
	var args UnblockArgs
	if err := req.BindArguments(&args); err != nil {
		return mcptypes.NewToolResultErrorf("invalid arguments: %v", err), nil
	}
	if args.RunID == "" || args.Team == "" || args.Message == "" {
		return mcptypes.NewToolResultError("run_id, team, and message are all required"), nil
	}
	entry, ok, err := s.registry.Get(ctx, args.RunID)
	if err != nil {
		return mcptypes.NewToolResultErrorf("read registry: %v", err), nil
	}
	if !ok {
		return mcptypes.NewToolResultErrorf("run %q not found", args.RunID), nil
	}
	state, err := s.stateReader(ctx, stateDir(entry.WorkspaceDir))
	if err != nil {
		return mcptypes.NewToolResultErrorf("read run state: %v", err), nil
	}
	sessionID, err := spawner.SteerableSessionID(state, args.Team)
	if err != nil {
		return mcptypes.NewToolResultErrorf("%v", err), nil
	}
	if err := s.steerer(ctx, sessionID, args.Message); err != nil {
		return mcptypes.NewToolResultErrorf("send user.message: %v", err), nil
	}
	// No host-side state write here. The signal_completion handler in
	// internal/customtools allows the agent's follow-up
	// signal_completion(done) to overwrite the recorded blocked signal,
	// so the run subprocess remains the single writer of state.json and
	// the cross-process race that an MCP-side clearer would introduce
	// (Codex P1 / Copilot review on PR #28) is avoided structurally.
	return jsonResult(UnblockResult{OK: true}, "ok")
}

// toRecipeParams maps the MCP tool arguments into the recipe's parameter
// struct. Validation lives in recipes.ShipDesignDocs; this layer only fills
// in the field translation (notably open_pull_requests → DisablePullRequests
// inversion).
func (a *ShipDesignDocsArgs) toRecipeParams() (*recipes.ShipDesignDocsParams, error) {
	if a == nil {
		return nil, errors.New("nil arguments")
	}
	p := &recipes.ShipDesignDocsParams{
		DocPaths:      append([]string(nil), a.Paths...),
		RepoURL:       a.RepoURL,
		DefaultBranch: a.DefaultBranch,
		Model:         a.Model,
		Concurrency:   a.Concurrency,
	}
	if a.OpenPullRequests != nil {
		p.DisablePullRequests = !*a.OpenPullRequests
	}
	return p, nil
}

// buildJobView fuses one registry entry with its on-disk state.json. State-
// read errors are reported via JobView.StateError rather than failing the
// whole list — a freshly-spawned run that hasn't written state.json yet
// should appear in the list with status="running" and no team rows, not
// disappear entirely.
func (s *Server) buildJobView(ctx context.Context, e *Entry) JobView {
	v := JobView{
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

// deriveStatus implements the §11.2 derivation: failed > blocked > done >
// running. Empty team list (state.json missing or unwritten) defaults to
// running so freshly-spawned jobs show up correctly.
//
// "all done" gates strictly on SignalStatus == "done", not on the engine's
// Status field. This is by design (§7.2): signal_completion is the canonical
// end-of-team signal — the host treats Status="done" without a matching
// signal as "the engine archived the session for an unrelated reason"
// (timeout, MA-side error, etc.) which is closer to incomplete than to done.
// Such runs stay at "running" until the engine flips a team to Status=
// "failed", which the case above promotes to RunStatusFailed.
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

// jsonResult builds a CallToolResult that carries the structured payload
// for clients that read structuredContent and a plain-text fallback for
// clients that only render TextContent. mcp-go's NewToolResultStructured
// does the fan-out; the helper exists so the call sites stay short and so
// the (result, nil) error-discipline at every handler site is uniform.
func jsonResult(payload any, fallback string) (*mcptypes.CallToolResult, error) {
	return mcptypes.NewToolResultStructured(payload, fallback), nil
}

// DefaultStateReader is the production StateReader. It calls into filestore
// without acquiring the run lock.
func DefaultStateReader(ctx context.Context, workspaceDir string) (*store.RunState, error) {
	return filestore.ReadActiveRunState(ctx, workspaceDir)
}

// SessionSteerer constructs a SessionEventsClient on demand and sends one
// user.message. Each call gets a fresh client; for the MCP server's
// occasional steering cadence the cost is negligible and the simpler
// lifetime is worth it.
func SessionSteerer(ctx context.Context, sessionID, message string) error {
	sessions, err := spawner.SessionEventsClient(ctx)
	if err != nil {
		return fmt.Errorf("session events client: %w", err)
	}
	return spawner.SendUserMessage(ctx, sessions, sessionID, message, defaultSteerRetries)
}

// defaultSteerRetries matches cmd/msg.go's default — at-least-once delivery
// on transient 429/5xx, capped retry budget.
const defaultSteerRetries = 4
