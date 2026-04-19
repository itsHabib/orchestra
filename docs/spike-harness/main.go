// Spike harness for Managed Agents I/O model.
// See docs/SPIKE-ma-io.md for the test matrix and questions under test.
//
// Usage:
//
//	spike setup            # create env + agent (idempotent; stores IDs in state.json)
//	spike t1               # file visibility via Files.List(scope_id=session_id)
//	spike t2               # Files.Download content matches what the agent wrote
//	spike t3               # publish via `anthropic files upload` from Bash
//	spike t4               # publish via curl from Bash
//	spike t5               # github_repository resource mount + branch push
//	spike t6               # downstream session sees the pushed branch
//	spike t7               # raw git-sync fallback (no repo resource)
//	spike t8               # 500-file Files-API mount ceiling
//	spike teardown         # archive sessions, delete env + agent
//
// Every test writes docs/spike-output/ma-io/<test-id>.json with the captured
// request/response pairs; the findings doc cites those files.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	outputDir = "../spike-output/ma-io"
	stateFile = "../spike-output/ma-io/state.json"
	envName   = "orchestra-spike-ma-io"
	agentName = "orchestra-spike-agent"
)

type harnessState struct {
	EnvironmentID string `json:"environment_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	AgentVersion  int    `json:"agent_version,omitempty"`
	LastSessionID string `json:"last_session_id,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client := anthropic.NewClient()

	cmd := os.Args[1]
	var err error
	switch cmd {
	case "setup":
		err = cmdSetup(ctx, client)
	case "setup2":
		err = cmdSetup2(ctx, client)
	case "t1":
		err = runTest(ctx, client, "t1", t1FileVisibility)
	case "t1b":
		err = runTest(ctx, client, "t1b", t1bFileVisibilityCorrectBeta)
	case "probe":
		err = runTest(ctx, client, "probe", probeContainer)
	case "t2":
		err = runTest(ctx, client, "t2", t2FileDownload)
	case "t3":
		err = runTest(ctx, client, "t3", t3PublishViaAnthropicCLI)
	case "t4":
		err = runTest(ctx, client, "t4", t4PublishViaCurl)
	case "t5", "t6", "t7", "t8":
		err = fmt.Errorf("%s not yet implemented in this pass", cmd)
	case "teardown":
		err = cmdTeardown(ctx, client)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: spike <setup|t1|t2|t3|t4|t5|t6|t7|t8|teardown>")
}

func loadState() (*harnessState, error) {
	b, err := os.ReadFile(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return &harnessState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s harnessState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func saveState(s *harnessState) error {
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, stateFile)
}

// fixture dumps a named payload under docs/spike-output/ma-io/<test>/<name>.json.
func fixture(test, name string, payload any) error {
	dir := filepath.Join(outputDir, test)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644)
}

// -----------------------------------------------------------------------------
// Setup / teardown
// -----------------------------------------------------------------------------

func cmdSetup(ctx context.Context, client anthropic.Client) error {
	state, err := loadState()
	if err != nil {
		return err
	}
	if state.EnvironmentID == "" {
		env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
			Name: envName,
			Config: anthropic.BetaCloudConfigParams{
				Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{
					OfLimited: &anthropic.BetaLimitedNetworkParams{
						AllowPackageManagers: anthropic.Bool(true),
						AllowedHosts: []string{
							"api.anthropic.com",
							"github.com",
							"objects.githubusercontent.com",
							"codeload.github.com",
						},
					},
				},
			},
		})
		if err != nil {
			return fmt.Errorf("create environment: %w", err)
		}
		state.EnvironmentID = env.ID
		if err := fixture("setup", "environment", env); err != nil {
			return err
		}
		fmt.Printf("created environment %s\n", env.ID)
	} else {
		fmt.Printf("reusing environment %s\n", state.EnvironmentID)
	}

	if state.AgentID == "" {
		agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
			Name: agentName,
			Model: anthropic.BetaManagedAgentsModelConfigParams{
				ID:    anthropic.BetaManagedAgentsModelClaudeSonnet4_6,
				Speed: anthropic.BetaManagedAgentsModelConfigParamsSpeedStandard,
			},
			System: anthropic.String("You are a spike-test agent. When the user asks you to do something with files, do exactly what they ask, then say DONE. Be concise."),
			Tools: []anthropic.BetaAgentNewParamsToolUnion{{
				OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
					Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
					Configs: []anthropic.BetaManagedAgentsAgentToolConfigParams{{
						Name:    anthropic.BetaManagedAgentsAgentToolConfigParamsNameBash,
						Enabled: anthropic.Bool(true),
					}},
				},
			}},
		})
		if err != nil {
			return fmt.Errorf("create agent: %w", err)
		}
		state.AgentID = agent.ID
		if err := fixture("setup", "agent", agent); err != nil {
			return err
		}
		fmt.Printf("created agent %s\n", agent.ID)
	} else {
		fmt.Printf("reusing agent %s\n", state.AgentID)
	}

	return saveState(state)
}

