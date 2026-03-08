package spawner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"strconv"
	"time"

	"github.com/michaelhabib/orchestra/internal/workspace"
)

// SpawnOpts configures a claude -p spawn.
type SpawnOpts struct {
	TeamName       string
	Prompt         string
	Model          string
	MaxTurns       int
	PermissionMode string
	TimeoutMinutes int
	LogWriter      io.Writer
	ProgressFunc   func(teamName, msg string) // called with progress updates for terminal display
	Command        string                     // defaults to "claude"
}

// streamEvent represents a top-level line from claude -p --output-format stream-json --verbose.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system.init
	SessionID string `json:"session_id"`

	// system.task_progress
	TaskID        string `json:"task_id,omitempty"`
	TaskToolUseID string `json:"tool_use_id,omitempty"`
	Description   string `json:"description,omitempty"`
	LastToolName  string `json:"last_tool_name,omitempty"`

	// assistant / user — content is nested in message
	Message struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	} `json:"message"`

	// result
	Result     string  `json:"result"`
	TotalCost  float64 `json:"total_cost_usd"`
	CostUSD    float64 `json:"cost_usd"` // fallback for older format
	NumTurns   int     `json:"num_turns"`
	DurationMs int64   `json:"duration_ms"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Name      string          `json:"name,omitempty"`  // tool_use name
	ID        string          `json:"id,omitempty"`    // tool_use id
	Input     json.RawMessage `json:"input,omitempty"` // tool_use input
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result reference
}

// Spawn runs claude -p with the given prompt, parses stream-json output,
// and returns the structured result. Blocks until the process exits or times out.
func Spawn(ctx context.Context, opts SpawnOpts) (*workspace.TeamResult, error) {
	cmd := opts.Command
	if cmd == "" {
		cmd = "claude"
	}

	args := []string{
		"-p", opts.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", opts.PermissionMode)
		if opts.PermissionMode == "bypassPermissions" {
			args = append(args, "--dangerously-skip-permissions")
		}
	}

	// Apply timeout via context
	if opts.TimeoutMinutes > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(opts.TimeoutMinutes)*time.Minute)
		defer cancel()
	}

	proc := exec.CommandContext(ctx, cmd, args...)

	// Strip CLAUDECODE env var so child claude processes don't refuse to start
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			env = append(env, e)
		}
	}
	proc.Env = env

	stdout, err := proc.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	var stderr bytes.Buffer
	proc.Stderr = &stderr

	if err := proc.Start(); err != nil {
		return nil, fmt.Errorf("starting process: %w", err)
	}

	progress := opts.ProgressFunc
	if progress == nil {
		progress = func(string, string) {} // no-op
	}

	startTime := time.Now()

	var (
		result         *workspace.TeamResult
		sessionID      string
		lastAssistText string
		turnCount      int
		filesWritten   int
		filesEdited    int
		bashCmds       int
	)

	// Track teammate names: tool_use_id → description from Agent tool_use events
	teammateNames := make(map[string]string)

	elapsed := func() string {
		d := time.Since(startTime).Round(time.Second)
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%02ds", m, s)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer for long lines

	for scanner.Scan() {
		line := scanner.Bytes()

		// Tee to log writer
		if opts.LogWriter != nil {
			opts.LogWriter.Write(line)
			opts.LogWriter.Write([]byte("\n"))
		}

		var evt streamEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			continue // skip non-JSON lines
		}

		switch evt.Type {
		case "system":
			if evt.Subtype == "init" {
				if evt.SessionID != "" {
					sessionID = evt.SessionID
				}
				progress(opts.TeamName, fmt.Sprintf("⏳ session started (%s)", elapsed()))
			} else if evt.Subtype == "task_progress" && evt.TaskToolUseID != "" {
				if role, ok := teammateNames[evt.TaskToolUseID]; ok {
					prefix := fmt.Sprintf("%s:%s", opts.TeamName, role)
					detail := evt.Description
					if detail == "" && evt.LastToolName != "" {
						detail = evt.LastToolName
					}
					if detail != "" {
						if len(detail) > 120 {
							detail = detail[:120] + "…"
						}
						progress(prefix, fmt.Sprintf("   %s", detail))
					}
				}
			}

		case "assistant":
			turnCount++
			for _, c := range evt.Message.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						lastAssistText = c.Text
						preview := c.Text
						// Replace newlines with spaces for single-line display
						preview = compactText(preview)
						if len(preview) > 140 {
							preview = preview[:140] + "…"
						}
						progress(opts.TeamName, fmt.Sprintf("💬 [turn %d | %s] %s", turnCount, elapsed(), preview))
					}
				case "tool_use":
					if c.Name != "" {
						detail := summarizeToolUse(c.Name, c.Input)
						progress(opts.TeamName, fmt.Sprintf("🔧 [turn %d | %s] %s", turnCount, elapsed(), detail))
						// Track teammate names from Agent tool_use
						if c.Name == "Agent" && c.ID != "" {
							var agentParams map[string]any
							if err := json.Unmarshal(c.Input, &agentParams); err == nil {
								if desc, ok := agentParams["description"].(string); ok && desc != "" {
									teammateNames[c.ID] = desc
								}
							}
						}
						// Track stats
						switch c.Name {
						case "Write":
							filesWritten++
						case "Edit":
							filesEdited++
						case "Bash":
							bashCmds++
						}
					}
				case "thinking":
					progress(opts.TeamName, fmt.Sprintf("🧠 [turn %d | %s] thinking...", turnCount, elapsed()))
				}
			}

		case "user":
			// Tool results come back as user messages — show completion
			for _, c := range evt.Message.Content {
				if c.Type == "tool_result" {
					progress(opts.TeamName, fmt.Sprintf("   [turn %d | %s] ✓ tool completed", turnCount, elapsed()))
				}
			}

		case "result":
			costUSD := evt.TotalCost
			if costUSD == 0 {
				costUSD = evt.CostUSD
			}
			sid := evt.SessionID
			if sid == "" {
				sid = sessionID
			}
			result = &workspace.TeamResult{
				Team:       opts.TeamName,
				Status:     evt.Subtype,
				Result:     evt.Result,
				CostUSD:    costUSD,
				NumTurns:   evt.NumTurns,
				DurationMs: evt.DurationMs,
				SessionID:  sid,
			}
			progress(opts.TeamName, fmt.Sprintf("✅ finished (%s) — %d turns, $%.4f, %d writes, %d edits, %d bash cmds",
				elapsed(), evt.NumTurns, costUSD, filesWritten, filesEdited, bashCmds))
		}
	}

	err = proc.Wait()

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("timeout after %d minutes", opts.TimeoutMinutes)
	}

	if result != nil {
		return result, nil
	}

	// Process exited without a result message
	if err == nil {
		// Exit code 0 but no result — use last assistant text
		return &workspace.TeamResult{
			Team:      opts.TeamName,
			Status:    "success",
			Result:    lastAssistText,
			SessionID: sessionID,
		}, nil
	}

	return nil, fmt.Errorf("process exited with error: %w; stderr: %s", err, stderr.String())
}

// CoordinatorHandle provides a non-blocking handle to a background claude -p process.
type CoordinatorHandle struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
	result *workspace.TeamResult
	err    error
}

// Wait blocks until the coordinator exits and returns its result.
func (h *CoordinatorHandle) Wait() (*workspace.TeamResult, error) {
	<-h.done
	return h.result, h.err
}

// Cancel signals the coordinator to stop.
func (h *CoordinatorHandle) Cancel() {
	h.cancel()
}

// Done returns a channel that is closed when the coordinator exits.
func (h *CoordinatorHandle) Done() <-chan struct{} {
	return h.done
}

// SpawnBackground starts a claude -p process in the background and returns immediately.
// The process runs until completion, timeout, or cancellation.
func SpawnBackground(ctx context.Context, opts SpawnOpts) (*CoordinatorHandle, error) {
	ctx, cancel := context.WithCancel(ctx)
	if opts.TimeoutMinutes > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(opts.TimeoutMinutes)*time.Minute)
	}

	handle := &CoordinatorHandle{
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go func() {
		defer close(handle.done)
		result, err := Spawn(ctx, opts)
		handle.result = result
		handle.err = err
	}()

	return handle, nil
}

// compactText replaces newlines and excess whitespace with single spaces.
func compactText(s string) string {
	out := make([]byte, 0, len(s))
	space := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' || s[i] == '\t' || s[i] == ' ' {
			if !space {
				out = append(out, ' ')
				space = true
			}
		} else {
			out = append(out, s[i])
			space = false
		}
	}
	return string(out)
}

// summarizeToolUse returns a human-readable summary of a tool invocation.
func summarizeToolUse(name string, input json.RawMessage) string {
	var params map[string]any
	if err := json.Unmarshal(input, &params); err != nil {
		return name
	}

	switch name {
	case "Write":
		if fp, ok := params["file_path"].(string); ok {
			return fmt.Sprintf("Write → %s", fp)
		}
	case "Edit":
		if fp, ok := params["file_path"].(string); ok {
			return fmt.Sprintf("Edit → %s", fp)
		}
	case "Read":
		if fp, ok := params["file_path"].(string); ok {
			return fmt.Sprintf("Read → %s", fp)
		}
	case "Bash":
		if cmd, ok := params["command"].(string); ok {
			cmd = compactText(cmd)
			if len(cmd) > 100 {
				cmd = cmd[:100] + "…"
			}
			return fmt.Sprintf("Bash → %s", cmd)
		}
	case "Glob":
		if pat, ok := params["pattern"].(string); ok {
			return fmt.Sprintf("Glob → %s", pat)
		}
	case "Grep":
		if pat, ok := params["pattern"].(string); ok {
			path := ""
			if p, ok := params["path"].(string); ok {
				path = " in " + p
			}
			return fmt.Sprintf("Grep → %s%s", pat, path)
		}
	case "Task":
		if desc, ok := params["description"].(string); ok {
			return fmt.Sprintf("Task → %s", desc)
		}
	case "TodoWrite", "TaskCreate":
		if subj, ok := params["subject"].(string); ok {
			return fmt.Sprintf("%s → %s", name, subj)
		}
	case "Agent":
		if desc, ok := params["description"].(string); ok {
			bg := ""
			if b, ok := params["run_in_background"].(bool); ok && b {
				bg = " (background)"
			}
			return fmt.Sprintf("Agent → %s%s", desc, bg)
		}
	case "TaskOutput":
		if taskID, ok := params["task_id"].(string); ok {
			short := taskID
			if len(short) > 12 {
				short = short[:12] + "…"
			}
			return fmt.Sprintf("TaskOutput → waiting on %s", short)
		}
	}

	return name
}
