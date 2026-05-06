package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/config"
)

// ToolRun is the v1 generic write entry point. Wraps `orchestra run` over an
// inline DAG (constructed by the chat-side LLM) or a path to an existing yaml.
const ToolRun = "run"

// RunArgs is the run tool input. Exactly one of InlineDAG / ConfigPath must be
// set; the handler rejects both-or-neither at validation time.
type RunArgs struct {
	InlineDAG   *InlineDAG `json:"inline_dag,omitempty" jsonschema:"inline DAG mirroring orchestra.yaml. Provide exactly one of inline_dag or config_path."`
	ConfigPath  string     `json:"config_path,omitempty" jsonschema:"absolute path to an existing orchestra.yaml. Provide exactly one of inline_dag or config_path."`
	ProjectName string     `json:"project_name,omitempty" jsonschema:"override the run's project name (otherwise taken from inline_dag.project_name or the loaded yaml)."`
}

// InlineDAG is the simplified shape the chat-side LLM constructs. The handler
// folds it into a real config.Config before validation; richer shapes (custom
// tools, skills, members, etc.) are accessible via ConfigPath.
//
// `agents:` is the v3 canonical key; `teams:` is accepted as an alias by
// [InlineDAG.UnmarshalJSON] so v2 clients keep working.
type InlineDAG struct {
	ProjectName string `json:"project_name,omitempty"`
	Backend     string `json:"backend,omitempty" jsonschema:"\"local\" (default) or \"managed_agents\""`

	// RequiresCredentials applies to every agent in the run; the engine
	// merges it with each agent's per-agent list and resolves the union via
	// internal/credentials at run start. Mirrors Defaults.RequiresCredentials
	// in the on-disk YAML schema.
	RequiresCredentials []string `json:"requires_credentials,omitempty" jsonschema:"credential names every agent in this run needs. Resolved via internal/credentials at run start; failing fast on any missing name."`

	// Files applies to every agent. Each entry is fanned out onto the
	// resolved config.Agent.Files list at toConfig time so the engine sees
	// the same per-agent shape it gets from on-disk YAML. Paths must be
	// absolute — there is no source YAML directory to resolve relative
	// entries against on the inline path.
	Files []config.FileMount `json:"files,omitempty" jsonschema:"file mounts shared by every agent. Each FileMount.Path must be absolute. Phase A: managed_agents only."`

	Agents []InlineAgent `json:"agents,omitempty"`
}

// UnmarshalJSON accepts the legacy `teams:` key alongside `agents:` so v2
// MCP clients keep working through the v3 transition. Setting both keys at
// the same time is rejected — even when one is empty — so a migration
// typo (`agents: []` plus `teams: [...]`) fails fast instead of silently
// falling back to the legacy spelling.
func (d *InlineDAG) UnmarshalJSON(data []byte) error {
	keys, err := dagKeysPresent(data)
	if err != nil {
		return err
	}
	hasAgents, hasTeams := keys.Agents, keys.Teams
	type rawInlineDAG struct {
		ProjectName         string             `json:"project_name,omitempty"`
		Backend             string             `json:"backend,omitempty"`
		RequiresCredentials []string           `json:"requires_credentials,omitempty"`
		Files               []config.FileMount `json:"files,omitempty"`
		Agents              []InlineAgent      `json:"agents"`
		Teams               []InlineAgent      `json:"teams"`
	}
	var raw rawInlineDAG
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if hasAgents && hasTeams {
		return errors.New("inline_dag: cannot set both `agents:` and `teams:` — use `agents:`")
	}
	d.ProjectName = raw.ProjectName
	d.Backend = raw.Backend
	d.RequiresCredentials = raw.RequiresCredentials
	d.Files = raw.Files
	d.Agents = raw.Agents
	if hasTeams {
		d.Agents = raw.Teams
	}
	return nil
}

// inlineDAGKeyPresence captures whether the JSON payload for the MCP
// `run` tool's inline DAG explicitly set `agents` and `teams`. Same role
// as [config.agentListKeyPresence] — the dual-key guard needs to
// distinguish a missing key from an empty list.
type inlineDAGKeyPresence struct {
	Agents bool
	Teams  bool
}

