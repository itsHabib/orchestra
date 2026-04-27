# DESIGN: Ship-Feature Workflow

Status: **Proposed**
Owner: @itsHabib
Last updated: 2026-04-26
Builds on: [DESIGN-v2.md](./DESIGN-v2.md) (managed_agents backend), [IDEAS-orchestra-mcp-addon.md](./IDEAS-orchestra-mcp-addon.md) (MCP server idea capture)
Target: smallest end-to-end "agent ecosystem" surface on top of orchestra. Once green, this is the workflow we use as the baseline for stripping orchestra back (see §15).

> **TL;DR.** Take N design-doc paths and a target repo. Spawn N parallel Managed Agents sessions, one per doc, each running the `/ship-feature` skill. Each agent self-loops through implement → PR → request reviews → address CI/comments. Agents emit a typed `signal_completion` custom-tool call when done or blocked. Orchestra surfaces those signals as notifications and exposes the workflow over an MCP server so the user's main Claude session is the only interface they need. No human babysitting; no manual session juggling. Phased build is ~7–11 days.
>
> This is a self-contained spec. An agent picking it up cold should be able to ship Phase 1 without external context beyond DESIGN-v2.md.

---

## 1. Why this exists

### 1.1 The pain today

The user's actual workflow: write a batch of design docs, then for each one open a fresh Claude Code session, invoke `/ship-feature`, and manually nudge each session to keep watching CI and addressing review comments. With four docs in flight that's four terminal windows, four mental contexts, and a constant low-grade attention tax. The agent does the work; the human is reduced to a babysitter and a notification surface.

The pieces to fix this **already mostly exist** in this repo:

- `backend: managed_agents` runs each team as an isolated MA session ([DESIGN-v2.md](./DESIGN-v2.md) §6). One doc → one team → one MA session is a natural mapping.
- The repo-backed artifact path ([features/05-p15-repo-artifact-flow.md](./features/05-p15-repo-artifact-flow.md)) handles the worktree/branch/push lifecycle.
- The DAG runner schedules tier-0 teams in parallel (`cmd/run.go:runTier`), already exercised by [features/06-p16-multi-team-text-only.md](./features/06-p16-multi-team-text-only.md).
- Native MA skills support is wired through `internal/agents/params.go:skillParams` — agents can reference custom skills by ID.
- Custom tools are wired through `internal/agents/params.go:customToolParam` — host-executed tools the agent emits via `agent.custom_tool_use`.
- `orchestra msg <team>` already implements human steering via `user.message` ([DESIGN-v2.md](./DESIGN-v2.md) §11).

What's **missing** for this specific workflow:

1. A way to register `/ship-feature` (and any future role skills) with Anthropic and reference them by `skill_id`. Today orchestra creates agents but doesn't manage skill content.
2. A typed completion signal so the orchestrator knows the agent is genuinely done (or genuinely blocked) — not just `stop_reason: end_turn`, which can fire mid-loop while the agent is still polling CI.
3. An MCP server so the user's main Claude session can call `ship_design_docs(...)` instead of shell-ing out to `orchestra run`. The IDEAS doc captures this as future work; this design promotes it to v0.
4. A notification surface so block/done events reach the human without `tail -f`.
5. A recipe abstraction — a parameterized way to generate the orchestra.yaml (or equivalent in-memory `*config.Config`) for "ship these N docs" without making the user hand-write a yaml every time.

### 1.2 Strategic intent

This is the smallest workflow that exercises the full chain *skills → MA agent → DAG → custom-tool signal → host-side notification → MCP-mediated unblock*. If this works end-to-end, every higher-order capability (architect/critic/closer split, recipe library, multi-stage workflows) is additive on the same primitives.

It's also the smallest workflow that creates **deletion pressure** on the v1 surface — once the parent Claude session can drive runs via MCP and steer via `orchestra msg`, the `/orchestra-coord` / `/orchestra-inbox` / `/orchestra-monitor` / `/orchestra-msg` companion skills, the file-bus, the coordinator agent, and large pieces of the local backend become removable. §15 enumerates.

---

## 2. Goals & non-goals

### Goals

- **G1.** From the user's main Claude session: `ship_design_docs(paths, repo_url)` runs to completion across N parallel MA sessions, with no further human input on the happy path.
- **G2.** Each MA session is configured with `/ship-feature` as a custom skill and a `signal_completion` custom tool. The agent self-drives to PR-open + reviews-requested + CI-green + comments-addressed, then signals.
- **G3.** Block/done signals fan out to a notification surface (terminal bell + append-only log; optional system-notification shell-out). The user is interrupted only when something needs them.
- **G4.** From any Claude session connected to the orchestra MCP, the user can: list runs, get per-team status, reply to a blocked team (which routes through `orchestra msg`).
- **G5.** All new code lives under `internal/`. No `pkg/` until SDK extraction is real ([features/02-pkg-to-internal.md](./features/02-pkg-to-internal.md)).
- **G6.** Re-uses every existing primitive: DAG runner, MA spawner, agent cache, repo flow, state store, session lifecycle, watchdog, `orchestra msg`. No parallel implementations.

### Non-goals (explicit)

- **NG1. Multiple roles.** v0 has exactly one role: the **implementer**. Architect, critic, reviewer, closer all happen *inside* `/ship-feature` (which already drives design-review → impl → PR → reviews → CI → close). Splitting the role is future work, not v0.
- **NG2. Local-backend support.** This workflow is `managed_agents`-only. Local backend stays for unrelated use cases.
- **NG3. Phone push notifications.** v0 ships terminal bell + log file + optional `notify-send`/`osascript` shell-out. ntfy/pushover is a P2 add-on.
- **NG4. Recipe library.** v0 has one recipe (`ship-design-docs`). DSL, YAML grammar, multi-recipe registry are deferred.
- **NG5. Replacing `orchestra.yaml` for general use.** The recipe is internal. Existing yaml-driven workflows are unchanged.
- **NG6. Mid-flight reconfiguration.** Once a run starts, the team set is fixed. No "add a doc to this run".
- **NG7. Reviewer agents as separate orchestra teams.** PR review continues to flow through GitHub's native machinery (copilot, codex, claude review bots) requested by the implementer's own `gh pr` calls inside `/ship-feature`. Orchestra doesn't run them.

