package mcp

import (
	"context"
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
type InlineDAG struct {
	ProjectName string       `json:"project_name,omitempty"`
	Backend     string       `json:"backend,omitempty" jsonschema:"\"local\" (default) or \"managed_agents\""`
	Teams       []InlineTeam `json:"teams"`
}

// InlineTeam is one team in an InlineDAG. Each team becomes one
// `orchestra run` agent with a single Task derived from Prompt.
type InlineTeam struct {
	Name   string   `json:"name" jsonschema:"unique team name"`
	Role   string   `json:"role" jsonschema:"team lead role/persona, e.g. designer, engineer, reviewer"`
	Prompt string   `json:"prompt" jsonschema:"free-form text describing what the team should accomplish; folded into a single Task.summary"`
	Deps   []string `json:"deps,omitempty" jsonschema:"team names this team depends on"`
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
// The richer shape (custom tools, skills, member roster, multi-task teams) is
// reachable through ConfigPath and is intentionally out of reach inline.
func (d *InlineDAG) toConfig(projectOverride string) (*config.Config, error) {
	if d == nil {
		return nil, errors.New("nil inline_dag")
	}
	if len(d.Teams) == 0 {
		return nil, errors.New("inline_dag.teams must have at least one team")
	}
	cfg := &config.Config{
		Name:    d.ProjectName,
		Backend: config.Backend{Kind: d.Backend},
	}
	if projectOverride != "" {
		cfg.Name = projectOverride
	}
	if cfg.Name == "" {
		cfg.Name = "mcp-run"
	}
	cfg.Teams = make([]config.Team, len(d.Teams))
	for i, t := range d.Teams {
		if t.Name == "" {
			return nil, fmt.Errorf("inline_dag.teams[%d].name is required", i)
		}
		if t.Role == "" {
			return nil, fmt.Errorf("inline_dag.teams[%d].role is required (got team %q)", i, t.Name)
		}
		if t.Prompt == "" {
			return nil, fmt.Errorf("inline_dag.teams[%d].prompt is required (got team %q)", i, t.Name)
		}
		cfg.Teams[i] = config.Team{
			Name:      t.Name,
			Lead:      config.Lead{Role: t.Role},
			DependsOn: append([]string(nil), t.Deps...),
			Tasks:     []config.Task{{Summary: t.Prompt}},
		}
	}
	return cfg, nil
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
