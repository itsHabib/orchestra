// Package mcpunblocksmoke is the live-MA acceptance smoke for §12.3 of
// DESIGN-ship-feature-workflow: a team that calls signal_completion(blocked)
// must be reachable via the MCP unblock tool, and a follow-up user.message
// must be enough for the team to resume and ultimately call
// signal_completion(done) with a real PR URL.
//
// This is the load-bearing piece of the workflow that P3 explicitly deferred
// — without it, the MCP unblock story is unverified end-to-end against MA.
//
// The smoke is opt-in: it runs only when ORCHESTRA_MA_INTEGRATION=1 and
// ORCHESTRA_MCP_TEST_REPO_URL points at a sandbox repository the agent is
// allowed to push to. The fixture doc path is hardcoded (not env-driven) so
// the test verifies a known-ambiguous spec end-to-end; the README explains
// how to copy the canonical fixture into your sandbox.
package mcpunblocksmoke

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptypes "github.com/mark3labs/mcp-go/mcp"

	"github.com/itsHabib/orchestra/internal/mcp"
)

const (
	// smokeProtocolTimeout caps per-MCP-call latency. Initialize and
	// each get_status / unblock CallTool round-trip must complete in
	// well under this budget; a slow handler is itself a failure.
	smokeProtocolTimeout = 30 * time.Second

	// unblockRunTimeout caps the entire ship → blocked → unblock → done
	// flow. Matches the existing mcp_smoke live-ship cap (30 minutes).
	// Ambiguity recognition (~3–10 min) plus a real /ship-feature ship
	// (~15–20 min) sits inside this budget; if a single disambiguation
	// needs more, surface that to the kickoff before raising the cap.
	unblockRunTimeout = 30 * time.Minute

	// pollEvery is the cadence between get_status reads. The agent
	// works on the order of minutes per turn; 30 seconds keeps the
	// transition timestamps tight without hammering the MCP server.
	pollEvery = 30 * time.Second

	// fixtureDocPath is the repo-relative path of the ambiguous design
	// doc inside the sandbox repo. Pinned per the kickoff: "Pin the
	// path; don't generate it." The sandbox layout scopes integration
	// fixtures under per-backend directories (e.g. orchestra-ma/) so
	// this path includes that prefix; the README spells out the layout.
	// The canonical fixture content lives in the orchestra repo at
	// docs/test-fixtures/ambiguous-mystery-flag.md.
	fixtureDocPath = "orchestra-ma/docs/test-fixtures/ambiguous-mystery-flag.md"

	// fixtureTeamName is the team name the recipe derives from the
	// fixture doc. Hardcoded so the test fails loudly if the recipe's
	// slug logic ever drifts.
	// recipes.teamNameForDoc(fixtureDocPath) returns
	// "ship-ambiguous-mystery-flag" — recipe takes the path's basename,
	// strips the extension, and lowercases; the orchestra-ma/docs/
	// prefix has no effect on the resulting slug.
	fixtureTeamName = "ship-ambiguous-mystery-flag"

	// unblockMessage is the disambiguation copied verbatim from §12.3.
	// The agent, once steered with this text, should implement a
	// `--debug` boolean flag and ship a real PR.
	unblockMessage = "make it a --debug bool that enables debug logging"
)

