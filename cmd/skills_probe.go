package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/itsHabib/orchestra/internal/machost"
	"github.com/itsHabib/orchestra/internal/skills"
)

const (
	probeAgentPrefix = "orchestra-skills-probe"
	probeEnvPrefix   = "orchestra-skills-probe"
	probePollEvery   = 3 * time.Second
	probeMaxWait     = 8 * time.Minute
)

// runSkillsProbeMount is the inline P0 probe. It uploads SKILL.md, creates a
// fresh env + agent, opens a session attaching the file as a resource, then
// asks the agent to dump enough container state to confirm where the file
// landed in the filesystem.
//
// The probe is hidden from the default --help (skills probe-mount) because it
// is one-off verification, not user-facing day-to-day behavior. Findings are
// captured by the operator and baked into the engine's bootstrap prompt in P2.
func runSkillsProbeMount(ctx context.Context, name string) error {
	source, err := skills.ResolveSource(name, skillsProbeFrom)
	if err != nil {
		return err
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("skills probe-mount: source %s: %w", source, err)
	}

	client, err := machost.NewClient()
	if err != nil {
		return fmt.Errorf("skills probe-mount: %w", err)
	}
	svc := skills.New(skills.NewFileCache(skills.DefaultCachePath()), &client.Beta.Files)

	upload, err := svc.Upload(ctx, name, source)
	if err != nil {
		return fmt.Errorf("skills probe-mount: upload: %w", err)
	}
	fmt.Printf("uploaded %s as %s (filename=%s)\n", name, upload.Entry.FileID, upload.Entry.Filename)

	env, err := createProbeEnv(ctx, &client)
	if err != nil {
		return fmt.Errorf("skills probe-mount: %w", err)
	}
	defer cleanupEnv(ctx, &client, env.ID)
	fmt.Printf("created environment %s\n", env.ID)

	agent, err := createProbeAgent(ctx, &client)
	if err != nil {
		return fmt.Errorf("skills probe-mount: %w", err)
	}
	defer cleanupAgent(ctx, &client, agent.ID)
	fmt.Printf("created agent %s\n", agent.ID)

	sess, err := createProbeSession(ctx, &client, env.ID, agent.ID, upload.Entry.FileID)
	if err != nil {
		return fmt.Errorf("skills probe-mount: %w", err)
	}
	fmt.Printf("created session %s\n", sess.ID)

	if err := sendProbePrompt(ctx, &client, sess.ID, upload.Entry.Filename); err != nil {
		return fmt.Errorf("skills probe-mount: %w", err)
	}
	if err := waitForIdle(ctx, &client, sess.ID); err != nil {
		return fmt.Errorf("skills probe-mount: %w", err)
	}

	if err := dumpEvents(ctx, &client, sess.ID); err != nil {
		return fmt.Errorf("skills probe-mount: dump events: %w", err)
	}
	return nil
}

func createProbeEnv(ctx context.Context, client *anthropic.Client) (*anthropic.BetaEnvironment, error) {
	name := probeEnvPrefix + "-" + time.Now().UTC().Format("20060102T150405Z")
	env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name: name,
		Config: anthropic.BetaCloudConfigParams{
			Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{
				OfLimited: &anthropic.BetaLimitedNetworkParams{
					AllowPackageManagers: anthropic.Bool(false),
					AllowedHosts:         []string{"api.anthropic.com"},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create environment: %w", err)
	}
	return env, nil
}

func createProbeAgent(ctx context.Context, client *anthropic.Client) (*anthropic.BetaManagedAgentsAgent, error) {
	model := anthropic.BetaManagedAgentsModelClaudeSonnet4_6
	if skillsProbeModel != "" {
		model = skillsProbeModel
	}
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name: probeAgentPrefix + "-" + time.Now().UTC().Format("20060102T150405Z"),
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID:    model,
			Speed: anthropic.BetaManagedAgentsModelConfigParamsSpeedStandard,
		},
		System: anthropic.String(
			"You are a probe agent. Run the requested bash command and report the full output verbatim. " +
				"Do not interpret, summarize, or modify the output. End with the literal token DONE on its own line.",
		),
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
		return nil, fmt.Errorf("create agent: %w", err)
	}
	return agent, nil
}

func createProbeSession(ctx context.Context, client *anthropic.Client, envID, agentID, fileID string) (*anthropic.BetaManagedAgentsSession, error) {
	resource := &anthropic.BetaManagedAgentsFileResourceParams{
		FileID: fileID,
		Type:   anthropic.BetaManagedAgentsFileResourceParamsTypeFile,
	}
	if skillsProbeMount != "" {
		resource.MountPath = anthropic.String(skillsProbeMount)
	}
	sess, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: anthropic.String(agentID),
		},
		EnvironmentID: envID,
		Title:         anthropic.String("orchestra-skills-probe"),
		Resources: []anthropic.BetaSessionNewParamsResourceUnion{
			{OfFile: resource},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

func sendProbePrompt(ctx context.Context, client *anthropic.Client, sessionID, filename string) error {
	prompt := strings.ReplaceAll(probePrompt, "<FILENAME>", filename)
	_, err := client.Beta.Sessions.Events.Send(ctx, sessionID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{
						Type: anthropic.BetaManagedAgentsTextBlockTypeText,
						Text: prompt,
					},
				}},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("send user message: %w", err)
	}
	return nil
}

