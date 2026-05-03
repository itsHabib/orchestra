package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/artifacts"
)

// Tool names for the v3 artifact-read surface.
const (
	ToolGetArtifacts = "get_artifacts"
	ToolReadArtifact = "read_artifact"
)

// artifactsSubdir is the directory under <workspace>/.orchestra/ that the
// dispatcher writes artifacts into. Mirrors the engine-side path computed in
// pkg/orchestra/ma.go::artifactStore.
const artifactsSubdir = "artifacts"

// GetArtifactsArgs is the get_artifacts tool input. agent and phase are
// optional filters; an empty agent lists every agent's artifacts in the run.
type GetArtifactsArgs struct {
	RunID string `json:"run_id" jsonschema:"run id from list_runs / get_run / run"`
	Agent string `json:"agent,omitempty" jsonschema:"optional agent name; omit to list across every agent in the run"`
	Phase string `json:"phase,omitempty" jsonschema:"optional phase filter; matches the phase the artifact was emitted in"`
}

// GetArtifactsResult is the get_artifacts tool output.
type GetArtifactsResult struct {
	Artifacts []ArtifactMetaView `json:"artifacts"`
}

// ArtifactMetaView is the chat-side LLM's view of one artifact's metadata.
// Mirrors [artifacts.Meta] but flattens the run_id (always == args.RunID for
// this call) and renames Type to a string so the schema reads naturally.
type ArtifactMetaView struct {
	Agent   string    `json:"agent"`
	Phase   string    `json:"phase,omitempty"`
	Key     string    `json:"key"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	Written time.Time `json:"written"`
}

// ReadArtifactArgs is the read_artifact tool input.
type ReadArtifactArgs struct {
	RunID string `json:"run_id" jsonschema:"run id from list_runs / get_run / run"`
	Agent string `json:"agent" jsonschema:"agent name from get_artifacts or RunView.Agents[].Name"`
	Key   string `json:"key" jsonschema:"artifact key from get_artifacts or RunView.Agents[].Artifacts[]"`
}

// ReadArtifactResult is the read_artifact tool output. Content is the raw
// JSON the agent emitted: a JSON string for type=text, an arbitrary JSON value
// for type=json. Clients decide how to deserialize.
type ReadArtifactResult struct {
	Type    string          `json:"type"`
	Phase   string          `json:"phase,omitempty"`
	Content json.RawMessage `json:"content"`
}

func (s *Server) handleGetArtifacts(ctx context.Context, _ *mcp.CallToolRequest, args GetArtifactsArgs) (*mcp.CallToolResult, GetArtifactsResult, error) {
	if args.RunID == "" {
		return errResult("run_id is required"), GetArtifactsResult{}, nil
	}
	entry, ok, err := s.registry.Get(ctx, args.RunID)
	if err != nil {
		return errResult("read registry: %v", err), GetArtifactsResult{}, nil
	}
	if !ok {
		return errResult("run %q not found", args.RunID), GetArtifactsResult{}, nil
	}

	store := s.artifactStore(artifactsRoot(entry.WorkspaceDir))
	metas, err := store.List(ctx, entry.RunID, args.Agent)
	if err != nil {
		return errResult("list artifacts: %v", err), GetArtifactsResult{}, nil
	}

	views := make([]ArtifactMetaView, 0, len(metas))
	for i := range metas {
		m := &metas[i]
		if args.Phase != "" && m.Phase != args.Phase {
			continue
		}
		views = append(views, ArtifactMetaView{
			Agent:   m.Agent,
			Phase:   m.Phase,
			Key:     m.Key,
			Type:    string(m.Type),
			Size:    m.Size,
			Written: m.Written,
		})
	}
	return textResult(fmt.Sprintf("%d artifact(s)", len(views))), GetArtifactsResult{Artifacts: views}, nil
}

func (s *Server) handleReadArtifact(ctx context.Context, _ *mcp.CallToolRequest, args ReadArtifactArgs) (*mcp.CallToolResult, ReadArtifactResult, error) {
	if args.RunID == "" {
		return errResult("run_id is required"), ReadArtifactResult{}, nil
	}
	if args.Agent == "" {
		return errResult("agent is required"), ReadArtifactResult{}, nil
	}
	if args.Key == "" {
		return errResult("key is required"), ReadArtifactResult{}, nil
	}
	entry, ok, err := s.registry.Get(ctx, args.RunID)
	if err != nil {
		return errResult("read registry: %v", err), ReadArtifactResult{}, nil
	}
	if !ok {
		return errResult("run %q not found", args.RunID), ReadArtifactResult{}, nil
	}

	store := s.artifactStore(artifactsRoot(entry.WorkspaceDir))
	art, meta, err := store.Get(ctx, entry.RunID, args.Agent, args.Key)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			return errResult("artifact %q for agent %q not found", args.Key, args.Agent), ReadArtifactResult{}, nil
		}
		return errResult("read artifact: %v", err), ReadArtifactResult{}, nil
	}
	return textResult(fmt.Sprintf("artifact %s/%s (%s, %d bytes)", args.Agent, args.Key, art.Type, meta.Size)),
		ReadArtifactResult{
			Type:    string(art.Type),
			Phase:   meta.Phase,
			Content: art.Content,
		}, nil
}

// artifactsRoot returns the directory the engine writes artifacts to for one
// run's workspace. Mirrors pkg/orchestra/ma.go::artifactStore: <workspace>/
// .orchestra/artifacts. Centralized so the engine and the MCP read tools
// stay in sync if the layout ever changes.
func artifactsRoot(workspaceDir string) string {
	return filepath.Join(stateDir(workspaceDir), artifactsSubdir)
}