// dagKeysPresent reports whether the JSON object explicitly set `agents`
// and `teams`.
func dagKeysPresent(data []byte) (inlineDAGKeyPresence, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return inlineDAGKeyPresence{}, err
	}
	_, hasAgents := probe["agents"]
	_, hasTeams := probe["teams"]
	return inlineDAGKeyPresence{Agents: hasAgents, Teams: hasTeams}, nil
}

// InlineAgent is one agent in an InlineDAG. Each becomes one `orchestra run`
// agent with a single Task derived from Prompt.
type InlineAgent struct {
	Name   string   `json:"name" jsonschema:"unique agent name"`
	Role   string   `json:"role" jsonschema:"lead role/persona, e.g. designer, engineer, reviewer"`
	Prompt string   `json:"prompt" jsonschema:"free-form text describing what the agent should accomplish; folded into a single Task.summary"`
	Deps   []string `json:"deps,omitempty" jsonschema:"agent names this agent depends on"`

	// RequiresCredentials extends the top-level InlineDAG.RequiresCredentials
	// for this agent. The engine sees the union (deduped + sorted) of both
	// lists at run start.
	RequiresCredentials []string `json:"requires_credentials,omitempty" jsonschema:"credential names this agent needs in addition to the top-level requires_credentials."`

	// Files extends the top-level InlineDAG.Files for this agent. Paths
	// must be absolute (same reason as the top-level field).
	Files []config.FileMount `json:"files,omitempty" jsonschema:"file mounts for this agent. Each FileMount.Path must be absolute. Phase A: managed_agents only."`

	// EnvironmentOverride substitutes backend-level environment fields for
	// this agent only — currently just repository, used by recipes that have
	// most agents share one repo but a single agent push to a different one.
	EnvironmentOverride *config.EnvironmentOverride `json:"environment_override,omitempty" jsonschema:"per-agent environment override; currently just repository (override the backend-level managed_agents repository for this one agent)."`
}

// RunResult is the run tool output.
type RunResult struct {
	RunID        string    `json:"run_id"`
	WorkspaceDir string    `json:"workspace_dir"`
	StartedAt    time.Time `json:"started_at"`
	PID          int       `json:"pid,omitempty"`
}

func (s *Server) handleRun(ctx context.Context, _ *mcp.CallToolRequest, args RunArgs) (*mcp.CallToolResult, RunResult, error) {
	cfg, err := args.resolveConfig()
	if err != nil {
		return errResult("%v", err), RunResult{}, nil
	}
	cfg.ResolveDefaults()
	res := cfg.Validate()
	if !res.Valid() {
		return errResult("config invalid: %v", res.Err()), RunResult{}, nil
	}

	runID := NewRunID(time.Now())
	entry, err := PrepareRun(s.workspaceRoot, runID, cfg, "", nil)
	if err != nil {
		return errResult("prepare run: %v", err), RunResult{}, nil
	}
	proc, err := s.spawner.Start(ctx, entry)
	if err != nil {
		// Spawn failed: workspace + yaml are on disk with no running
		// process and no registry entry. Reclaim the directory so
		// repeated failures do not pile up under the user data dir.
		_ = os.RemoveAll(entry.WorkspaceDir)
		return errResult("spawn run: %v", err), RunResult{}, nil
	}
	if proc != nil {
		entry.PID = proc.Pid
	}
	if err := s.registry.Put(ctx, entry); err != nil {
		// Subprocess running but unregistered — list_runs / get_run /
		// send_message / read_messages cannot reach it. Best-effort: kill
		// the process and reclaim the workspace so the run stops doing
		// work no caller can observe.
		if proc != nil {
			_ = proc.Kill()
		}
		_ = os.RemoveAll(entry.WorkspaceDir)
		return errResult("register run: %v", err), RunResult{}, nil
	}

	out := RunResult{
		RunID:        entry.RunID,
		WorkspaceDir: entry.WorkspaceDir,
		StartedAt:    entry.StartedAt,
		PID:          entry.PID,
	}
	return textResult(fmt.Sprintf("run %s started in %s", out.RunID, out.WorkspaceDir)), out, nil
}

// resolveConfig builds a *config.Config from the tool args. Caller must
// ResolveDefaults / Validate before handing it to PrepareRun.
func (a *RunArgs) resolveConfig() (*config.Config, error) {
	if a == nil {
		return nil, errors.New("nil arguments")
	}
	hasInline := a.InlineDAG != nil
	hasPath := a.ConfigPath != ""
	if hasInline == hasPath {
		return nil, errors.New("provide exactly one of inline_dag or config_path")
	}
	if hasInline {
		return a.InlineDAG.toConfig(a.ProjectName)
	}
	return loadConfigFromPath(a.ConfigPath, a.ProjectName)
}

