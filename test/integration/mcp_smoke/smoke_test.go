// Package mcpsmoke is the binary integration smoke for the orchestra MCP
// server: a real `orchestra mcp` subprocess driven over stdio by an SDK
// client, end-to-end through Initialize → ListTools → CallTool.
//
// The smoke is opt-in and runs only when ORCHESTRA_MA_INTEGRATION=1. It does
// not require a repo or live MA — every assertion exercises the protocol path
// against an empty registry. Live-DAG smoke arrives with the run tool in the
// follow-up PR.
package mcpsmoke

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	orchestraMCP "github.com/itsHabib/orchestra/internal/mcp"
)

const smokeProtocolTimeout = 30 * time.Second

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

	ctx, cancel := context.WithTimeout(cmdCtx, smokeProtocolTimeout)
	defer cancel()

	session := startMCPClient(ctx, t, bin, registryPath, workspaceRoot)

	assertToolsAdvertised(ctx, t, session)
	assertResourcesAdvertised(ctx, t, session)
	assertEmptyListRuns(ctx, t, session)
}

func assertResourcesAdvertised(ctx context.Context, t *testing.T, c *mcp.ClientSession) {
	t.Helper()
	resources, err := c.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	var sawRuns bool
	for _, r := range resources.Resources {
		if r.URI == orchestraMCP.ResourceRunsURI {
			sawRuns = true
		}
	}
	if !sawRuns {
		t.Fatalf("ListResources missing %q", orchestraMCP.ResourceRunsURI)
	}
	templates, err := c.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	want := map[string]bool{
		orchestraMCP.ResourceRunTemplateURI:         false,
		orchestraMCP.ResourceRunMessagesTemplateURI: false,
	}
	for _, tmpl := range templates.ResourceTemplates {
		if _, ok := want[tmpl.URITemplate]; ok {
			want[tmpl.URITemplate] = true
		}
	}
	for uri, seen := range want {
		if !seen {
			t.Fatalf("ListResourceTemplates missing %q", uri)
		}
	}
}

func startMCPClient(ctx context.Context, t *testing.T, bin, registryPath, workspaceRoot string) *mcp.ClientSession {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin,
		"mcp",
		"--transport", "stdio",
		"--registry-path", registryPath,
		"--workspace-root", workspaceRoot,
	)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "orchestra-smoke", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func assertToolsAdvertised(ctx context.Context, t *testing.T, c *mcp.ClientSession) {
	t.Helper()
	tools, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		orchestraMCP.ToolListRuns:     false,
		orchestraMCP.ToolGetRun:       false,
		orchestraMCP.ToolRun:          false,
		orchestraMCP.ToolSendMessage:  false,
		orchestraMCP.ToolReadMessages: false,
	}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("ListTools missing %q", name)
		}
	}
}

func assertEmptyListRuns(ctx context.Context, t *testing.T, c *mcp.ClientSession) {
	t.Helper()
	res, err := c.CallTool(ctx, &mcp.CallToolParams{Name: orchestraMCP.ToolListRuns})
	if err != nil {
		t.Fatalf("CallTool list_runs: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_runs IsError: %s", contentText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out orchestraMCP.ListRunsResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, raw)
	}
	if len(out.Runs) != 0 {
		t.Fatalf("runs: %d, want 0", len(out.Runs))
	}
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

func contentText(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