// cmdSetup2 creates a second agent with the FULL agent_toolset_20260401
// (bash + read + write + edit + glob + grep + web_*), replacing the
// bash-only agent for follow-up tests.
func cmdSetup2(ctx context.Context, client anthropic.Client) error {
	state, err := loadState()
	if err != nil {
		return err
	}
	if state.EnvironmentID == "" {
		return fmt.Errorf("run `spike setup` first to create the env")
	}
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name: agentName + "-fulltools",
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID:    anthropic.BetaManagedAgentsModelClaudeSonnet4_6,
			Speed: anthropic.BetaManagedAgentsModelConfigParamsSpeedStandard,
		},
		System: anthropic.String("You are a spike-test agent. When the user asks you to do something with files, do exactly what they ask using the dedicated file tools (write/edit) where possible — not bash echo redirects. Then say DONE. Be concise."),
		Tools: []anthropic.BetaAgentNewParamsToolUnion{{
			OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
				Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
				// no Configs => all tools enabled by default
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	state.AgentID = agent.ID
	if err := fixture("setup2", "agent", agent); err != nil {
		return err
	}
	fmt.Printf("created full-toolset agent %s\n", agent.ID)
	return saveState(state)
}

func cmdTeardown(ctx context.Context, client anthropic.Client) error {
	state, err := loadState()
	if err != nil {
		return err
	}
	if state.AgentID != "" {
		if _, err := client.Beta.Agents.Archive(ctx, state.AgentID, anthropic.BetaAgentArchiveParams{}); err != nil {
			fmt.Fprintf(os.Stderr, "archive agent: %v\n", err)
		} else {
			fmt.Printf("archived agent %s\n", state.AgentID)
		}
	}
	if state.EnvironmentID != "" {
		if _, err := client.Beta.Environments.Archive(ctx, state.EnvironmentID, anthropic.BetaEnvironmentArchiveParams{}); err != nil {
			fmt.Fprintf(os.Stderr, "archive environment: %v\n", err)
		} else {
			fmt.Printf("archived environment %s\n", state.EnvironmentID)
		}
	}
	state.AgentID = ""
	state.EnvironmentID = ""
	state.LastSessionID = ""
	return saveState(state)
}

// -----------------------------------------------------------------------------
// Test driver
// -----------------------------------------------------------------------------

type testFn func(ctx context.Context, client anthropic.Client, state *harnessState) error

func runTest(ctx context.Context, client anthropic.Client, name string, fn testFn) error {
	state, err := loadState()
	if err != nil {
		return err
	}
	if state.EnvironmentID == "" || state.AgentID == "" {
		return fmt.Errorf("run `spike setup` first")
	}
	fmt.Printf("== %s ==\n", name)
	if err := fn(ctx, client, state); err != nil {
		return err
	}
	return saveState(state)
}

// startSessionAndSend opens a session, streams events, sends an initial
// user.message per the stream-first ordering in DESIGN-v2 §6, and blocks until
// the session transitions to idle.
func startSessionAndSend(ctx context.Context, client anthropic.Client, state *harnessState, test, prompt string) (sessionID string, events []any, err error) {
	sess, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: anthropic.String(state.AgentID),
		},
		EnvironmentID: state.EnvironmentID,
		Title:         anthropic.String("spike-" + test),
	})
	if err != nil {
		return "", nil, fmt.Errorf("create session: %w", err)
	}
	sessionID = sess.ID
	state.LastSessionID = sessionID
	_ = fixture(test, "session-created", sess)
	fmt.Printf("  session %s\n", sessionID)

	// Send initial user message. We are not opening an SSE stream in this
	// spike — we poll events via List instead, which is sufficient to answer
	// Q1/Q2. The design's stream-first ordering is a P1.4 concern.
	_, err = client.Beta.Sessions.Events.Send(ctx, sessionID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{
						Text: prompt,
						Type: anthropic.BetaManagedAgentsTextBlockTypeText,
					},
				}},
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
			},
		}},
	})
	if err != nil {
		return sessionID, nil, fmt.Errorf("send user message: %w", err)
	}

	// Poll until idle. Guard with the harness's outer context timeout.
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		cur, err := client.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{})
		if err != nil {
			return sessionID, nil, fmt.Errorf("get session: %w", err)
		}
		status := string(cur.Status)
		if status == "idle" || status == "terminated" {
			_ = fixture(test, "session-final", cur)
			fmt.Printf("  session %s -> %s\n", sessionID, status)
			break
		}
		time.Sleep(3 * time.Second)
	}

	// Snapshot the event list. Dump raw JSON — the event union is large and we
	// care about seeing every field for the findings doc, not cherry-picking.
	list, err := client.Beta.Sessions.Events.List(ctx, sessionID, anthropic.BetaSessionEventListParams{})
	if err != nil {
		return sessionID, nil, fmt.Errorf("list events: %w", err)
	}
	_ = fixture(test, "events", list)
	return sessionID, nil, nil
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// probe cats useful container state for the findings doc.
func probeContainer(ctx context.Context, client anthropic.Client, state *harnessState) error {
	prompt := `Run this exact bash command and report the full output verbatim:

cat <<'EOF' | bash
echo "=== uname ==="; uname -a
echo "=== whoami ==="; whoami
echo "=== pwd ==="; pwd
echo "=== ls / ==="; ls /
echo "=== ls /workspace ==="; ls -la /workspace 2>&1
echo "=== which git ==="; which git && git --version
echo "=== which python python3 pip ==="; which python python3 pip 2>&1
echo "=== which node npm ==="; which node npm 2>&1
echo "=== which anthropic ==="; which anthropic 2>&1
echo "=== env keys (names only) ==="; env | cut -d= -f1 | sort
echo "=== /etc/os-release ==="; cat /etc/os-release 2>&1 | head -5
EOF

Then say DONE.`
	if _, _, err := startSessionAndSend(ctx, client, state, "probe", prompt); err != nil {
		return err
	}
	return nil
}

