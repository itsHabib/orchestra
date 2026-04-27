// Package mcpsmoke is the live-MA integration smoke for the P3 MCP server:
// orchestra binary spawned as `orchestra mcp` over stdio, an MCP client driven
// from this Go test, end-to-end through Initialize → ListTools → CallTool.
//
// The smoke is opt-in: it runs only when ORCHESTRA_MA_INTEGRATION=1. The
// protocol path (Initialize, ListTools, CallTool list_jobs) does not require
// a repo and runs whenever the env var is set.
//
// The "live ship" portion — calling ship_design_docs against a real repo and
// polling until status=done — additionally requires ORCHESTRA_MCP_TEST_REPO_URL
// pointing at a sandbox the user is willing to push to and a reachable
// ANTHROPIC_API_KEY. When that env var is absent the test logs how to enable
// it and stops after the protocol smoke. The full GitHub fixture is P4 work.
package mcpsmoke

import (
	"context"
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
	smokeProtocolTimeout = 30 * time.Second
	// Live runs against a real /ship-feature workflow can take well into
	// the per-team default (90 minutes). Cap at 30 minutes for the smoke;
	// anything slower is a P4 fixture concern, not a P3 smoke.
	liveRunTimeout = 30 * time.Minute
	livePollEvery  = 30 * time.Second
)

// liveShip carries the env-driven inputs for the optional live-ship phase.
type liveShip struct {
	repoURL string
	docPath string
}

func TestMCPProtocolSmoke_LiveBinary(t *testing.T) {
	if os.Getenv("ORCHESTRA_MA_INTEGRATION") != "1" {
		t.Skip("set ORCHESTRA_MA_INTEGRATION=1 to enable")
	}

	cmdCtx, cancelCmd := context.WithCancel(context.Background())
	t.Cleanup(cancelCmd)

	bin := buildOrchestraBinary(cmdCtx, t)
	dataRoot := t.TempDir()
	registryPath := filepath.Join(dataRoot, "mcp-runs.json")
	workspaceRoot := filepath.Join(dataRoot, "runs")
	client := startMCPClient(t, bin, registryPath, workspaceRoot)

	ctx, cancel := context.WithTimeout(cmdCtx, smokeProtocolTimeout)
	defer cancel()

	if _, err := client.Initialize(ctx, mcptypes.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	assertToolsAdvertised(ctx, t, client)
	assertEmptyListJobs(ctx, t, client)

	live, ok := liveShipParams(t)
	if !ok {
		return
	}
	runID := callLiveShip(ctx, t, client, live)
	pollUntilDone(cmdCtx, t, client, runID)
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

func assertToolsAdvertised(ctx context.Context, t *testing.T, c *mcpclient.Client) {
	t.Helper()
	tools, err := c.ListTools(ctx, mcptypes.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		mcp.ToolShipDesignDocs: false,
		mcp.ToolListJobs:       false,
		mcp.ToolGetStatus:      false,
		mcp.ToolUnblock:        false,
	}
	for i := range tools.Tools {
		if _, ok := want[tools.Tools[i].Name]; ok {
			want[tools.Tools[i].Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("ListTools missing %q", name)
		}
	}
}

func assertEmptyListJobs(ctx context.Context, t *testing.T, c *mcpclient.Client) {
	t.Helper()
	res, err := c.CallTool(ctx, mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{Name: mcp.ToolListJobs},
	})
	if err != nil {
		t.Fatalf("CallTool list_jobs: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_jobs IsError: %s", contentText(res))
	}
}

// liveShipParams returns the repo URL and doc path for the live ship phase,
// or ok=false when one or both env vars are unset. A missing repo is a soft
// skip with a Logf so the protocol smoke still runs in the common case; a
// repo-without-doc is a hard t.Skip because the test would otherwise need to
// invent a doc path.
func liveShipParams(t *testing.T) (liveShip, bool) {
	t.Helper()
	repoURL := os.Getenv("ORCHESTRA_MCP_TEST_REPO_URL")
	if repoURL == "" {
		t.Logf("skipping live ship_design_docs path: " +
			"set ORCHESTRA_MCP_TEST_REPO_URL=https://github.com/<owner>/<repo> to enable")
		return liveShip{}, false
	}
	docPath := os.Getenv("ORCHESTRA_MCP_TEST_DOC_PATH")
	if docPath == "" {
		t.Skip("set ORCHESTRA_MCP_TEST_DOC_PATH=<repo-relative path> alongside ORCHESTRA_MCP_TEST_REPO_URL")
	}
	return liveShip{repoURL: repoURL, docPath: docPath}, true
}

func callLiveShip(ctx context.Context, t *testing.T, c *mcpclient.Client, live liveShip) string {
	t.Helper()
	res, err := c.CallTool(ctx, mcptypes.CallToolRequest{
		Params: mcptypes.CallToolParams{
			Name: mcp.ToolShipDesignDocs,
			Arguments: map[string]any{
				"paths":    []any{live.docPath},
				"repo_url": live.repoURL,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool ship_design_docs: %v", err)
	}
	if res.IsError {
		t.Fatalf("ship_design_docs IsError: %s", contentText(res))
	}
	runID := extractRunID(t, res)
	t.Logf("ship_design_docs returned run_id=%s", runID)
	return runID
}

func pollUntilDone(parentCtx context.Context, t *testing.T, c *mcpclient.Client, runID string) {
	t.Helper()
	deadline := time.Now().Add(liveRunTimeout)
	for time.Now().Before(deadline) {
		if err := parentCtx.Err(); err != nil {
			t.Fatalf("driver context canceled while polling: %v", err)
		}
		statusCtx, statusCancel := context.WithTimeout(parentCtx, smokeProtocolTimeout)
		res, err := c.CallTool(statusCtx, mcptypes.CallToolRequest{
			Params: mcptypes.CallToolParams{
				Name:      mcp.ToolGetStatus,
				Arguments: map[string]any{"run_id": runID},
			},
		})
		statusCancel()
		if err != nil {
			t.Fatalf("CallTool get_status: %v", err)
		}
		if res.IsError {
			t.Fatalf("get_status IsError: %s", contentText(res))
		}
		text := contentText(res)
		switch {
		case strings.Contains(text, mcp.RunStatusDone):
			return
		case strings.Contains(text, mcp.RunStatusFailed):
			t.Fatalf("run %s failed: %s", runID, text)
		case strings.Contains(text, mcp.RunStatusBlocked):
			t.Fatalf("run %s blocked (manual unblock not exercised by smoke): %s", runID, text)
		}
		time.Sleep(livePollEvery)
	}
	t.Fatalf("run %s did not reach status=done within %s", runID, liveRunTimeout)
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

// extractRunID returns the run_id from the structured response of
// ship_design_docs. The fallback text encodes "run <id> started in <dir>", so
// even clients that drop StructuredContent during JSON round-trip can recover
// the id.
func extractRunID(t *testing.T, r *mcptypes.CallToolResult) string {
	t.Helper()
	text := contentText(r)
	const prefix = "run "
	idx := strings.Index(text, prefix)
	if idx < 0 {
		t.Fatalf("could not extract run_id from result text: %q", text)
	}
	rest := text[idx+len(prefix):]
	end := strings.Index(rest, " ")
	if end < 0 {
		t.Fatalf("malformed result text: %q", text)
	}
	return rest[:end]
}