---

## 3. Glossary

| Term | Meaning |
| --- | --- |
| **Skill (orchestra)** | Markdown file describing a role. Lives in a skills directory, distributed via skill-sync. Registered with Anthropic (custom skills API) → returns a `skill_id`. |
| **Skill (MA)** | First-class MA primitive: agents reference skills via `{type: "custom" \| "anthropic", skill_id, version?}`. Wired by `internal/agents/params.go:skillParams`. |
| **Recipe** | A parameterized function that produces a `*config.Config` for a specific workflow. v0 has one: `ship-design-docs`. |
| **Sentinel** | The `signal_completion` custom tool. The agent calls it once with `{status: "done" \| "blocked", ...}`. The host treats this as the canonical end-of-team signal. |
| **Run** | One invocation of the recipe. Maps 1:1 to one `orchestra run` invocation and one `.orchestra/` workspace. |
| **Parent Claude** | The user's main Claude session connected to the orchestra MCP server. The dispatcher. |

---

## 4. The role: implementer

v0 has one role.

**Behavior.** Whatever `/ship-feature` already does. This design does **not** redefine the skill — it orchestrates it. The skill drives:

1. Read the assigned design doc.
2. Implement the feature on a fresh branch in `/workspace/repo`.
3. Open a PR via `gh pr create`.
4. Request reviews from copilot, codex, claude (via `gh pr review request` or equivalent).
5. Watch CI; address failures.
6. Read review comments; address them; push fixups.
7. Loop on 5–6 until CI is green and all required-changes are acked.
8. **New for orchestra:** call the `signal_completion` custom tool with `{status: "done", pr_url, summary}` once the PR is in the merge-ready state. If blocked at any point (genuine ambiguity, hard CI failure the agent can't resolve, contradictory review comments), call `signal_completion` with `{status: "blocked", reason}` and stop.

The only modification to `/ship-feature` to support this workflow is the final step: emit the sentinel via the custom tool instead of just outputting "DONE" in a final message. The custom-tool path is mechanically observable (`agent.custom_tool_use` events), prose isn't.

**Where the skill content lives.** TBD — verify during P0. Likely `~/.claude/skills/ship-feature/SKILL.md` (Claude Code default). The skill registration step (§5) reads from that path.

---

## 5. Skills mechanics: how `/ship-feature` reaches the MA agent

The MA backend has two ways to surface role-shaped content to an agent: the **native skill primitive** (registered with Anthropic, referenced by `skill_id`, attached on the agent resource) and **file resources** (uploaded via the Files API, attached to the session at creation, the agent reads them like any input file).

We use **file resources** for v0. Reasons:

- Files API behavior is already verified by the prior spike (`docs/SPIKE-ma-io-findings.md` confirms `POST /v1/files` + session-resource attachment work). The custom-skill registration API is referenced by SDK plumbing in `internal/agents/params.go:skillParams` but the registration *endpoint* hasn't been exercised in this codebase.
- One mental model. The "skill" markdown is a file. The design doc is a file. Project conventions are a file. All travel through the same Files API + `resources` channel.
- No drift risk on an unverified Anthropic surface. If file-resource-based loading proves flaky in practice (see §5.6), the MA SDK skill hooks are still available for a follow-up migration — and the on-disk artifact (`SKILL.md`) doesn't change.

### 5.1 The flow

```
~/.claude/skills/ship-feature/SKILL.md
        │
        │   orchestra skills upload ship-feature
        ▼
┌────────────────────────────┐
│  Anthropic Files API       │
│  POST /v1/files            │
│  body: SKILL.md content    │
│  returns: { file_id, ... } │
└────────────────────────────┘
        │
        ▼
~/.config/orchestra/skills.json   (cache: name → {file_id, content_hash, source_path, uploaded_at})
        │
        │   On session creation, the engine attaches the cached file as a session resource:
        │   resources: [{type: "file", file_id: <cached>}]
        ▼
The MA session container has SKILL.md accessible (mount path TBD — verify during P0; §5.5).
        │
        │   The team's bootstrap prompt directs the agent at it:
        │   "Your role is defined in the SKILL.md file attached to this session.
        │    Read it as your first action; follow it for the rest of the session."
        ▼
Agent reads the file early in the conversation and applies the role.
```

### 5.2 Cache shape

`~/.config/orchestra/skills.json`:

```json
{
  "skills": {
    "ship-feature": {
      "file_id": "file_01ABC...",
      "content_hash": "sha256:...",
      "source_path": "/Users/.../.claude/skills/ship-feature/SKILL.md",
      "uploaded_at": "2026-04-26T10:00:00Z"
    }
  }
}
```

Drift detection: on each session start, the engine recomputes `content_hash` from the source file. If it changed, re-upload (Files API returns a new `file_id`), update the cache. The old `file_id` can be cleaned up lazily by `orchestra skills sync` (Files API supports delete; verify the lifetime semantics during P0).

### 5.3 New CLI commands

```
orchestra skills upload <name> [--from <path>]
  # Read SKILL.md from --from (default: $HOME/.claude/skills/<name>/SKILL.md),
  # POST to Files API, cache result. Idempotent on no-content-change.

orchestra skills ls
  # Print cached skills with file_id, content_hash, drift status.

orchestra skills sync
  # Re-upload all cached skills whose source file has drifted; optionally
  # delete superseded file_ids.
```

### 5.4 New package

```
internal/skills/
├── upload.go     # Upload, Lookup, Sync — talks to Anthropic Files API
├── cache.go      # Read/write ~/.config/orchestra/skills.json with gofrs/flock
├── hash.go       # Content hashing (NFC-normalized, line-ending-normalized)
└── upload_test.go
```

Mirrors the cache + drift-detection shape of `internal/agents/`.

### 5.5 Bootstrap prompt + mount path

The recipe-generated `Team.context` includes a directive telling the agent where to find its role. Exact path is the one open question for P0 — verify how MA exposes file resources to the container (filesystem mount under `/files/<name>`? retrievable only via a tool call? returned in a special initial event?). The bootstrap prompt looks roughly like:

> "Your role is defined in the SKILL.md file attached to this session via the Files API. Read it as your first action — that file *is* your job description. Apply the role to the design doc at `/workspace/repo/<DocPath>`. When you finish, call the `signal_completion` tool. If genuinely blocked, call `signal_completion(status='blocked', reason=...)`."

If MA mounts files at a known path, the prompt names it directly. If files are retrievable only via tool, the prompt names the tool and the file_id (`Beta.Files.Get` or equivalent).

### 5.6 Migration path to native skills (not v0)

`internal/agents/params.go:skillParams` already supports the native skill primitive. If file-resource-based loading proves flaky in practice (agent forgets to read the file, role drifts mid-session, prompt-engineering becomes a tax), we migrate by adding a registration step in `internal/skills/` and flipping the engine's resolution from `Team.Skills → file resource` to `Team.Skills → AgentSpec.Skills` with a real `skill_id`. The `Team.Skills` config field stays the same; only the loader changes. This is captured as a future option, not a v0 deliverable.

---

## 6. The recipe: `ship-design-docs`

### 6.1 In-memory representation, not YAML on disk

A recipe is a Go function that returns a `*config.Config`:

```go
package recipes

type ShipDesignDocsParams struct {
    DocPaths        []string  // paths relative to repo root, e.g. ["docs/feat-foo.md", "docs/feat-bar.md"]
    RepoURL         string    // https://github.com/org/repo
    DefaultBranch   string    // default "main"
    Model           string    // default cfg-defaults
    Concurrency     int       // default 4 (well below MA's 60/min)
    Timeout         time.Duration // default 90 minutes per team
    OpenPullRequests bool     // default true
}

func ShipDesignDocs(p ShipDesignDocsParams) (*config.Config, error)
```

Each `DocPath` becomes one team. All teams are tier-0 (no `depends_on`). The generated config is equivalent to:

```yaml
name: ship-design-docs-<run_id>
backend:
  kind: managed_agents
  managed_agents:
    repository:
      url: <RepoURL>
      mount_path: /workspace/repo
      default_branch: <DefaultBranch>
    open_pull_requests: <OpenPullRequests>

defaults:
  model: <Model or "claude-opus-4-7">
  max_turns: 200
  timeout_minutes: 90
  ma_concurrent_sessions: <Concurrency>
  permission_mode: acceptEdits

teams:
  - name: ship-<slug(doc-1)>
    lead:
      role: "Feature Implementer"
    context: |
      You will run the /ship-feature skill against the design doc at
      /workspace/repo/<DocPath>. Drive it to: PR open, reviews requested
      (copilot, codex, claude), CI green, all required-changes acked.

      When the PR is in the merge-ready state, call signal_completion
      with status="done", pr_url=<the PR URL>, summary=<one-line summary>.

      If you hit a hard block — genuine ambiguity in the spec, an unresolvable
      review conflict, a CI failure outside your scope — call signal_completion
      with status="blocked", reason=<a sentence the human can act on>, then stop.

      The user can reach you via orchestra's steering channel; treat any
      incoming user.message as authoritative.
    tasks:
      - summary: "Ship the design doc"
        details: "Run /ship-feature against /workspace/repo/<DocPath>"
        deliverables: []
        verify: ""
    skills:
      - name: ship-feature
        type: custom
    custom_tools:
      - name: signal_completion
        # input_schema is a constant (§7); recipe doesn't need to repeat it
        from_recipe: true

  # ... one team per DocPath, all in tier 0
```

The recipe lives at `internal/recipes/ship_design_docs.go`. It does **not** write a yaml file; it constructs `*config.Config` directly and hands it to the engine. Yaml-on-disk is only useful for users authoring runs by hand; this run is generated.

### 6.2 Why no yaml on disk

DESIGN-v2's existing `cmd/run.go` flow already accepts a `*config.Config` (the yaml loader produces one). Recipes side-step the loader. Benefits:

- No filesystem step in the parent-Claude → MCP → engine path.
- Recipe parameter validation happens in Go, not by surviving a yaml round-trip.
- Future recipes (architect+critic, hotfix shape, etc.) can compose Go functions.

### 6.3 Schema additions to `internal/config/`

The existing `Team` struct has no `skills` or `custom_tools` field. Add them:

```go
// internal/config/schema.go

type Team struct {
    // ... existing fields ...

    // Skills attached to the team's MA agent. Resolved against the orchestra
    // skills cache (~/.config/orchestra/skills.json) at agent-creation time.
    // Local backend ignores this field with a warning.
    Skills []SkillRef `yaml:"skills,omitempty" json:"skills,omitempty"`

    // Custom tools attached to the team's MA agent. Each tool gets a host-side
    // handler registered by the engine (§7). Local backend ignores with a warning.
    CustomTools []CustomToolRef `yaml:"custom_tools,omitempty" json:"custom_tools,omitempty"`
}

type SkillRef struct {
    Name    string `yaml:"name" json:"name"`
    Type    string `yaml:"type,omitempty" json:"type,omitempty"`        // "custom" (default) or "anthropic"
    Version string `yaml:"version,omitempty" json:"version,omitempty"`  // pin a specific version; default = latest cached
}

type CustomToolRef struct {
    Name string `yaml:"name" json:"name"`  // must be a tool name registered in internal/customtools/
}
```

User-authored yamls can opt into skills or custom tools by listing them, but the v0 recipe does not require yaml authorship.

### 6.4 Mapping into `AgentSpec`

The MA spawner already takes `Skills []Skill` and `Tools []Tool` on `AgentSpec` (`internal/agents/params.go`). The new wiring:

1. Engine resolves `Team.Skills` against `internal/skills/` cache → `[]agents.Skill` with `Name` = `skill_id`, `Metadata["type"]` = "custom".
2. Engine resolves `Team.CustomTools` against the registered host-side custom tools (§7) → `[]agents.Tool` with `Type: "custom"` and the tool's `InputSchema`.
3. `EnsureAgent` is called as today; the spec hash now incorporates skill_ids and custom-tool names (already supported by `internal/agents/hash.go:canonicalSkills` + `canonicalTools`).

No spawner changes required.

---

## 7. The sentinel: `signal_completion`

### 7.1 Tool definition

```go
// internal/customtools/signal_completion.go

var SignalCompletion = customtools.Definition{
    Name:        "signal_completion",
    Description: "Called once per session when the team's work is fully done OR genuinely blocked. After calling this, stop emitting actions.",
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "status":  map[string]any{"type": "string", "enum": []string{"done", "blocked"}},
            "pr_url":  map[string]any{"type": "string", "description": "Required when status=done"},
            "summary": map[string]any{"type": "string", "description": "One-line summary of what was shipped or why it's blocked"},
            "reason":  map[string]any{"type": "string", "description": "Required when status=blocked. A sentence the human can act on."},
        },
        "required": []string{"status", "summary"},
    },
}
```

### 7.2 Host-side handler

```go
// internal/customtools/handler.go

type Handler interface {
    // Tool returns the tool definition (registered on agent creation).
    Tool() Definition

    // Handle is called when the engine receives an agent.custom_tool_use event
    // for this tool. It returns the result to send back as user.custom_tool_result.
    Handle(ctx context.Context, run *RunContext, team string, input json.RawMessage) (result json.RawMessage, err error)
}
```

For `signal_completion`, `Handle`:

1. Unmarshals `{status, pr_url, summary, reason}`.
2. Calls `store.UpdateTeamState(team, mutator)` to set `signal_status`, `signal_summary`, `signal_pr_url`, `signal_reason`, `signal_at`.
3. Emits a notification (§9).
4. If `status == done`: returns success; engine archives the session as part of the normal `idle + end_turn` exit path (DESIGN-v2 §6 step 5).
5. If `status == blocked`: returns success but engine **leaves the session alive** — the team can be unblocked via `orchestra msg`. `runTier` waits on the team until it eventually signals done (or the team-level timeout fires).

### 7.3 New package

```
internal/customtools/
├── definition.go       # Tool definition type
├── handler.go          # Handler interface, registry
├── signal_completion.go
└── handler_test.go
```

The registry is the simplest possible: a `map[string]Handler` keyed by tool name. Handlers register themselves at package init or are wired by the engine startup.

### 7.4 Wiring custom-tool events into the engine

`cmd/run_ma.go` already streams events. Add a case for `agent.custom_tool_use`:

```go
case *spawner.AgentCustomToolUseEvent:
    if h, ok := customtools.Lookup(evt.Name); ok {
        result, err := h.Handle(ctx, runCtx, teamName, evt.Input)
        if err != nil {
            // Emit user.custom_tool_result with error; team continues
        } else {
            // Emit user.custom_tool_result with success
        }
        session.Send(ctx, spawner.UserCustomToolResultEvent{
            ToolUseID: evt.ID,
            Content:   result,
            IsError:   err != nil,
        })
    }
```

The session's filesystem and conversation continue; the agent receives the tool result and can decide whether to keep going or stop. For `signal_completion`, the prompt instructs the agent to stop after calling it.

---

## 8. The MCP server: parent-Claude interface

### 8.1 New CLI command

```
orchestra mcp [--transport stdio|http] [--port N]
  # Starts an in-process MCP server. Default transport: stdio (for Claude Code attachment).
  # Long-running; one process per parent-Claude session.
```

### 8.2 Tools exposed

```
ship_design_docs(
    paths: string[],          // design doc paths, relative to repo root
    repo_url: string,         // https://github.com/org/repo
    default_branch?: string,  // default "main"
    model?: string,
    concurrency?: int,        // default 4
    open_pull_requests?: bool // default true
) -> { run_id: string }
  # Resolves params → ShipDesignDocsParams → recipe → *config.Config →
  # spawns `orchestra run` (§8.4 below) in the workspace .orchestra-mcp/runs/<run_id>/.
  # Returns immediately with the run_id; execution is async.

list_jobs() -> [{
    run_id: string,
    started_at: string,
    status: "running" | "done" | "failed" | "blocked",  // "blocked" if any team is blocked
    teams: [{
        name: string,
        status: "pending" | "running" | "done" | "blocked" | "failed",
        signal_status: "" | "done" | "blocked",
        signal_summary: string,
        signal_pr_url: string,
        signal_reason: string,
        cost_usd: number
    }]
}]
  # Reads .orchestra-mcp/runs/*/state.json. Live for active runs.

get_status(run_id: string) -> { ...same shape as one entry from list_jobs... }

unblock(run_id: string, team: string, message: string) -> { ok: bool }
  # Resolves the workspace from run_id, then equivalent to:
  #   cd <workspace> && orchestra msg --team <team> --message <message>
  # (Same code path as `cmd/msg.go`; just reachable from MCP.)
```

### 8.3 Tool input shapes (JSON Schema for the MCP layer)

Standard MCP tool definitions; concrete shapes are in the implementation.

### 8.4 Process model: subprocess, not in-goroutine

The MCP server **shells out to `orchestra run`** for each `ship_design_docs` call. Reasons:

1. **Isolation.** A panic in a recipe-driven run shouldn't take down the MCP server. The user might be steering 4 concurrent runs from one parent Claude session.
2. **Reuse.** `orchestra run` already handles workspace setup, state archival, signal handling, archive-on-exit. Reimplementing in-goroutine duplicates that.
3. **Independent lifetimes.** The MCP server is the parent-Claude's lifetime (a Claude Code session). Runs outlive the parent if the user closes Claude. Subprocess detachment is straightforward; in-goroutine isn't.

The MCP server tracks subprocess state in `~/.config/orchestra/mcp-runs.json` (or similar) — enough to map `run_id → workspace_path → PID`. `list_jobs` reads workspace state.json files; doesn't need to talk to the subprocess.

For `unblock`, the MCP server invokes `orchestra msg` as a sub-subprocess, OR (cleaner) calls into `internal/run` directly since `orchestra msg` is already designed to read state.json without acquiring the run lock ([DESIGN-v2.md](./DESIGN-v2.md) §11). The lock-free read makes either path safe.

### 8.5 Workspace layout for MCP-driven runs

```
~/.config/orchestra/mcp-runs.json   # registry of {run_id → workspace_path, started_at, pid}
~/.local/share/orchestra/mcp-runs/<run_id>/
  ├── state.json
  ├── results/<team>/summary.md
  ├── logs/<team>.ndjson
  └── archive/
```

OR (simpler) the MCP server creates each workspace under the parent-Claude's CWD as `.orchestra-mcp/runs/<run_id>/`. Decide during P2; depends on whether the user expects to `cd` into a workspace and run CLI commands directly.

### 8.6 Auth and lifetime

- MCP server inherits `ANTHROPIC_API_KEY` and `GITHUB_TOKEN` from the environment that started it. No additional auth.
- The MCP server is not network-exposed by default (stdio). HTTP transport (`--transport http`) is for advanced cases and requires the user to set their own auth (out of scope for v0).
- The server runs until killed. No idle timeout; runs survive parent-Claude restarts because they're subprocesses.

### 8.7 New package

```
internal/mcp/
├── server.go      # MCP server implementation (using mark3labs/mcp-go or anthropic-mcp-go — pick one)
├── tools.go       # Tool definitions + handlers
├── runs.go        # Subprocess management + run registry
└── server_test.go
```

---

## 9. Notifications

### 9.1 v0 surface

When `signal_completion` fires (either status):

1. **Append-only log.** Write a JSON line to `<workspace>/notifications.ndjson`:
   ```json
   {"ts": "...", "run_id": "...", "team": "...", "status": "done", "summary": "...", "pr_url": "..."}
   ```
2. **Terminal bell.** If stdout is a TTY, write `\a` and a one-line `[NOTIFY] <run_id>/<team>: <status> — <summary>`.
3. **System notification (best-effort).** Detect platform; shell out to:
   - macOS: `osascript -e 'display notification "..." with title "Orchestra"'`
   - Linux: `notify-send Orchestra "..."` (only if `notify-send` is on `$PATH`)
   - Windows: `powershell -Command "[System.Windows.Forms.MessageBox]::..."` or skip
   
   Failure to notify is logged and ignored — never blocks the run.

### 9.2 v1 surface (future, not in v0)

- ntfy.sh / pushover webhook integration
- Quiet hours / batched digest mode
- Per-run notification policy (always-page vs only-blocks vs only-done)

### 9.3 New package

```
internal/notify/
├── notify.go      # interface: Notify(ctx, Notification) error
├── log.go         # append-only log writer
├── terminal.go    # TTY bell + line
├── system.go      # platform-specific shell-outs
└── notify_test.go
```

The `signal_completion` handler in `internal/customtools/` calls `notify.Notify(...)` after updating state.

---

## 10. Architecture: where new code lands

### 10.1 New packages

```
internal/skills/        # Skill registration with Anthropic + local cache
internal/customtools/   # Custom-tool registry + signal_completion handler
internal/recipes/       # Recipe library (ship-design-docs only in v0)
internal/mcp/           # MCP server
internal/notify/        # Notification fan-out
```

### 10.2 Existing packages, wired in

| Package | Change |
| --- | --- |
| `internal/config/schema.go` | Add `Team.Skills` and `Team.CustomTools` fields. Validator rejects unknown skill names if MA backend; warns on local backend. |
| `internal/agents/` | No structural change. Skill resolution is engine-side; `EnsureAgent` already accepts pre-resolved `Skills`. |
| `cmd/run_ma.go` | New event-loop case for `agent.custom_tool_use` that dispatches to `internal/customtools` registry. |
| `cmd/run.go` | No change (backend selection unchanged). |
| `cmd/` | New commands: `mcp`, `skills register/ls/sync`. |
| `internal/spawner/` | No change (the union events already include custom-tool events). |
| `internal/store/` | Add `signal_*` fields to TeamState. |

### 10.3 What runs where

```
                 Parent Claude (user's main session)
                            │  (MCP stdio)
                            ▼
                 orchestra mcp (long-lived process)
                            │
                ┌───────────┴───────────┐
                ▼                       ▼
     ship_design_docs                 unblock
     spawns subprocess                shells `orchestra msg`
                │                       │
                ▼                       │
        orchestra run                   │
        (one per ship call)             │
                │                       │
                ▼                       ▼
        DAG runner ──► MA backend ──► N MA sessions
                            ▲              │
                            │              │ agent.custom_tool_use(signal_completion)
                            └──────────────┘
                                          │
                                          ▼
                            internal/customtools.Handle
                                          │
                            ┌─────────────┴─────────────┐
                            ▼                           ▼
                      store update                notify.Notify
                            │                           │
                            ▼                           ▼
                     state.json                  terminal bell + log + notify-send
```

---

## 11. State model additions

### 11.1 `TeamState` extensions

Add to `internal/store/state.go` (current TeamState):

```go
type TeamState struct {
    // ... existing fields ...

    SignalStatus  string    `json:"signal_status,omitempty"`   // "" | "done" | "blocked"
    SignalSummary string    `json:"signal_summary,omitempty"`
    SignalPRUrl   string    `json:"signal_pr_url,omitempty"`
    SignalReason  string    `json:"signal_reason,omitempty"`
    SignalAt      time.Time `json:"signal_at,omitempty"`
}
```

These are written via the existing `Workspace.UpdateTeamState(team, mutator)` funnel (DESIGN-v2 §10.2). No new write-path concerns.

### 11.2 Run-level status derivation

`get_status` and `list_jobs` derive a top-level run status from team states:

- Any team `failed` → run `failed`.
- Any team `signal_status: blocked` → run `blocked`.
- All teams `signal_status: done` → run `done`.
- Otherwise → run `running`.

No new persisted run-level field; it's derived state.

### 11.3 The notifications log

`<workspace>/notifications.ndjson` is append-only and host-process-owned. No coordination required. `list_jobs` doesn't read from it; the state.json signals are the source of truth.

---

## 12. End-to-end test plan

### 12.1 Fixture

Create a small scratch repo with two design docs:

```
test/integration/ship_feature/
├── repo/                          # the fixture repo (committed as a tar / git fixture)
│   ├── README.md
│   ├── go.mod                     # minimal Go module
│   ├── main.go                    # one-liner main
│   └── docs/
│       ├── feat-flag-quiet.md     # spec: add a --quiet flag
│       └── feat-flag-version.md   # spec: add a --version flag
└── README.md                      # how to run the integration test
```

Each design doc is short, unambiguous, and resolvable in <5 turns: "add a `--quiet` boolean flag to `main.go` that suppresses output", "add a `--version` flag that prints `0.1.0`". Trivial enough that a real `/ship-feature` run completes within the timeout under realistic MA latency.

### 12.2 Happy-path test

```bash
# Setup (one-time)
orchestra skills register ship-feature

# Run
orchestra mcp &
MCP_PID=$!

# From a parent Claude session connected to the MCP, invoke:
#   ship_design_docs(
#     paths=["docs/feat-flag-quiet.md", "docs/feat-flag-version.md"],
#     repo_url="https://github.com/<test-user>/orchestra-shipfeat-fixture"
#   )
```

**Acceptance:**

- Two MA sessions spawn within ~10 seconds of the `ship_design_docs` call.
- Within the per-team timeout (90 minutes default), both teams reach `signal_status: done` with non-empty `signal_pr_url`.
- The two PR URLs are real and visible on GitHub.
- `notifications.ndjson` contains two `done` entries.
- `state.json` shows both teams `status: done`, `signal_status: done`.
- `list_jobs` returns the run with overall `status: "done"`.
- Each team's `summary.md` contains a coherent description.
- No teams call `signal_completion(blocked)`.

### 12.3 Block-and-unblock test

Same setup, but introduce a **deliberately ambiguous** third doc: `docs/feat-flag-mystery.md` saying *"add a flag that does the right thing for users"*. Run the recipe with all three docs.

**Acceptance:**

- Two teams complete normally (as in §12.2).
- The third team eventually calls `signal_completion(blocked, reason: "<...>")` and pauses.
- A notification fires.
- `list_jobs` shows the run as `status: "blocked"` and the third team's `signal_status: "blocked"`.
- From the parent Claude session: `unblock(run_id, "ship-feat-flag-mystery", "make it a --debug bool that enables debug logging")`.
- The team resumes (next event arrives within ~10s), implements the disambiguation, ultimately calls `signal_completion(done)`.
- Final state: all three teams `done`, run `done`.

### 12.4 What we explicitly do not test in v0

- Concurrent `ship_design_docs` calls (two parallel runs from one MCP server). Should work — each spawns its own subprocess; verify ad-hoc but don't gate v0 on it.
- Skill drift mid-run (`/ship-feature` updates while a run is in flight). Edge case; the agent's skill_id is captured at agent-creation time and is stable across the run.
- MCP server restart mid-run. Subprocess `orchestra run` is independent of the MCP server's lifetime.
- ntfy / phone notifications.
- Performance under N>4 docs in one run.

---

## 13. Implementation phases

Each phase is a mergeable PR. Order matters; dependencies are explicit.

### P0 — Skill upload + cache (1–2 days)

- `internal/skills/` package: cache (gofrs/flock), content hash (NFC-normalized + line-ending-normalized), Files API client.
- `cmd/skills.go`: `upload`, `ls`, `sync` subcommands.
- Unit tests: cache read/write, drift detection, idempotent re-upload.
- **Real probe (inline, not a separate spike):** upload `ship-feature` SKILL.md against a real `ANTHROPIC_API_KEY`, capture the response shape, then start a one-off MA session that attaches the file as a resource and dumps its container filesystem (`ls -la /files/`, `find / -name SKILL.md -not -path "/proc/*" 2>/dev/null`, etc.). Document the actual mount path — that becomes the bootstrap-prompt directive in P2.

**Exit criterion:** `orchestra skills upload ship-feature` writes a valid cache entry; `orchestra skills ls` shows it; the file is reachable from inside an MA session at a documented path.

### P1 — Custom-tool plumbing + `signal_completion` (1–2 days)

- `internal/customtools/` package: `Definition`, `Handler`, registry.
- `internal/customtools/signal_completion.go`: the handler.
- `internal/notify/` package: log + terminal + best-effort system shell-out.
- `internal/store` extensions for `Signal*` fields.
- `cmd/run_ma.go`: new event-loop case for `agent.custom_tool_use` dispatching to the registry.
- Unit tests: handler updates state, fires notify, returns sensible result.

**Exit criterion:** A hand-rolled MA agent (using `orchestra spawn` against a temporary config) that calls `signal_completion(done, ...)` results in state.json having the right fields and a notification firing.

### P2 — Recipe + config-schema additions (1–2 days)

- `internal/config/schema.go`: `Team.Skills`, `Team.CustomTools`. Validator updates.
- `internal/recipes/ship_design_docs.go`: the recipe function.
- Engine wiring: resolve `Team.Skills` against the skills cache, resolve `Team.CustomTools` against the customtools registry, populate `AgentSpec.Skills` and `AgentSpec.Tools`.
- Unit tests: recipe produces a valid Config; resolution correctly errors on unknown skill names.

**Exit criterion:** `recipes.ShipDesignDocs(...)` produces a Config that `cmd/run_ma.go` can run end-to-end against a fixture repo (without the MCP layer yet — invoke the recipe from a test driver or a hidden CLI subcommand).

### P3 — MCP server (2–3 days)

- `internal/mcp/` package: server, tools (`ship_design_docs`, `list_jobs`, `get_status`, `unblock`).
- Subprocess management: spawn `orchestra run` per `ship_design_docs` call; track in `mcp-runs.json`.
- `cmd/mcp.go`: the CLI command.
- Integration test: MCP client (a small Go test driver, not a real Claude session) calls `ship_design_docs`, polls `list_jobs`, eventually sees `status: done`.

**Exit criterion:** From a script or test driver acting as a Claude-Code-like MCP client, the four tools work end-to-end against the fixture from §12.

### P4 — End-to-end integration test (1 day)

- `test/integration/ship_feature/` fixture (§12.1).
- Test runner script that exercises the full path (§12.2 happy + §12.3 block/unblock).
- Opt-in via `ORCHESTRA_MA_INTEGRATION=1` (matches the convention from [features/06-p16](./features/06-p16-multi-team-text-only.md)).

**Exit criterion:** The integration test passes against live MA + a real GitHub fixture repo.

### P5 — Docs + dogfood (0.5–1 day)

- README section: "Ship a batch of design docs."
- One-page `docs/ship-feature-quickstart.md`: setup → register skill → run via MCP.
- Dogfood it on this repo: take 2–3 outstanding design docs (e.g., the next features in `docs/features/`) and ship them through the workflow. Capture any rough edges.

**Total v0:** ~7–11 days.

---

## 14. Open questions (verify during implementation)

1. **Anthropic custom-skill registration API shape.** This design assumes there's a `POST /v1/skills` (or similar) that takes markdown + metadata and returns a `skill_id`. The MA SDK in this repo passes `SkillID` as a reference (`internal/agents/params.go:skillParams`) but doesn't perform registration. **First task in P0 is to find the actual endpoint** in [the Anthropic Go SDK](https://github.com/anthropics/anthropic-sdk-go) under `Beta.Skills` or equivalent, or via the platform docs. If skills are registered via a different mechanism (e.g., uploaded as files first, then converted), §5 needs a small rewrite. **Architecture is robust to this — only the registration flow changes.**

2. **`/ship-feature` canonical location.** This design assumes `~/.claude/skills/ship-feature/SKILL.md`. The project's `.claude/CLAUDE.md` references `/ship-feature` as a global skill but doesn't pin its source. Verify path before P0.

3. **Skill content format for registration.** Markdown with YAML frontmatter (Claude Code standard)? Just markdown? JSON-wrapped? The registration API will dictate. Document the answer in `internal/skills/registry.go` doc comments after P0.

4. **Skill version drift on re-register.** Does Anthropic's API return a *new* `skill_id` on content change, or bump a `version` on the same `skill_id`? `BetaManagedAgentsCustomSkillParams` carries both, so both shapes are supported, but the cache logic needs to know which.

5. **MCP transport choice.** `stdio` is the natural Claude Code attachment shape, but verify that the user's main Claude Code session can attach to a long-lived stdio MCP server (vs. requiring per-call spawn). If stdio doesn't fit, fall back to local HTTP on a Unix socket.

6. **Concurrent `ship_design_docs` runs.** If the user fires two recipe calls 30 seconds apart, both should run independently. Confirm during P3 — the subprocess model makes this trivially correct, but verify state.json isolation.

7. **PR review request mechanics inside `/ship-feature`.** The existing skill's behavior for "request reviews from copilot, codex, claude" is assumed but not verified for this design. If the skill *doesn't* do this today, either extend the skill (preferred — keep orchestration-agnostic) or add a `request_reviews` custom tool the agent can call.

8. **Repo URL inference.** `ship_design_docs(paths, repo_url)` is explicit. Should `repo_url` default to inferred-from-CWD when CWD is a git repo? Marginal convenience; defer.

9. **Recipe path of design docs.** The recipe uses `paths` as repo-relative. If the user's design docs live *outside* the target repo, we'd need to upload them as `resources: [{type: "file", file_id}]` per [DESIGN-v2.md §9.6](./DESIGN-v2.md). v0 assumes docs are in the target repo. Document the constraint.

10. **What happens if `signal_completion` is called twice in one session.** Idempotent on second call (no-op + return success), or error? Idempotent is friendlier to a confused agent. Pick idempotent unless it hides a bug.

---

## 15. What this enables removing from orchestra (post-v0)

Once §12's tests pass, the deletion case for the surface below becomes concrete and defensible. Each item is a candidate; final cuts happen as follow-up PRs after v0 has run real workloads.

### 15.1 The companion-skills constellation

`/orchestra-coord`, `/orchestra-inbox`, `/orchestra-monitor`, `/orchestra-msg`, `/orchestra-init` were built to give the user a way to drive runs and observe state from within Claude Code when there was no MCP surface. Once `orchestra mcp` is the entry point:

- `orchestra-monitor` is replaced by `list_jobs` + `get_status` MCP tools.
- `orchestra-inbox` is replaced by `get_status` (the `signal_*` fields on each team).
- `orchestra-msg` is replaced by `unblock`.
- `orchestra-coord` is replaced by the parent Claude session itself.
- `orchestra-init` is replaced by recipes (the user describes intent in chat; the LLM picks a recipe).

**Action:** delete the five skills under `.claude/skills/orchestra-*/` and `.agents/skills/orchestra-*/`. README section on "Coordinating an orchestra run" gets a single line: "Use the orchestra MCP server from your main Claude session."

### 15.2 The file-based message bus

`internal/messaging/` (~209 lines + skill plumbing) exists to let teams pass messages mid-run on the local backend. The MA backend has no file-bus (DESIGN-v2 explicit non-goal), and the ship-design-docs recipe doesn't need cross-team coordination — each team is independent.

**Action:** if local-backend usage drops to zero in practice (track in P5 dogfood), gut `internal/messaging/` and the file-bus prompt fragments in `internal/injection/builder.go`. Otherwise, leave it as a local-backend-only feature and clearly mark it as such in the README.

### 15.3 The autonomous coordinator agent

`internal/injection/coordinator.go` and the `coordinator:` config block exist to spawn a long-lived "coordinator" Claude session that polls inboxes and resolves cross-team blockers. Under MA it's already a no-op-with-warning. With MCP unblock and per-team signals, the coordinator's job is the parent Claude session's job.

**Action:** mark `coordinator:` deprecated in v3, remove in v4. Or: cut now and accept the local-backend functionality regression as the cost of simplification.

### 15.4 Members under MA — already removed

DESIGN-v2 already documents members as a no-op-with-warning under MA. The ship-design-docs workflow is solo-lead per team. **No action**, but the ongoing presence of `Team.Members` in the MA path is dead weight. Consider: gate `Team.Members` parsing behind `backend.kind == local` and reject under MA at validate-time.

### 15.5 The `examples/miniflow` subproject

`examples/miniflow/` is a separate Go project (~20 files, its own server/cli/store/handler layout) inside the orchestra tree. Its connection to orchestra's actual flow is unclear from the README. If it's a learning artifact rather than a tested example, move it out.

**Action:** in P5, audit miniflow's role. If unused, delete or relocate. If used, document why.

### 15.6 The `.claude/worktrees/` directory contents

Multiple stale worktrees (`eager-mahavira-ac3230`, `lucid-napier-222a7f`, `blissful-euler-45fb2e`) shadow the main tree with ~hundreds of duplicated source files in glob output. This is dev artifact, not source.

**Action:** add `.claude/worktrees/` to `.gitignore` if not already; ensure no committed content under those paths. Cosmetic but cleans up tooling output.

### 15.7 Validation warnings with phase pins

`cmd/run_ma.go` warnings still reference "in P1.4" / "in P1.6" phase markers. [features/06](./features/06-p16-multi-team-text-only.md) §5.4 already flags this. After P5: drop the version pin in any remaining warnings; "not supported under managed_agents" without a phase reference reads better.

### 15.8 The endgame

Orchestra core ends up as: **DAG runner + MA spawner + recipe executor + MCP server + skills/customtools/notify add-ons**. Local backend, file-bus, coordinator, members, companion-skills surface — all become candidates for removal once this v0 ships and proves out.

This isn't a commitment to remove them — it's the deletion case becoming legible. The actual cuts happen as follow-up PRs gated on dogfood evidence.

---

## 16. Appendix: how this composes with future role-splits

v0 has one role: implementer. The same primitives extend naturally to multi-role workflows:

- **Architect** → another skill (`/design-doc`), another team in tier 0 producing a doc, downstream implementer team depends on it.
- **Critic** → skill (`/critique`), team that consumes architect's output and emits `signal_completion(done)` only after its `attacks[]` are addressed.
- **Closer** → skill (`/merge`), team that takes the implementer's PR and reviewer outputs, decides merge vs. escalate.

Each new role adds: one skill registration, one recipe entry, one or more new custom tools (e.g., `request_revision`, `merge_pr`). The substrate (DAG, MA, custom-tool dispatch, notify, MCP) doesn't change.

The point of v0 isn't to ship the smallest *possible* workflow — it's to ship the smallest workflow that exercises *every* primitive. Once that's solid, layering richer team shapes is data (recipes), not code.

---

## 17. Review checklist

- [ ] §5 (skills mechanics) — sign-off after P0 spike confirms the registration API shape
- [ ] §6 (recipe + config-schema additions) — sign-off on `Team.Skills` / `Team.CustomTools` field shape
- [ ] §7 (sentinel protocol) — sign-off on `signal_completion` schema and idempotency choice (Q10)
- [ ] §8 (MCP server) — sign-off on subprocess vs. in-goroutine and workspace placement (§8.5)
- [ ] §9 (notifications) — sign-off on v0 surface; ntfy deferred
- [ ] §10–11 (architecture / state model) — sign-off on package layout and schema additions
- [ ] §12 (test plan) — sign-off on fixture shape and acceptance criteria
- [ ] §13 (phases) — sign-off on P0 → P5 ordering
- [ ] §15 (deletion candidates) — sign-off on which removals to do as v0 follow-up vs. defer