// t1bFileVisibilityCorrectBeta retries T1 with the right beta header
// (managed-agents-2026-04-01), and writes to several candidate paths to see
// what (if anything) the Files API surfaces.
func t1bFileVisibilityCorrectBeta(ctx context.Context, client anthropic.Client, state *harnessState) error {
	prompt := `Use the dedicated 'write' tool (NOT bash echo) to create three files with these exact contents:

1. /workspace/hello-write.txt  — "hello from write tool at /workspace"
2. /workspace/out/hello-write.txt — "hello from write tool at /workspace/out"  (create the directory first via bash mkdir if needed)
3. /tmp/hello-write.txt        — "hello from write tool at /tmp"

Use the write tool for each file individually. Then say DONE.`
	sessionID, _, err := startSessionAndSend(ctx, client, state, "t1b", prompt)
	if err != nil {
		return err
	}
	files, err := client.Beta.Files.List(ctx, anthropic.BetaFileListParams{
		ScopeID: anthropic.String(sessionID),
		Betas:   []anthropic.AnthropicBeta{"managed-agents-2026-04-01"},
	})
	if err != nil {
		return fmt.Errorf("files.list: %w", err)
	}
	if err := fixture("t1b", "files-list", files); err != nil {
		return err
	}
	fmt.Printf("  files.list(scope_id=%s, beta=managed-agents) -> %d items\n", sessionID, len(files.Data))
	for _, f := range files.Data {
		fmt.Printf("    - %s (%s, %d bytes)\n", f.Filename, f.ID, f.SizeBytes)
	}
	return nil
}