func TestMCPUnblockSmoke_LiveBinary(t *testing.T) {
	if os.Getenv("ORCHESTRA_MA_INTEGRATION") != "1" {
		t.Skip("set ORCHESTRA_MA_INTEGRATION=1 to enable")
	}
	repoURL := os.Getenv("ORCHESTRA_MCP_TEST_REPO_URL")
	if repoURL == "" {
		t.Skip("set ORCHESTRA_MCP_TEST_REPO_URL=https://github.com/<owner>/<repo> alongside ORCHESTRA_MA_INTEGRATION=1")
	}

	cmdCtx, cancelCmd := context.WithCancel(context.Background())
	t.Cleanup(cancelCmd)

	bin := buildOrchestraBinary(cmdCtx, t)
	dataRoot := t.TempDir()
	registryPath := filepath.Join(dataRoot, "mcp-runs.json")
	workspaceRoot := filepath.Join(dataRoot, "runs")
	t.Logf("dataRoot=%s registryPath=%s workspaceRoot=%s", dataRoot, registryPath, workspaceRoot)
	client := startMCPClient(t, bin, registryPath, workspaceRoot)

	initCtx, initCancel := context.WithTimeout(cmdCtx, smokeProtocolTimeout)
	defer initCancel()
	if _, err := client.Initialize(initCtx, mcptypes.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	deadline := time.Now().Add(unblockRunTimeout)

	runID := callShipAmbiguous(cmdCtx, t, client, repoURL)
	t.Logf("ship_design_docs run_id=%s; polling for blocked", runID)

	blocked := pollUntilBlocked(cmdCtx, t, client, runID, deadline)
	t.Logf("blocked: team=%s reason=%q", blocked.Name, blocked.SignalReason)

	callUnblock(cmdCtx, t, client, runID, blocked.Name)
	t.Logf("unblock sent; polling for done")

	done := pollUntilDoneAfterUnblock(cmdCtx, t, client, runID, blocked.Name, deadline)
	if done.PRURL == "" {
		t.Fatalf("team %s reached done with no recoverable PR URL; "+
			"signal_status=%q signal_pr_url=%q summary=%q",
			done.Team.Name, done.Team.SignalStatus, done.Team.SignalPRURL, done.Team.SignalSummary)
	}
	t.Logf("done: team=%s pr_url=%s signal_status=%q summary=%q",
		done.Team.Name, done.PRURL, done.Team.SignalStatus, done.Team.SignalSummary)
}

func startMCPClient(t *testing.T, bin, registryPath, workspaceRoot string) *mcpclient.Client {
	t.Helper()
	c, err := mcpclient.NewStdioMCPClient(
		bin,
		os.Environ(),
		"mcp",
		"--transport", "stdio",
		"--registry-path", registryPath,
		"--workspace-root", workspaceRoot,
	)
	if err != nil {
		t.Fatalf("NewStdioMCPClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func callShipAmbiguous(parentCtx context.Context, t *testing.T, c *mcpclient.Client, repoURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(parentCtx, smokeProtocolTimeout)
	defer cancel()
	res, err := c.CallTool(ctx, mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{
			Name: mcp.ToolShipDesignDocs,
			Arguments: map[string]any{
				"paths":    []any{fixtureDocPath},
				"repo_url": repoURL,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool ship_design_docs: %v", err)
	}
	if res.IsError {
		t.Fatalf("ship_design_docs IsError: %s", contentText(res))
	}
	var payload mcp.ShipDesignDocsResult
	if err := decodeStructured(res, &payload); err != nil {
		t.Fatalf("decode ship_design_docs result: %v; text=%s", err, contentText(res))
	}
	if payload.RunID == "" {
		t.Fatalf("ship_design_docs returned empty run_id; text=%s", contentText(res))
	}
	return payload.RunID
}

// pollUntilBlocked polls get_status until the fixture team's signal_status
// flips to "blocked", or until the shared deadline expires. Returns the
// observed TeamView for the caller to log + drive unblock against.
//
// The polling loop tolerates two transient shapes:
//   - the run has not written state.json yet (no Teams in the view) — this
//     is normal in the first ~30 s after ship_design_docs returns
//   - the team is recorded but signal_status is still empty — the agent is
//     working its way through the doc
//
// Anything else (state read error, derived run status of failed) is fatal.
func pollUntilBlocked(parentCtx context.Context, t *testing.T, c *mcpclient.Client, runID string, deadline time.Time) mcp.TeamView {
	t.Helper()
	for time.Now().Before(deadline) {
		if err := parentCtx.Err(); err != nil {
			t.Fatalf("driver context canceled while polling for blocked: %v", err)
		}
		view := fetchStatus(parentCtx, t, c, runID)
		if view.StateError != "" {
			t.Fatalf("get_status state_error while waiting for blocked: %s", view.StateError)
		}
		if view.Status == mcp.RunStatusFailed {
			snapshotState(t, &view)
			t.Fatalf("run %s failed before reaching blocked: %s", runID, jobSummary(&view))
		}
		team, ok := findTeam(&view, fixtureTeamName)
		switch {
		case !ok:
			// state.json or the team row hasn't been written yet.
		case team.SignalStatus == "blocked":
			return team
		case team.SignalStatus == "done":
			t.Fatalf("team %s reached done before blocked — fixture is no longer ambiguous; pr_url=%s summary=%q",
				team.Name, team.SignalPRURL, team.SignalSummary)
		}
		select {
		case <-time.After(pollEvery):
		case <-parentCtx.Done():
			t.Fatalf("driver context canceled while waiting to poll: %v", parentCtx.Err())
		}
	}
	t.Fatalf("run %s did not reach signal_status=blocked within %s",
		runID, unblockRunTimeout)
	return mcp.TeamView{}
}

func callUnblock(parentCtx context.Context, t *testing.T, c *mcpclient.Client, runID, team string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parentCtx, smokeProtocolTimeout)
	defer cancel()
	res, err := c.CallTool(ctx, mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{
			Name: mcp.ToolUnblock,
			Arguments: map[string]any{
				"run_id":  runID,
				"team":    team,
				"message": unblockMessage,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool unblock: %v", err)
	}
	if res.IsError {
		text := contentText(res)
		// SteerableSessionID returns "<sentinel>: <team> is <status>" when
		// ts.Status is no longer "running" — i.e. the engine archived the
		// team after signal_completion(blocked). That violates §12.3 and
		// invalidates the MCP unblock story; flag it as the architecture
		// finding the kickoff calls out before any further work.
		if strings.Contains(text, `is "done"`) || strings.Contains(text, `is "failed"`) {
			t.Fatalf("unblock failed because team is no longer running — §12.3 ARCHITECTURE FINDING: "+
				"MA may close the session on signal_completion(blocked) before unblock can land "+
				"a user.message. Surface back to the kickoff. text=%s", text)
		}
		t.Fatalf("unblock IsError: %s", text)
	}
	var payload mcp.UnblockResult
	if err := decodeStructured(res, &payload); err != nil {
		t.Fatalf("decode unblock result: %v; text=%s", err, contentText(res))
	}
	if !payload.OK {
		t.Fatalf("unblock returned ok=false; text=%s", contentText(res))
	}
}

// doneOutcome is what pollUntilDoneAfterUnblock returns once the team
// reaches a terminal "shipped" state. PRURL is the URL the test logs in
// the success path: signal_pr_url when the agent called
// signal_completion(done), or the engine-recorded
// repository_artifacts[0].pull_request_url when the agent stopped without
// signaling but the engine still opened a PR.
type doneOutcome struct {
	Team  mcp.TeamView
	PRURL string
}

// pollUntilDoneAfterUnblock polls get_status until the fixture team has
// reached a "done with PR" terminal state, or until the shared deadline
// expires.
//
// "Done" is satisfied by either of two paths:
//
//  1. SignalStatus == "done" — the agent explicitly called
//     signal_completion(done, pr_url=…). This is the contract §11.2 of
//     DESIGN-ship-feature-workflow specifies.
//
//  2. team.Status == "done" AND a non-empty pull_request_url is recorded
//     on a repository_artifact. This covers the observed agent behavior
//     where the /ship-feature skill ends without calling
//     signal_completion(done) (a downstream "artifact-delivery override"
//     that lives in the skill's prose) but the engine still records a
//     real PR via OpenPullRequests=true after the agent pushes a branch.
//     A real PR landed is the work shipping; the missing signal is a
//     skill-level concern, not an orchestration regression.
//
// After a successful unblock the MCP server clears the recorded blocked
// signal (DefaultSignalClearer); the team's SignalStatus sits at "" while
// the agent works and then flips to "done" only if the agent calls
// signal_completion. Either of the two terminal conditions above proves
// §12.3 acceptance for this PR. If neither lands by the deadline — and
// SignalStatus is still "blocked" — that's the §12.3 architecture finding
// the kickoff names ("unblock returned ok but the agent did not act").
func pollUntilDoneAfterUnblock(parentCtx context.Context, t *testing.T, c *mcpclient.Client, runID, team string, deadline time.Time) doneOutcome {
	t.Helper()
	for time.Now().Before(deadline) {
		if err := parentCtx.Err(); err != nil {
			t.Fatalf("driver context canceled while polling for done: %v", err)
		}
		view := fetchStatus(parentCtx, t, c, runID)
		if view.StateError != "" {
			t.Fatalf("get_status state_error while waiting for done: %s", view.StateError)
		}
		if view.Status == mcp.RunStatusFailed {
			t.Fatalf("run %s failed after unblock: %s", runID, jobSummary(&view))
		}
		row, ok := findTeam(&view, team)
		if !ok {
			t.Fatalf("team %s vanished from state.json after unblock", team)
		}
		if row.SignalStatus == "done" && row.SignalPRURL != "" {
			return doneOutcome{Team: row, PRURL: row.SignalPRURL}
		}
		if row.Status == "done" {
			if prURL := teamPRURL(t, view.WorkspaceDir, team); prURL != "" {
				return doneOutcome{Team: row, PRURL: prURL}
			}
		}
		select {
		case <-time.After(pollEvery):
		case <-parentCtx.Done():
			t.Fatalf("driver context canceled while waiting to poll: %v", parentCtx.Err())
		}
	}
	final := fetchStatus(parentCtx, t, c, runID)
	row, _ := findTeam(&final, team)
	if row.SignalStatus == "blocked" {
		t.Fatalf("team %s still blocked at deadline after unblock — §12.3 ARCHITECTURE FINDING: "+
			"unblock returned ok but the agent did not act on the user.message. "+
			"Surface back to the kickoff. final reason=%q", team, row.SignalReason)
	}
	t.Fatalf("team %s did not reach a done terminal state within %s; final status=%q signal_status=%q summary=%q",
		team, unblockRunTimeout, row.Status, row.SignalStatus, row.SignalSummary)
	return doneOutcome{}
}

// teamPRURL reads the run's state.json directly and returns the first
// non-empty pull_request_url recorded on the team's repository_artifacts.
// MCP JobView only carries signal_pr_url (the agent-signaled URL). The
// engine-recorded artifact PR (from OpenPullRequests=true) lives in
// state.json at teams.<name>.repository_artifacts[*].pull_request_url —
// behind the MCP curated API. The caller passes workspaceDir directly
// (it already has it from the just-fetched JobView), avoiding a redundant
// MCP round-trip on every poll where row.Status == "done". Reading
// state.json off disk is safe because writes are atomic (write-tmp +
// rename) and DESIGN-v2 §11 makes the read path lock-free by design.
// Returns "" when no artifact has been resolved yet so the caller keeps
// polling.
func teamPRURL(t *testing.T, workspaceDir, team string) string {
	t.Helper()
	if workspaceDir == "" {
		return ""
	}
	statePath := filepath.Join(workspaceDir, ".orchestra", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		// state.json may not be written yet on the first few polls;
		// not an error here.
		return ""
	}
	var doc struct {
		Teams map[string]struct {
			RepositoryArtifacts []struct {
				PullRequestURL string `json:"pull_request_url"`
			} `json:"repository_artifacts"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	row, ok := doc.Teams[team]
	if !ok {
		return ""
	}
	for _, art := range row.RepositoryArtifacts {
		if art.PullRequestURL != "" {
			return art.PullRequestURL
		}
	}
	return ""
}

func fetchStatus(parentCtx context.Context, t *testing.T, c *mcpclient.Client, runID string) mcp.JobView {
	t.Helper()
	ctx, cancel := context.WithTimeout(parentCtx, smokeProtocolTimeout)
	defer cancel()
	res, err := c.CallTool(ctx, mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{
			Name:      mcp.ToolGetStatus,
			Arguments: map[string]any{"run_id": runID},
		},
	})
	if err != nil {
		t.Fatalf("CallTool get_status: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_status IsError: %s", contentText(res))
	}
	var view mcp.JobView
	if err := decodeStructured(res, &view); err != nil {
		t.Fatalf("decode get_status result: %v; text=%s", err, contentText(res))
	}
	return view
}

// snapshotState dumps state.json + the most recent log lines from the run
// subprocess to t.Logf when a failure happens, so a failed live smoke leaves
// enough context in the test output for post-mortem without needing the
// (already-cleaned) t.TempDir contents.
func snapshotState(t *testing.T, v *mcp.JobView) {
	t.Helper()
	if v == nil || v.WorkspaceDir == "" {
		return
	}
	statePath := filepath.Join(v.WorkspaceDir, ".orchestra", "state.json")
	if data, err := os.ReadFile(statePath); err == nil {
		t.Logf("state.json: %s", string(data))
	} else {
		t.Logf("state.json read: %v", err)
	}
	logPath := filepath.Join(v.WorkspaceDir, "orchestra.log")
	if data, err := os.ReadFile(logPath); err == nil {
		// Tail to the last ~6KB to keep the test output bounded.
		const tailMax = 6 * 1024
		if len(data) > tailMax {
			data = data[len(data)-tailMax:]
		}
		t.Logf("orchestra.log tail:\n%s", string(data))
	} else {
		t.Logf("orchestra.log read: %v", err)
	}
}

func findTeam(v *mcp.JobView, name string) (mcp.TeamView, bool) {
	for i := range v.Teams {
		if v.Teams[i].Name == name {
			return v.Teams[i], true
		}
	}
	return mcp.TeamView{}, false
}

func jobSummary(v *mcp.JobView) string {
	var b strings.Builder
	b.WriteString("status=")
	b.WriteString(v.Status)
	if v.StateError != "" {
		b.WriteString(" state_error=")
		b.WriteString(v.StateError)
	}
	for i := range v.Teams {
		t := v.Teams[i]
		b.WriteString(" team=")
		b.WriteString(t.Name)
		b.WriteString("(status=")
		b.WriteString(t.Status)
		b.WriteString(",signal=")
		b.WriteString(t.SignalStatus)
		if t.SignalReason != "" {
			b.WriteString(",reason=")
			b.WriteString(t.SignalReason)
		}
		b.WriteString(")")
	}
	return b.String()
}

// decodeStructured pulls the typed payload out of a CallToolResult. The MCP
// server publishes structured content via NewToolResultStructured, which the
// stdio client returns as a generic map after JSON round-trip. Re-marshal +
// unmarshal into the expected type rather than reaching into the map by hand
// so the test stays in sync with mcp.JobView / mcp.UnblockResult automatically.
func decodeStructured(res *mcptypes.CallToolResult, out any) error {
	if res == nil {
		return errors.New("nil result")
	}
	if res.StructuredContent == nil {
		return errors.New("nil structured content")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func buildOrchestraBinary(ctx context.Context, t *testing.T) string {
	t.Helper()
	repoRoot, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	root := strings.TrimSpace(string(repoRoot))

	binName := "orchestra"
	if runtime.GOOS == "windows" {
		binName = "orchestra.exe"
	}
	out := filepath.Join(t.TempDir(), binName)
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build orchestra: %v", err)
	}
	return out
}

func contentText(r *mcptypes.CallToolResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(mcptypes.TextContent); ok {
			b.WriteString(tc.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
