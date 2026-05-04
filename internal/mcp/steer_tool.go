package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/spawner"
	"github.com/itsHabib/orchestra/internal/store"
)

// ToolSteer is the MCP tool name for [Server.handleSteer].
const ToolSteer = "steer"

// steerSendRetries matches `orchestra msg`'s 4-retry budget. SendUserMessage
// counts retries (not attempts) so 4 yields up to 5 total HTTP calls — same
// shape as the CLI's at-least-once delivery story.
const steerSendRetries = 4

// backendManagedAgents marks the run as MA-backed in state.json. Steering and
// the soon-to-be-deleted file-bus tools both gate on this string; defining
// it here keeps the value owned by the surface that depends on it after the
// bus is removed.
const backendManagedAgents = "managed_agents"

// SessionEventsFactory returns a [spawner.SessionEventSender] authenticated
// with the host API key. Production callers wire DefaultSessionEventsFactory;
// tests pass a stub that returns an in-memory fake.
type SessionEventsFactory func(ctx context.Context) (spawner.SessionEventSender, error)

// DefaultSessionEventsFactory constructs the production [spawner.
// SessionEventSender] via [spawner.SessionEventsClient]. Reused for the
// standard wiring path so tests can replace the entire dependency in a
// single line.
func DefaultSessionEventsFactory(ctx context.Context) (spawner.SessionEventSender, error) {
	return spawner.SessionEventsClient(ctx)
}

// SteerArgs is the steer tool input.
type SteerArgs struct {
	RunID   string `json:"run_id" jsonschema:"run id from list_runs / get_run"`
	Agent   string `json:"agent" jsonschema:"agent name; must be in status running with a recorded session id"`
	Content string `json:"content" jsonschema:"the user.message body to inject into the agent's session"`
}

// SteerResult is the steer tool output.
type SteerResult struct {
	RunID   string `json:"run_id"`
	Agent   string `json:"agent"`
	Backend string `json:"backend"`
}

func (s *Server) handleSteer(ctx context.Context, _ *mcp.CallToolRequest, args SteerArgs) (*mcp.CallToolResult, SteerResult, error) {
	if args.RunID == "" {
		return errResult("run_id is required"), SteerResult{}, nil
	}
	if args.Agent == "" {
		return errResult("agent is required"), SteerResult{}, nil
	}
	if args.Content == "" {
		return errResult("content is required"), SteerResult{}, nil
	}

	entry, ok, err := s.registry.Get(ctx, args.RunID)
	if err != nil {
		return errResult("read registry: %v", err), SteerResult{}, nil
	}
	if !ok {
		return errResult("run %q not found", args.RunID), SteerResult{}, nil
	}

	state, err := s.stateReader(ctx, stateDir(entry.WorkspaceDir))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errResult("run %q has no state yet (engine still starting)", args.RunID), SteerResult{}, nil
		}
		return errResult("read state: %v", err), SteerResult{}, nil
	}
	if state == nil {
		return errResult("run %q has no state yet (engine still starting)", args.RunID), SteerResult{}, nil
	}

	// Local backend gets a documented error rather than silent failure. The
	// v3.0 design defers local steering — keeping `claude -p` stdin open
	// requires deeper changes than Phase A scope (see DESIGN-v3 §7.7.1 and
	// the kickoff doc). The error names the alternative so the chat-side
	// LLM can act on it without round-tripping through docs.
	if state.Backend != "" && state.Backend != backendManagedAgents {
		return errResult(
			"local steering not supported in v3.0; restart the run with appended context or switch to backend: managed_agents",
		), SteerResult{}, nil
	}

	sessionID, err := spawner.SteerableSessionID(state, args.Agent)
	if err != nil {
		return errResult("agent %q not steerable: %v", args.Agent, err), SteerResult{}, nil
	}

	sessions, err := s.sessionEvents(ctx)
	if err != nil {
		return errResult("session events client: %v", err), SteerResult{}, nil
	}
	if err := spawner.SendUserMessage(ctx, sessions, sessionID, args.Content, steerSendRetries); err != nil {
		return errResult("send user.message: %v", err), SteerResult{}, nil
	}

	out := SteerResult{
		RunID:   args.RunID,
		Agent:   args.Agent,
		Backend: backendManagedAgents,
	}
	return textResult(fmt.Sprintf("steered agent %s in run %s", args.Agent, args.RunID)), out, nil
}
