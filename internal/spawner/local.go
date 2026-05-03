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
	"strconv"
	"strings"
	"time"

	"github.com/itsHabib/orchestra/internal/workspace"
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

// LocalSubprocessSpawner adapts the existing claude -p subprocess backend.
type LocalSubprocessSpawner struct {
	Command string
}

// NewLocalSubprocessSpawner returns a local subprocess backend.
func NewLocalSubprocessSpawner() *LocalSubprocessSpawner {
	return &LocalSubprocessSpawner{}
}

// Spawn runs a local claude -p subprocess.
func (s *LocalSubprocessSpawner) Spawn(ctx context.Context, opts *SpawnOpts) (*workspace.AgentResult, error) {
	localOpts := cloneSpawnOpts(opts)
	if localOpts.Command == "" {
		localOpts.Command = s.Command
	}
	return Spawn(ctx, &localOpts)
}

// SpawnBackground starts a local claude -p subprocess in the background.
func (s *LocalSubprocessSpawner) SpawnBackground(ctx context.Context, opts *SpawnOpts) (*CoordinatorHandle, error) {
	localOpts := cloneSpawnOpts(opts)
	if localOpts.Command == "" {
		localOpts.Command = s.Command
	}
	return SpawnBackground(ctx, &localOpts)
}

func cloneSpawnOpts(opts *SpawnOpts) SpawnOpts {
	if opts == nil {
		return SpawnOpts{}
	}
	return *opts
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

	// result.usage
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
	} `json:"usage"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Name      string          `json:"name,omitempty"`        // tool_use name
	ID        string          `json:"id,omitempty"`          // tool_use id
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use input
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result reference
}

// Spawn runs claude -p with the given prompt, parses stream-json output,
// and returns the structured result. Blocks until the process exits or times out.
func Spawn(ctx context.Context, opts *SpawnOpts) (*workspace.AgentResult, error) {
	localOpts := cloneSpawnOpts(opts)
	ctx, cancel := withSpawnTimeout(ctx, localOpts.TimeoutMinutes)
	if cancel != nil {
		defer cancel()
	}

	proc, stdout, stderr, err := startLocalProcess(ctx, &localOpts)
	if err != nil {
		return nil, err
	}

	parser := newStreamParser(&localOpts)
	result, err := parser.read(stdout)
	if err != nil {
		return nil, err
	}
	err = waitForLocalProcess(proc, result != nil, parser.progress, localOpts.TeamName)

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("timeout after %d minutes", localOpts.TimeoutMinutes)
	}

	if result != nil {
		return result, nil
	}

	// Process exited without a result message
	if err == nil {
		// Exit code 0 but no result — use last assistant text
		return &workspace.AgentResult{
			Agent:    localOpts.TeamName,
			Status:    "success",
			Result:    parser.lastAssistText,
			SessionID: parser.sessionID,
		}, nil
	}

	return nil, fmt.Errorf("process exited with error: %w; stderr: %s", err, stderr.String())
}

func withSpawnTimeout(ctx context.Context, timeoutMinutes int) (context.Context, context.CancelFunc) {
	if timeoutMinutes <= 0 {
		return ctx, nil
	}
	return context.WithTimeout(ctx, time.Duration(timeoutMinutes)*time.Minute)
}

func startLocalProcess(ctx context.Context, opts *SpawnOpts) (*exec.Cmd, io.Reader, *bytes.Buffer, error) {
	cmdName := opts.Command
	if cmdName == "" {
		cmdName = "claude"
	}

	proc := exec.CommandContext(ctx, cmdName, buildClaudeArgs(opts)...)
	proc.Env = claudeEnv()

	stdout, err := proc.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	stderr := &bytes.Buffer{}
	proc.Stderr = stderr

	if err := proc.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("starting process: %w", err)
	}
	return proc, stdout, stderr, nil
}

func buildClaudeArgs(opts *SpawnOpts) []string {
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
	if opts.PermissionMode == "" {
		return args
	}
	args = append(args, "--permission-mode", opts.PermissionMode)
	if opts.PermissionMode == "bypassPermissions" {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

func claudeEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			env = append(env, e)
		}
	}
	return env
}

type streamParser struct {
	teamName       string
	logWriter      io.Writer
	progress       func(teamName, msg string)
	startTime      time.Time
	result         *workspace.AgentResult
	sessionID      string
	lastAssistText string
	turnCount      int
	filesWritten   int
	filesEdited    int
	bashCmds       int
	teammateNames  map[string]string
}

func newStreamParser(opts *SpawnOpts) *streamParser {
	progress := opts.ProgressFunc
	if progress == nil {
		progress = func(string, string) {}
	}
	return &streamParser{
		teamName:      opts.TeamName,
		logWriter:     opts.LogWriter,
		progress:      progress,
		startTime:     time.Now(),
		teammateNames: make(map[string]string),
	}
}

func (p *streamParser) read(stdout io.Reader) (*workspace.AgentResult, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer for long lines
	for scanner.Scan() {
		if err := p.handleLine(scanner.Bytes()); err != nil {
			return nil, err
		}
		if p.result != nil {
			break
		}
	}
	return p.result, nil
}

func (p *streamParser) handleLine(line []byte) error {
	if err := p.writeLogLine(line); err != nil {
		return err
	}

	var evt streamEvent
	if !decodeStreamEvent(line, &evt) {
		return nil
	}
	p.handleEvent(&evt)
	return nil
}

func decodeStreamEvent(line []byte, evt *streamEvent) bool {
	return json.Unmarshal(line, evt) == nil
}

func (p *streamParser) writeLogLine(line []byte) error {
	if p.logWriter == nil {
		return nil
	}
	if _, err := p.logWriter.Write(line); err != nil {
		return fmt.Errorf("writing log line: %w", err)
	}
	if _, err := p.logWriter.Write([]byte("\n")); err != nil {
		return fmt.Errorf("writing log newline: %w", err)
	}
	return nil
}

func (p *streamParser) handleEvent(evt *streamEvent) {
	switch evt.Type {
	case "system":
		p.handleSystem(evt)
	case "assistant":
		p.handleAssistant(evt)
	case "user":
		p.handleUser(evt)
	case "result":
		p.handleResult(evt)
	}
}

func (p *streamParser) handleSystem(evt *streamEvent) {
	switch evt.Subtype {
	case "init":
		p.handleInit(evt.SessionID)
	case "task_progress":
		p.handleTaskProgress(evt)
	}
}

func (p *streamParser) handleInit(sessionID string) {
	if sessionID != "" {
		p.sessionID = sessionID
	}
	p.progress(p.teamName, fmt.Sprintf("⏳ session started (%s)", p.elapsed()))
}

func (p *streamParser) handleTaskProgress(evt *streamEvent) {
	if evt.TaskToolUseID == "" {
		return
	}
	role, ok := p.teammateNames[evt.TaskToolUseID]
	if !ok {
		return
	}
	detail := taskProgressDetail(evt)
	if detail == "" {
		return
	}
	p.progress(p.teamName+":"+role, "   "+truncateText(detail, 120))
}

func taskProgressDetail(evt *streamEvent) string {
	if evt.Description != "" {
		return evt.Description
	}
	return evt.LastToolName
}

func (p *streamParser) handleAssistant(evt *streamEvent) {
	p.turnCount++
	for i := range evt.Message.Content {
		p.handleContentBlock(&evt.Message.Content[i])
	}
}

func (p *streamParser) handleContentBlock(c *contentBlock) {
	switch c.Type {
	case "text":
		p.handleText(c.Text)
	case "tool_use":
		p.handleToolUse(c)
	case "thinking":
		p.progress(p.teamName, fmt.Sprintf("🧠 [turn %d | %s] thinking...", p.turnCount, p.elapsed()))
	}
}

func (p *streamParser) handleText(text string) {
	if text == "" {
		return
	}
	p.lastAssistText = text
	preview := truncateText(compactText(text), 140)
	p.progress(p.teamName, fmt.Sprintf("💬 [turn %d | %s] %s", p.turnCount, p.elapsed(), preview))
}

func (p *streamParser) handleToolUse(c *contentBlock) {
	if c.Name == "" {
		return
	}
	detail := summarizeToolUse(c.Name, c.Input)
	p.progress(p.teamName, fmt.Sprintf("🔧 [turn %d | %s] %s", p.turnCount, p.elapsed(), detail))
	p.trackAgentToolUse(c)
	p.trackToolStats(c.Name)
}

func (p *streamParser) trackAgentToolUse(c *contentBlock) {
	if c.Name != "Agent" || c.ID == "" {
		return
	}
	var agentParams map[string]any
	if err := json.Unmarshal(c.Input, &agentParams); err != nil {
		return
	}
	if desc, ok := agentParams["description"].(string); ok && desc != "" {
		p.teammateNames[c.ID] = desc
	}
}

func (p *streamParser) trackToolStats(name string) {
	switch name {
	case "Write":
		p.filesWritten++
	case "Edit":
		p.filesEdited++
	case "Bash":
		p.bashCmds++
	}
}

func (p *streamParser) handleUser(evt *streamEvent) {
	for i := range evt.Message.Content {
		if evt.Message.Content[i].Type == "tool_result" {
			p.progress(p.teamName, fmt.Sprintf("   [turn %d | %s] ✓ tool completed", p.turnCount, p.elapsed()))
		}
	}
}

func (p *streamParser) handleResult(evt *streamEvent) {
	inputTokens := evt.Usage.InputTokens + evt.Usage.CacheCreationInputTokens + evt.Usage.CacheReadInputTokens
	outputTokens := evt.Usage.OutputTokens
	p.result = &workspace.AgentResult{
		Agent:        p.teamName,
		Status:       evt.Subtype,
		Result:       evt.Result,
		CostUSD:      eventCost(evt),
		NumTurns:     evt.NumTurns,
		DurationMs:   evt.DurationMs,
		SessionID:    p.eventSessionID(evt.SessionID),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}
	p.progress(p.teamName, fmt.Sprintf("✅ finished (%s) — %d turns, %s in / %s out, %d writes, %d edits, %d bash cmds",
		p.elapsed(), evt.NumTurns, formatTokens(inputTokens), formatTokens(outputTokens), p.filesWritten, p.filesEdited, p.bashCmds))
}

func (p *streamParser) eventSessionID(sessionID string) string {
	if sessionID != "" {
		return sessionID
	}
	return p.sessionID
}

func (p *streamParser) elapsed() string {
	d := time.Since(p.startTime).Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", m, s)
}

func eventCost(evt *streamEvent) float64 {
	if evt.TotalCost != 0 {
		return evt.TotalCost
	}
	return evt.CostUSD
}

func waitForLocalProcess(proc *exec.Cmd, gotResult bool, progress func(string, string), teamName string) error {
	if !gotResult {
		return proc.Wait()
	}

	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(60 * time.Second):
		progress(teamName, "⚠️  process didn't exit 60s after result — force killing")
		if killErr := proc.Process.Kill(); killErr != nil {
			progress(teamName, "failed to force kill process: "+killErr.Error())
		}
		return <-done
	}
}

// CoordinatorHandle provides a non-blocking handle to a background claude -p process.
type CoordinatorHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	result *workspace.AgentResult
	err    error
}

// Wait blocks until the coordinator exits and returns its result.
func (h *CoordinatorHandle) Wait() (*workspace.AgentResult, error) {
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
func SpawnBackground(ctx context.Context, opts *SpawnOpts) (*CoordinatorHandle, error) {
	localOpts := cloneSpawnOpts(opts)

	ctx, cancel := context.WithCancel(ctx)
	if localOpts.TimeoutMinutes > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(localOpts.TimeoutMinutes)*time.Minute)
	}

	handle := &CoordinatorHandle{
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go func() {
		defer close(handle.done)
		result, err := Spawn(ctx, &localOpts)
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

func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// formatTokens formats a token count as a human-readable string (e.g. "284K", "1.2M").
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dK", n/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// summarizeToolUse returns a human-readable summary of a tool invocation.
func summarizeToolUse(name string, input json.RawMessage) string {
	params, ok := toolParams(input)
	if !ok {
		return name
	}

	switch name {
	case "Write", "Edit", "Read":
		return summarizeFileTool(name, params)
	case "Bash":
		return summarizeBashTool(params)
	case "Glob":
		return summarizePatternTool("Glob", params)
	case "Grep":
		return summarizeGrepTool(params)
	case "Task":
		return summarizeStringParam("Task", "description", params)
	case "TodoWrite", "TaskCreate":
		return summarizeStringParam(name, "subject", params)
	case "Agent":
		return summarizeAgentTool(params)
	case "TaskOutput":
		return summarizeTaskOutput(params)
	}

	return name
}

func toolParams(input json.RawMessage) (map[string]any, bool) {
	var params map[string]any
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, false
	}
	return params, true
}

func summarizeFileTool(name string, params map[string]any) string {
	return summarizeStringParam(name, "file_path", params)
}

func summarizeBashTool(params map[string]any) string {
	if cmd, ok := params["command"].(string); ok {
		return "Bash → " + truncateText(compactText(cmd), 100)
	}
	return "Bash"
}

func summarizePatternTool(name string, params map[string]any) string {
	return summarizeStringParam(name, "pattern", params)
}

func summarizeGrepTool(params map[string]any) string {
	pattern, ok := params["pattern"].(string)
	if !ok {
		return "Grep"
	}
	path, ok := params["path"].(string)
	if !ok {
		return "Grep → " + pattern
	}
	return "Grep → " + pattern + " in " + path
}

func summarizeStringParam(name, key string, params map[string]any) string {
	value, ok := params[key].(string)
	if !ok {
		return name
	}
	return name + " → " + value
}

func summarizeAgentTool(params map[string]any) string {
	desc, ok := params["description"].(string)
	if !ok {
		return "Agent"
	}
	if bg, ok := params["run_in_background"].(bool); ok && bg {
		return "Agent → " + desc + " (background)"
	}
	return "Agent → " + desc
}

func summarizeTaskOutput(params map[string]any) string {
	taskID, ok := params["task_id"].(string)
	if !ok {
		return "TaskOutput"
	}
	return "TaskOutput → waiting on " + truncateText(taskID, 12)
}