// toConfig folds the simplified InlineDAG shape into a full config.Config.
// The richer shape (custom tools, skills, member roster, multi-task agents) is
// reachable through ConfigPath and is intentionally out of reach inline.
//
// Inline file mounts must carry absolute paths — the on-disk YAML loader
// resolves relative paths against the YAML file's directory, but the inline
// path has no source file to resolve against. The MCP server's CWD is not a
// useful anchor either (it depends on whichever process spawned the server).
// Callers compose absolute paths chat-side.
func (d *InlineDAG) toConfig(projectOverride string) (*config.Config, error) {
	if d == nil {
		return nil, errors.New("nil inline_dag")
	}
	if len(d.Agents) == 0 {
		return nil, errors.New("inline_dag.agents must have at least one agent")
	}
	if err := validateInlineFiles("inline_dag.files", d.Files); err != nil {
		return nil, err
	}
	cfg := &config.Config{
		Name:    d.ProjectName,
		Backend: config.Backend{Kind: d.Backend},
		Defaults: config.Defaults{
			RequiresCredentials: append([]string(nil), d.RequiresCredentials...),
		},
	}
	if projectOverride != "" {
		cfg.Name = projectOverride
	}
	if cfg.Name == "" {
		cfg.Name = "mcp-run"
	}
	cfg.Agents = make([]config.Agent, len(d.Agents))
	for i := range d.Agents {
		a := &d.Agents[i]
		if a.Name == "" {
			return nil, fmt.Errorf("inline_dag.agents[%d].name is required", i)
		}
		if a.Role == "" {
			return nil, fmt.Errorf("inline_dag.agents[%d].role is required (got agent %q)", i, a.Name)
		}
		if a.Prompt == "" {
			return nil, fmt.Errorf("inline_dag.agents[%d].prompt is required (got agent %q)", i, a.Name)
		}
		if err := validateInlineFiles(fmt.Sprintf("inline_dag.agents[%d].files", i), a.Files); err != nil {
			return nil, err
		}
		agent := config.Agent{
			Name:                a.Name,
			Lead:                config.Lead{Role: a.Role},
			DependsOn:           append([]string(nil), a.Deps...),
			Tasks:               []config.Task{{Summary: a.Prompt}},
			RequiresCredentials: append([]string(nil), a.RequiresCredentials...),
			Files:               mergeFileMounts(d.Files, a.Files),
		}
		if a.EnvironmentOverride != nil {
			agent.EnvironmentOverride = *a.EnvironmentOverride
		}
		cfg.Agents[i] = agent
	}
	return cfg, nil
}

// validateInlineFiles enforces the absolute-path requirement on inline file
// mounts. The error message names the mount entry (path prefix + index) so a
// chat-side composer can fix the right one without re-reading the whole DAG.
func validateInlineFiles(prefix string, mounts []config.FileMount) error {
	for i, m := range mounts {
		if m.Path == "" {
			return fmt.Errorf("%s[%d].path is required", prefix, i)
		}
		if !filepath.IsAbs(m.Path) {
			return fmt.Errorf("%s[%d].path must be absolute (got %q)", prefix, i, m.Path)
		}
	}
	return nil
}

// mergeFileMounts concatenates the top-level shared mounts with the per-agent
// mounts. Returns a fresh slice so neither input is aliased into the resolved
// config (mutating either later would otherwise change the agent's view).
func mergeFileMounts(shared, agent []config.FileMount) []config.FileMount {
	if len(shared) == 0 && len(agent) == 0 {
		return nil
	}
	out := make([]config.FileMount, 0, len(shared)+len(agent))
	out = append(out, shared...)
	out = append(out, agent...)
	return out
}

func loadConfigFromPath(path, projectOverride string) (*config.Config, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("config_path must be absolute (got %q)", path)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("config_path: %w", err)
	}
	res, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	if !res.Valid() {
		return nil, fmt.Errorf("validation: %w", res.Err())
	}
	cfg := res.Config
	if projectOverride != "" {
		cfg.Name = projectOverride
	}
	return cfg, nil
}
