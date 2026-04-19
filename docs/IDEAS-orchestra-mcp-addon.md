# Idea: orchestra MCP server (add-on, not core)

Status: **Idea capture — full design TBD**
Surfaced: 2026-04-18, during the MA-IO spike review
Not blocked by, and does not block, anything in Phase 1 or Phase 2 of [DESIGN-v2.md](./DESIGN-v2.md).

> **Framing.** Orchestra core stays a powerful but simple DAG agent runner — see the principle locked in via session feedback (2026-04-18): "richer capabilities ship as opt-in add-ons, not in core." This document captures the orchestra-MCP-server idea so we can pick it up cold when it's time, without polluting the v2 design doc.

---

## Why this came up

The MA-IO spike (April 2026) discovered that under `backend: managed_agents` there are exactly four output channels from a session back to the orchestrator host:

1. **Event stream** — `agent.message`, `agent.thinking`, `agent.tool_use/_result`, status, span. Free, durable, no work to capture.
2. **Custom tools** — agent emits `agent.custom_tool_use`; orchestra processes it host-side; replies with `user.custom_tool_result`. First-class server-side hook.
3. **MCP servers** — same model, but tools live in an MCP server you run. The agent's MCP calls land on your server.
4. **External writes** — git push, S3, an HTTP API. Vault-credentialed.

The §9.6 rewrite (post-spike) makes (1) + (4-via-git) the default for v2: events for summaries, repo for artifacts. That's enough to ship Phase 1.

But (2) and (3) are unused. Together they're a much cleaner artifact and coordination path than git for **non-code outputs and runtime coordination** — which is exactly what the v1 file-bus + coordinator + `/loop` were for. An optional MCP server would let users opt into that surface without orchestra core having an opinion about it.

---

## Sketched tool surface (illustrative — not committed)

If we eventually build this, plausible tools:

| Tool | Purpose | Replaces (in v1 / what it unlocks) |
| --- | --- | --- |
| `publish_artifact(name, content_or_path, kind)` | Capture a non-repo deliverable on the orchestra host (text snippet, JSON, small binary). Returns an `artifact_id` other teams can reference. | The `.orchestra/results/<team>/<file>` flow that v1 has and v2-MA dropped because container writes don't surface. |
| `get_upstream_summary(team)` / `get_upstream_artifact(team, name)` | Pull a specific upstream deliverable on demand instead of having everything pre-injected into the prompt. | Replaces the "inline all upstream summaries into the initial prompt" scaling cliff for fan-in-heavy DAGs. |
| `signal_progress(milestone, fraction, note)` | Structured progress reporting that orchestra captures on the host without the user reading prose. | Replaces "agent prints status, monitor parses prose" — gives `/orchestra-monitor`-style tooling structured input. |
| `request_human_input(question, urgency)` | Pause the agent until a human (via `orchestra msg <team>`) responds. | Native steering primitive — turns ad-hoc `orchestra msg` into a structured handoff with explicit pause semantics. |
| `send_team_message(to, text)` | Cross-team messaging at runtime. | The v1 file-bus, but typed and audited via the MCP transcript. |
| `signal_completion(deliverables: [...])` | Explicit "I'm done; here are my artifacts" — orchestra uses this to mark the team complete instead of inferring from `stop_reason: end_turn`. | Cleaner end-of-team semantics; enables verify-on-complete checks (mentioned in the v2 design's "later" list). |

None of these are required for a DAG to run. All of them are user-opt-in.

---

## Simpler alternative to consider first: custom tools

MA supports host-executed **custom tools** declared directly on the agent config (type `custom`, with an `input_schema`). The agent emits `agent.custom_tool_use`; orchestra runs the logic host-side; orchestra replies with `user.custom_tool_result`. Same host-side interception story as MCP, minus the server.

For an MVP of the tools sketched below (`publish_artifact`, `signal_progress`, `request_human_input`), custom tools are the lower-complexity starting point:

- No second process or HTTP endpoint to run.
- No vault / MCP-auth plumbing.
- Declared per agent, so teams opt in or out individually.
- Same `Workspace.UpdateTeamState` funnel writes state on disk.

When would an MCP server beat custom tools? When tools want to be reused across agent configs, when the tool surface becomes large enough that a protocol boundary helps, or when we want to offer the same tools to third-party MA consumers outside orchestra. None of those apply for a v1 add-on.

**Gotcha either way:** MCP toolsets default to `permission_policy: always_ask` (agent toolset defaults to `always_allow`). If we go the MCP route, orchestra must explicitly set `always_allow` on its own `mcp_toolset` entry — otherwise every orchestra-tool call parks the session at `stop_reason: requires_action` waiting for a `user.tool_confirmation`, and orchestra would have to auto-confirm in a loop. Custom tools have no such policy layer; they just execute host-side.

---

## Architectural shape

- **Sibling package or repo.** Lives in `pkg/orchestra-mcp/` (sub-package of the orchestra Go module) or `github.com/itsHabib/orchestra-mcp` (separate repo). Either way, importable by the orchestra binary but not pulled in unless the user enables it.
- **Wire-up via config.** A user opting in adds something like:
  ```yaml
  backend:
    kind: managed_agents
    managed_agents:
      mcp_servers:
        - type: orchestra
          enabled: true
          config: { ... }
  ```
  Orchestra core registers the MCP server URL on each session's agent at `EnsureAgent` time. From the agent's perspective, the orchestra MCP looks like any other MCP server.
- **Process model.** The orchestra MCP server runs in-process with the orchestra CLI (same goroutine pool, same `state.Store`, same `Workspace.UpdateTeamState` funnel). No second process to manage. Lifetime is bounded by the run.
- **State writes.** All tool calls funnel through the same `Workspace.UpdateTeamState(team, mutator)` writer that DESIGN-v2 §10.2 already defines. Consistency story is unchanged.
- **Artifacts on disk.** `publish_artifact` writes to `.orchestra/results/<team>/<artifact_id>/` — reusing the directory we kept for summaries. Resumes the v1 mental model but gated behind explicit user action.

---

## What this is NOT

- **Not a replacement for git as the artifact medium for code-shaped deliverables.** Git stays the source of truth for code. The orchestra MCP is for *non-code* artifacts and coordination semantics.
- **Not a replacement for the event stream as the runtime observability surface.** Events stay primary; the MCP adds structured side-channels.
- **Not a Phase 1 or Phase 2 deliverable for v2.** Earliest landing is post-v2.0.0, after the runtime ships and we have real users hitting real cliffs.
- **Not opinionated.** Each tool is opt-out. A user who wants pure DAG + git + events should never see this.

---

## Open questions (for the eventual design pass)

1. Sibling-package vs separate-repo. Sibling keeps versioning aligned; separate repo signals "truly optional, not core."
2. Should `publish_artifact` also work under `backend: local`? Doing so re-unifies the artifact model across backends but pulls coordination concerns back into the local subprocess flow.
3. Auth between agent and the orchestra MCP. MA's MCP transport supports auth headers; what's the minimum that prevents accidental cross-run tool calls?
4. If multiple teams call `request_human_input` concurrently, is the human responding via `orchestra msg <team>` enough, or do we need a queue/priority?
5. Naming. "Orchestra MCP" is descriptive but generic. A real product name comes later.

---

## When to revisit

- Reopen this when v2 ships (P1.10 done, default backend may or may not be flipped) **and** the first user-reported pain point is "I can't share a non-repo deliverable cleanly between teams" or "I want structured progress reporting instead of parsing prose." Not before. The whole point of capturing this now is to avoid building it speculatively.