// T1: does the agent's write to /workspace land in Files.List(scope_id=session_id)?
func t1FileVisibility(ctx context.Context, client anthropic.Client, state *harnessState) error {
	prompt := "Use the bash tool to write the text 'hello from spike T1' to /workspace/hello.txt. Do not upload anything. Just write the file and say DONE."
	sessionID, _, err := startSessionAndSend(ctx, client, state, "t1", prompt)
	if err != nil {
		return err
	}
	files, err := client.Beta.Files.List(ctx, anthropic.BetaFileListParams{})
	if err != nil {
		return fmt.Errorf("files.list: %w", err)
	}
	if err := fixture("t1", "files-list", files); err != nil {
		return err
	}
	fmt.Printf("  files.list(scope_id=%s) -> %d items\n", sessionID, len(files.Data))
	for _, f := range files.Data {
		fmt.Printf("    - %s (%s, %d bytes)\n", f.Filename, f.ID, f.SizeBytes)
	}
	return nil
}

// T2: downloaded content matches what the agent wrote.
// Requires that T1's session still exists and produced files.
func t2FileDownload(ctx context.Context, client anthropic.Client, state *harnessState) error {
	if state.LastSessionID == "" {
		return fmt.Errorf("no session; run t1 first")
	}
	files, err := client.Beta.Files.List(ctx, anthropic.BetaFileListParams{})
	if err != nil {
		return fmt.Errorf("files.list: %w", err)
	}
	if len(files.Data) == 0 {
		fmt.Println("  no files to download — Q1 is likely answer (b)")
		return fixture("t2", "result", map[string]any{"downloadable": 0, "note": "files.list returned empty — container writes invisible to files api"})
	}
	dl := make(map[string]any, len(files.Data))
	for _, f := range files.Data {
		resp, err := client.Beta.Files.Download(ctx, f.ID, anthropic.BetaFileDownloadParams{})
		if err != nil {
			dl[f.Filename] = map[string]string{"error": err.Error()}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200] + "...(truncated)"
		}
		dl[f.Filename] = map[string]any{
			"size":    len(body),
			"preview": preview,
		}
		fmt.Printf("  %s (%d bytes): %s\n", f.Filename, len(body), strings.ReplaceAll(preview, "\n", " "))
	}
	return fixture("t2", "result", dl)
}

// T3: agent publishes via `anthropic files upload` (if CLI is present in the env).
func t3PublishViaAnthropicCLI(ctx context.Context, client anthropic.Client, state *harnessState) error {
	prompt := `Use the bash tool:
1. First, run 'which anthropic || echo "anthropic-cli not installed"' and report the result.
2. Write the text 'hello from T3' to /workspace/t3.txt.
3. If the anthropic CLI is available, run 'anthropic files upload /workspace/t3.txt' and report the file_id.
4. If not available, just say DONE.`
	if _, _, err := startSessionAndSend(ctx, client, state, "t3", prompt); err != nil {
		return err
	}
	files, err := client.Beta.Files.List(ctx, anthropic.BetaFileListParams{})
	if err != nil {
		return err
	}
	return fixture("t3", "files-list", files)
}

// T4: agent publishes via raw curl to /v1/files.
func t4PublishViaCurl(ctx context.Context, client anthropic.Client, state *harnessState) error {
	prompt := `Use the bash tool:
1. Write the text 'hello from T4' to /workspace/t4.txt.
2. Check whether the environment variable ANTHROPIC_API_KEY is set (do NOT print its value, just say set or unset).
3. If set, run:
   curl -sS -X POST https://api.anthropic.com/v1/files \
     -H "x-api-key: $ANTHROPIC_API_KEY" \
     -H "anthropic-version: 2023-06-01" \
     -H "anthropic-beta: files-api-2025-04-14" \
     -F "file=@/workspace/t4.txt"
   and report the JSON response.
4. If not set, report that and say DONE.`
	if _, _, err := startSessionAndSend(ctx, client, state, "t4", prompt); err != nil {
		return err
	}
	files, err := client.Beta.Files.List(ctx, anthropic.BetaFileListParams{})
	if err != nil {
		return err
	}
	return fixture("t4", "files-list", files)
}