// waitForIdle waits until the session has transitioned from running back to
// idle. Treating an immediate-idle status as "done" would race with the
// session's startup — `Send` returns before the agent picks up the message,
// so the first Get can return "idle" before any work has happened.
func waitForIdle(ctx context.Context, client *anthropic.Client, sessionID string) error {
	deadline := time.Now().Add(probeMaxWait)
	sawRunning := false
	for time.Now().Before(deadline) {
		cur, err := client.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{})
		if err != nil {
			return fmt.Errorf("get session: %w", err)
		}
		status := string(cur.Status)
		switch {
		case status == "running":
			sawRunning = true
		case status == "terminated":
			fmt.Printf("session %s -> terminated\n", sessionID)
			return nil
		case status == "idle" && sawRunning:
			fmt.Printf("session %s -> idle\n", sessionID)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(probePollEvery):
		}
	}
	return fmt.Errorf("session %s did not reach running→idle within %s", sessionID, probeMaxWait)
}

func dumpEvents(ctx context.Context, client *anthropic.Client, sessionID string) error {
	pager := client.Beta.Sessions.Events.ListAutoPaging(ctx, sessionID, anthropic.BetaSessionEventListParams{
		Order: anthropic.BetaSessionEventListParamsOrderAsc,
	})
	fmt.Println("\n=== session events ===")
	for pager.Next() {
		evt := pager.Current()
		body := strings.TrimSpace(eventBody(&evt))
		if body == "" {
			continue
		}
		fmt.Printf("--- %s ---\n%s\n", evt.Type, body)
	}
	return pager.Err()
}

// eventBody returns the body to print per event type. It dispatches on the
// event's type so the JSON union's overlapping content arrays don't duplicate
// the same text block multiple times. Unknown types fall through to raw JSON
// so the probe never silently drops anything.
func eventBody(evt *anthropic.BetaManagedAgentsSessionEventUnion) string {
	switch evt.Type {
	case "user.message":
		return readTextBlocks(evt.Content.OfBetaManagedAgentsUserMessageEventContentArray, func(b *anthropic.BetaManagedAgentsUserMessageEventContentUnion) (string, string) {
			return b.Type, b.Text
		})
	case "agent.message":
		return readTextBlocks(evt.Content.OfBetaManagedAgentsTextBlockArray, func(b *anthropic.BetaManagedAgentsTextBlock) (string, string) {
			return string(b.Type), b.Text
		})
	case "agent.tool_use":
		input, _ := json.Marshal(evt.Input)
		return fmt.Sprintf("[tool_use name=%s] %s", evt.Name, input)
	case "agent.tool_result":
		return readTextBlocks(evt.Content.OfBetaManagedAgentsAgentToolResultEventContentArray, func(b *anthropic.BetaManagedAgentsAgentToolResultEventContentUnion) (string, string) {
			return b.Type, b.Text
		})
	case "agent.thinking":
		return ""
	default:
		return evt.RawJSON()
	}
}

// readTextBlocks collects text-typed entries from one of the SDK's content
// arrays into a single string. The selector returns each block's type and
// text so this works across the SDK's overlapping content variants.
func readTextBlocks[T any](blocks []T, sel func(*T) (typ, text string)) string {
	var b strings.Builder
	for i := range blocks {
		typ, text := sel(&blocks[i])
		if typ == "text" && text != "" {
			b.WriteString(text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func cleanupEnv(ctx context.Context, client *anthropic.Client, envID string) {
	if skillsProbeKeep || envID == "" {
		return
	}
	if _, err := client.Beta.Environments.Archive(ctx, envID, anthropic.BetaEnvironmentArchiveParams{}); err != nil {
		fmt.Fprintf(os.Stderr, "archive environment %s: %v\n", envID, err)
		return
	}
	fmt.Printf("archived environment %s\n", envID)
}

func cleanupAgent(ctx context.Context, client *anthropic.Client, agentID string) {
	if skillsProbeKeep || agentID == "" {
		return
	}
	if _, err := client.Beta.Agents.Archive(ctx, agentID, anthropic.BetaAgentArchiveParams{}); err != nil {
		fmt.Fprintf(os.Stderr, "archive agent %s: %v\n", agentID, err)
		return
	}
	fmt.Printf("archived agent %s\n", agentID)
}

const probePrompt = `Run this exact bash command using the bash tool, then report the complete output verbatim:

cat <<'EOF' | bash
echo "=== pwd ==="; pwd
echo "=== ls / ==="; ls /
echo "=== ls /mnt ==="; ls -la /mnt 2>&1
echo "=== ls /mnt/session ==="; ls -laR /mnt/session 2>&1
echo "=== ls /workspace ==="; ls -la /workspace 2>&1
echo "=== ls /files ==="; ls -la /files 2>&1
echo "=== find for <FILENAME> ==="; find / -name '<FILENAME>' -not -path '/proc/*' -not -path '/sys/*' 2>/dev/null
echo "=== env keys ==="; env | cut -d= -f1 | sort
EOF

When done, write DONE on its own line.`
