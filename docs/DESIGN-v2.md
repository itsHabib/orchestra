# Orchestra v2 — Managed Agents backend

Status: **Proposed (spike complete 2026-04-18; amended)**
Owner: @itsHabib
Last updated: 2026-04-18
Spike: [SPIKE-ma-io-findings.md](./SPIKE-ma-io-findings.md) — T1/T3/T4 executed live against MA (see `docs/spike-output/ma-io/` for raw evidence). Q1 answered negatively — the Files API has no session-scoped view of container-written files — and §5 D5, §9.6, §10, §12.2, §13, §14, §16 were rewritten accordingly. The artifact medium pivoted from Files API to GitHub. Q7 (credential injection) resolved from platform docs on 2026-04-18: vaults are MCP-auth-only and never reach the container; `github_repository` resources carry auth directly via `resources[].authorization_token`. No open gates remain.

> **TL;DR.** v2 introduces a second backend — `backend: managed_agents` — that runs each DAG team as a Claude Managed Agents (MA) session. The existing `backend: local` (spawns `claude -p` subprocesses, supports team-with-members + coordinator + file-bus) is **unchanged**. The MA backend is deliberately narrower: DAG orchestration only. No members, no coordinator, no cross-team messaging. What MA gives us in return is durable cloud sessions, native steering, managed container infra, and a cleaner path to an embeddable Go SDK.
>
> This document specifies only the MA backend and the Spawner abstraction that hosts both. It is self-contained; an agent picking it up cold should be able to implement Phase 1 without external context.

---

## 1. Goals & non-goals

### Goals

- Add a second backend, `managed_agents`, that runs an orchestra DAG against MA sessions.
- Keep the `local` backend — and every feature it supports (members, coordinator, file-bus, `/loop`) — **untouched** in v2.
- Make MA's durability a first-class feature: resume runs after disconnect via stored `session_id`s.
- Define a `Spawner` Go interface so both backends share a contract, and future backends can plug in.
- Prepare the ground for Phase 2: extracting a public Go SDK under `pkg/`.

### Non-goals (v1 of this design)

- **Team-with-members under MA.** Claude Managed Agents orchestra teams run as a single lead session. `members:` in config is a no-op under `managed_agents` (warning emitted). Team hierarchy is a future chapter, contingent on the MA multiagent research preview going GA and demand surfacing.
- **Coordinator agent under MA.** The `coordinator:` config block is ignored under `managed_agents` (warning emitted). Use `backend: local` if you need a coordinator.
- **Cross-team file-bus messaging under MA.** `.orchestra/messages/` is a local-backend feature. Under MA, cross-team communication happens implicitly via dependency-result injection (as v1 already does) and via the Files API for binary/file artifacts. Explicit team-to-team messaging at runtime is out of scope.
- **Custom orchestra tools / MCP server.** Not needed for DAG-only orchestration. Deferred along with cross-team messaging.
- **Stalled-team reconciler, verify-on-complete, duration anomaly flags.** Worth porting from cortex eventually, but not on the MA v1 critical path.
- **Server-mode / daemon.** Stays off the critical path. Durable MA session IDs + stateless CLI cover the "close laptop, resume tomorrow" story without a daemon.
- **Bundled UI / dashboard.**

The aggressive scope cut is the point: orchestra-under-MA in v1 does **one thing** — run a DAG of agents, durably.

---

## 2. Glossary

| Term | Meaning |
| --- | --- |
| **MA** | Claude Managed Agents (Anthropic platform product). |
| **Backend** | The runtime that executes a DAG team. v2 has two: `local` (today, unchanged) and `managed_agents` (new). |
| **Agent (MA)** | Versioned MA resource: model + system prompt + tools + MCP servers + skills. Created via `POST /v1/agents`. |
| **Environment (MA)** | Cloud container template: packages + networking. Each session gets its own isolated container instance from the template. Created via `POST /v1/environments`. |
| **Session (MA)** | Running agent instance within an environment. Durable ID. Created via `POST /v1/sessions`. Owns a filesystem container not shared with other sessions. |
| **Event (MA)** | Unit of communication between client and a session. SSE stream + REST list. Every event has an `id` + `processed_at`. |
| **Team (orchestra)** | A node in the DAG. In v2 under `managed_agents`, each team maps to exactly one MA session running the team's lead prompt. |
| **Run** | One invocation of `orchestra run`. Produces one `.orchestra/state.json` for its backend. |

---

## 3. Background: orchestra v1 today

Orchestra is a Go CLI that reads `orchestra.yaml`, builds a DAG via Kahn's algorithm (`internal/dag/dag.go`), and spawns one `claude -p` subprocess per team lead via `internal/spawner/spawner.go:79-323`. Parallel within a tier, sequential across tiers. File-based message bus in `internal/messaging/bus.go:1-209`. State in `internal/workspace/` written atomically via `internal/fsutil/atomic.go`. Prompt construction in `internal/injection/`. Config in `internal/config/schema.go`.

All v1 behavior is preserved when `backend: local` is selected (the default in v2 until we flip it — see §11).

---

## 4. What Managed Agents gives us

Facts from the MA platform docs (as of April 2026), relevant to this design:

- **Beta header:** `anthropic-beta: managed-agents-2026-04-01`. Go SDK sets it automatically.
- **Agent resource** (`POST /v1/agents`): fields `name`, `model`, `system`, `tools`, `mcp_servers`, `skills`, `callable_agents`, `metadata`, `description`. Versioned; updates bump version.
- **Environment resource** (`POST /v1/environments`): fields `name`, `config.type` (`cloud`), `config.packages` (apt/cargo/gem/go/npm/pip), `config.networking` (`unrestricted` or `limited + allowed_hosts + allow_mcp_servers + allow_package_managers`). Not versioned.
- **Session resource** (`POST /v1/sessions`): fields `agent` (id or pinned `{type, id, version}`), `environment_id`, `vault_ids`, `resources` (including file resources and `github_repository` resources). Statuses: `idle` | `running` | `rescheduling` | `terminated`. Cumulative `usage` object.
- **Events:** User-sent types — `user.message`, `user.interrupt`, `user.tool_confirmation`, `user.custom_tool_result`, `user.define_outcome`. Agent-emitted types — `agent.message`, `agent.thinking`, `agent.tool_use`, `agent.tool_result`, `agent.mcp_tool_use`, `agent.mcp_tool_result`, `agent.custom_tool_use`, `agent.thread_context_compacted`. Session types — `session.status_*`, `session.error`. Span types — `span.model_request_*`.
- **Creating a session does not start work.** You must send a `user.message` event to begin.
- **Steering is native.** `user.message` sent to a `running` or `idle` session is processed as the next input. `user.interrupt` stops the current turn; can be immediately followed by `user.message` to redirect.
- **Durability.** Session state (including filesystem, conversation history, usage, status) is held server-side. Event history is persisted and can be fetched. Stream reconnection: open SSE + list events, dedupe by ID, continue.
- **Filesystem isolation.** "Each session gets its own container instance. Sessions do not share file system state." (environments docs)
- **Files API.** `POST /v1/files`, `GET /v1/files?scope_id=<session_id>`, `GET /v1/files/{id}/content`. Files can be added to a session via `session.resources` at creation or via `POST /v1/sessions/{id}/resources`; the current documented cap is 100 file resources per session.
- **Rate limits:** 60 creates/min, 600 reads/min per org.
- **Pricing:** token-based; prompt cache with 5-minute TTL.

Key implications for our design:

1. **Sessions do not share a filesystem AND the Files API has no view of container-produced files.** Verified by spike T1: `Files.List(scope_id=session_id)` returns 400 *"unknown field scope_id"*; there is no `/v1/sessions/{id}/files` endpoint; `session.resources` is input-only. The MA product does not treat container writes as first-class artifacts. Cross-team artifact flow must therefore use an external substrate — in practice, GitHub.
2. **Steering is a real primitive.** Human interventions can be direct `user.message` events — no polling needed.
3. **Durability is server-side.** Orchestra stores `session_id`s; session state lives on Anthropic's infra. No daemon required for resume.
4. **Repo I/O via `github_repository` session resources is the primary artifact path.** Team A's deliverable is a branch push; orchestra reads the branch head via the GitHub API; team B mounts the same repo checked out to A's branch. The Files API remains useful only for small host-side input uploads.
5. **Credentials for `git push` come from the `github_repository` resource itself, not from vaults.** Spike T4 + container probe confirmed no Anthropic or GitHub creds are present in the container by default. Per the [GitHub docs](https://platform.claude.com/docs/en/managed-agents/github), each `github_repository` resource carries its own `authorization_token` (a raw PAT, write-only at the API, rotatable via `Sessions.Resources.Update`; repos are cached across sessions that reuse the same URL). Vaults are an adjacent primitive scoped to MCP-server authentication only — each credential binds to an `mcp_server_url` — so `vault_ids` only becomes relevant if an agent uses an MCP server that needs auth (e.g. the GitHub MCP for PR creation, see §9.6 and §14 Q8).
6. **`callable_agents` is research preview** and gives us threads for one level of delegation. **We do not depend on it in v1** — the v2 MA backend is single-lead per team. Multi-level team hierarchy (members) stays on the local backend.

---

## 5. Decisions

| # | Decision | Why |
| --- | --- | --- |
| **D1** | **Two backends, selected by `backend:` in `orchestra.yaml`.** `local` is today's behavior, unchanged. `managed_agents` is new. Default stays `local` in v2.0; flipped to `managed_agents` only after migration tooling is ready. | Lets us ship the new runtime without destabilizing existing users. Each backend has its own feature set — picking the backend implicitly picks the capabilities. |
| **D2** | **Under `managed_agents`: one MA session per team, DAG-only.** `members:` and `coordinator:` are honored only under `local`; under MA they are ignored with a validation warning. | Keeps v1 scope small. Team-with-members needs MA threads (research preview, one-level-delegation cap). Coordinator needs real-time cross-session observation. Both are significant extra design surface; neither is on the critical path. |
| **D3** | **`Spawner` is a Go interface.** v1 code (`internal/spawner/spawner.go:79-323`) is refactored into `LocalSubprocessSpawner`; `ManagedAgentsSpawner` is the new implementation. Both implement the same `Spawner` + `Session` interfaces. | Clean seam, idiomatic Go, mockable for tests, forward-compatible with future backends. |
| **D4** | **Event contract is modeled on MA's shape** (typed union: agent message, thinking, tool use/result, session status, span). The `local` backend's implementation adapts `claude -p` stdout NDJSON upward into this shape (as its existing parser already does, see `internal/spawner/spawner.go:163-284`). | Preserves the expressiveness MA gives us. Lowest-common-denominator would defeat the migration. |
| **D5** | **Artifacts flow through GitHub, not the Files API** (MA backend only). Each team is given a `github_repository` resource carrying its own `authorization_token` (a GitHub PAT supplied per-session by orchestra). Teams commit and push to a feature branch; orchestra resolves the branch head via the GitHub API on `session.status_idle + end_turn` and records `repository_artifacts[]` (url, branch, base_sha, commit_sha) in `state.json`. Downstream teams mount the same `github_repository` checked out to the upstream branch. Small textual outputs continue to be inlined into downstream prompts from the final `agent.message`. | Spike T1/T3/T4 verified the Files API is not a viable artifact medium under MA: `Files.List(scope_id=session_id)` is rejected, no endpoint exposes container-produced files, and the agent has no credentials in-container by default. GitHub is the substrate the MA product implicitly assumes for code-shaped deliverables, and "commit it to a repo" is the cleanest path even for non-code artifacts. |
| **D6** | **Durable resume via `session_id`.** `.orchestra/state.json` records each team's `session_id` + `last_event_id`. `orchestra resume` reconnects to each live session, dedupes events by ID, rebuilds in-memory state, continues the DAG. | MA gives us server-side durability. We just need to remember the IDs. |
| **D7** | **Agents and environments are cached orchestra resources,** keyed by project + role + content hash. Stored in `~/.config/orchestra/agents.json` and per-run `registry.json`. On spec drift, `Update` the agent (bumps MA version). | Avoids re-creating identical agents every run (hits MA rate limits and loses lineage). |
| **D8** | **Human steering via native MA events.** `orchestra msg <team> "..."` sends `user.message`. `orchestra interrupt <team>` sends `user.interrupt`. No custom tools, no file-bus, no polling. | Simplest possible story. MA's steering is strong enough that we don't need to build our own. |
| **D9** | **Public Go SDK extraction is Phase 2**, not Phase 1. The code is organized during Phase 1 to make Phase 2 mechanical (new packages land under `pkg/` from the start), but the public surface isn't stabilized or documented until Phase 2. | Scope discipline. Ship the runtime first; package the library once it's proven. |

---

## 6. Architecture

```
                           orchestra CLI (local process)
                                     │
                       ┌─────────────┴──────────────┐
                       │   pkg/orchestra.Engine      │
                       │   - loads Config            │
                       │   - builds DAG              │
                       │   - drives tiers            │
                       │   - holds state.Store       │
                       │   - owns spawner.Spawner    │
                       └─────────────┬──────────────┘
                                     │
                      ┌──────────────┴───────────────┐
                      │                              │
           LocalSubprocessSpawner          ManagedAgentsSpawner
                      │                              │
                 claude -p                  Anthropic MA API
            (unchanged v1 flow)         /v1/agents /v1/environments
                      │                  /v1/sessions /v1/files
                      │                              │
              local filesystem                cloud containers
              + v1 file-bus                   (isolated per session)
                                                      │
                                              Files API flow
                                              for cross-session
                                              artifacts (D5)
                                                      │
                                              host-side
                                              .orchestra/state.json
                                              .orchestra/results/
```

Shape of an MA-backend run:

1. `orchestra run` loads `orchestra.yaml`, validates, builds DAG.
2. For each team: `EnsureAgent` (create or reuse cached), `EnsureEnvironment` (one per project by default).
3. Tier loop:
   a. For each team in the tier concurrently: `StartSession` (with the team's `github_repository` resource — carrying its own `authorization_token` — attached; `vault_ids` only if the agent has an MCP server that needs auth), **open event stream first**, then send the initial `user.message` (built by the existing prompt injection code). Opening the stream before sending is required: MA only delivers events emitted *after* the stream is opened, so the send-first ordering loses the early `session.status_running` and initial `agent.thinking`/`agent.message` events.
   b. Stream events; persist each to `.orchestra/logs/<team>.ndjson`; update in-memory state; atomically rewrite `.orchestra/state.json` on status-changing events.
   c. On `session.status_idle` with `stop_reason: end_turn`: extract textual summary from the final `agent.message`; resolve the team's expected feature branch via the GitHub API (`GET /repos/{owner}/{repo}/branches/{branch}`); record `{url, branch, base_sha, commit_sha}` in `repository_artifacts[]`; mark team complete. If no branch was pushed, the team is marked `failed` with `last_error: "no branch pushed"`.
4. When all teams in a tier complete, proceed to next tier. Downstream teams start sessions with `github_repository` resources checked out to each upstream's recorded branch (multi-upstream → multiple mount points like `/workspace/upstream/<team-a>/`). Textual summaries are inlined into the downstream prompt as v1 already does.
5. **Exit handling:**
   - **All teams completed successfully:** persist final state, archive each MA session via `Beta.Sessions.Archive`. Archiving preserves event history + usage (needed for audit and `orchestra status` post-hoc) while signaling to MA that the session is closed, so the container is reclaimed. Sessions are *not* deleted by default — deletion is destructive and loses history. Users who want to reclaim MA-side storage can run `orchestra sessions rm --run-id=<id>` explicitly.
   - **Unhandled error during run:** persist state, do *not* archive sessions (they may be resumable), exit non-zero. User can later run `orchestra resume`.
   - **Partial failure (some teams terminated, others never started):** persist state with mixed statuses, don't archive the failed-but-still-reachable ones. Resume logic handles per-team archive/delete decisions based on current MA status.

---

## 7. Package layout

Phase 1 targets this layout (final public surface is stabilized in Phase 2):

```
orchestra/
├── cmd/                         # Cobra CLI
├── pkg/                         # Public (Phase 2 stabilizes)
│   ├── orchestra/               # Engine
│   ├── config/
│   ├── spawner/                 # Interface + backends
│   │   ├── spawner.go
│   │   ├── event.go
│   │   ├── local.go             # wraps internal/spawner (v1)
│   │   └── managed_agents.go
│   ├── state/
│   └── dag/                     # moved from internal/
├── internal/
│   ├── prompt/                  # renamed from injection/
│   ├── messaging/               # local-backend file-bus (unchanged)
│   ├── workspace/               # atomic I/O primitives
│   ├── registry/                # ~/.config/orchestra/ cache
│   ├── fsutil/
│   └── log/
```

The old `internal/spawner/` package is relocated under `pkg/spawner/local.go` so both backends sit behind the same interface. No code deleted in Phase 1 — v1 behavior is preserved bit-for-bit, just behind the interface.

---

## 8. Spawner interface

```go
package spawner

import (
    "context"
    "time"
)

// Spawner creates and manages backend-specific agent runtimes.
type Spawner interface {
    // EnsureAgent creates or reuses a backend agent. For the local backend this is a no-op
    // (claude -p doesn't have persisted agent resources); it returns a synthetic handle.
    EnsureAgent(ctx context.Context, spec AgentSpec) (AgentHandle, error)

    // EnsureEnvironment creates or reuses a container/env template. Local backend returns a no-op handle.
    EnsureEnvironment(ctx context.Context, spec EnvSpec) (EnvHandle, error)

    // StartSession starts a new session. Initial prompt is sent via Session.Send.
    StartSession(ctx context.Context, req StartSessionRequest) (Session, error)

    // ResumeSession reconnects to an existing backend session by ID. Local backend returns
    // an error (no durable sessions).
    ResumeSession(ctx context.Context, sessionID string) (Session, error)
}

type AgentSpec struct {
    Name         string
    Model        string
    SystemPrompt string
    Tools        []Tool
    MCPServers   []MCPServer
    Skills       []Skill
    Metadata     map[string]string
}

type EnvSpec struct {
    Name       string
    Packages   PackageSpec
    Networking NetworkSpec
}

type StartSessionRequest struct {
    Agent    AgentHandle
    Env      EnvHandle
    VaultIDs []string
    Resources []ResourceRef // file and repository refs attached at session creation
    Metadata map[string]string
}

type ResourceRef struct {
    Type string // "file" | "github_repository"

    // Type == "file"
    FileID string

    // Type == "github_repository"
    URL                string
    Checkout           *RepoCheckout
    AuthorizationToken string // in-memory only; never persisted to orchestra.yaml or state.json

    MountPath string
}

type RepoCheckout struct {
    Type string // "branch" | "commit"
    Name string // branch name, when Type == "branch"
    SHA  string // commit SHA, when Type == "commit"
}

type Session interface {
    ID() string
    Status(ctx context.Context) (SessionStatus, error)
    Usage(ctx context.Context) (Usage, error)

    Send(ctx context.Context, event UserEvent) error
    Events(ctx context.Context) (<-chan Event, error)
    History(ctx context.Context, after EventID) ([]Event, error)

    // ListProducedFiles returns files created during the session (MA: Files API scope_id=session_id).
    // Local backend returns files in the team's working dir.
    ListProducedFiles(ctx context.Context) ([]FileRef, error)
    DownloadFile(ctx context.Context, ref FileRef, w io.Writer) error

    Interrupt(ctx context.Context) error
    Cancel(ctx context.Context) error
}
```

Event union (tagged structs) mirrors MA's shape exactly. Appendix §14 has the full mapping. Local backend maps `claude -p` stdout NDJSON into the same union (`internal/spawner/spawner.go:163-284` already does most of this projection).

---

## 9. Managed Agents backend details

`pkg/spawner/managed_agents.go` uses `github.com/anthropics/anthropic-sdk-go` (Go SDK supports all MA endpoints: `Beta.Agents`, `Beta.Environments`, `Beta.Sessions`, `Beta.Sessions.Events`, `Beta.Files`).

**SDK version pin.** `go.mod` pins to a specific tag (not `latest`) so MA shape changes don't drift us unexpectedly. Phase 1 uses the latest available tag as of P1.2 (record the exact version in `go.mod` and reference it in a doc comment on the spawner package). SDK upgrades are deliberate PRs, reviewed against any MA-docs changes.

**API key sourcing** (tried in order): `ANTHROPIC_API_KEY` env var → `~/.config/orchestra/config.json` `api_key` field → error with actionable message. No key in config files ever; orchestra writes `config.json` only on `orchestra login` (a future convenience, not part of Phase 1). Beta header `managed-agents-2026-04-01` is set by the SDK automatically; do not hand-set it.

**Rate limits & retry.** MA caps at 60 creates/min and 600 reads/min per org. The MA backend wraps all API calls with a retry layer:

- **Retryable:** HTTP 429 (rate-limited) and 5xx. Exponential backoff with jitter starting at 1s, max 30s, max 5 attempts. Respect `Retry-After` header when present.
- **Non-retryable:** 4xx other than 429 (400, 401, 403, 404, 422). Fail fast with the response body in the error.
- **Transport errors:** retried under the same policy as 5xx.

Concurrent-tier bursts (creating N sessions at once) are throttled client-side by a single shared semaphore with burst cap = `rate_limit_burst` (default 20 in-flight creates). Steady-state throughput is naturally bounded by the server's 60/min, which we do not try to predict — the retry layer handles it.

**Session timeout / stalled detection.** Each team has a watchdog goroutine that tracks the `processed_at` of the last observed event. If no events arrive for `defaults.timeout_minutes` (existing v1 config field, default 30) while the session is `running`, the watchdog sends `user.interrupt` + a `user.message("Status check: you've been quiet for N minutes. If you are waiting on something, state it clearly; otherwise continue.")`. If silence persists for another `timeout_minutes` after the nudge, the team is marked `stalled` in `state.json`, the session is left alive (resumable), and the CLI surfaces the stall on exit. This is a lightweight replacement for the full cortex reconciler deferred to post-v1; it covers the 95% case without adding a separate reconciliation pass.

### 9.1 `EnsureAgent`

1. Compute `spec_hash = sha256(model + system_prompt + tools + mcp_servers + skills)`.
2. Look up in `~/.config/orchestra/agents.json` (file-locked) keyed by `<project>__<role>`.
3. On cache hit: GET the agent; if `spec_hash` matches the stored version's hash and the agent isn't archived → reuse. If hash differs → `Update` (bumps MA version). If the agent returns 404 (was deleted from the MA dashboard) → fall through to step 4.
4. **On cache miss (including after-404): reconcile before creating.** List existing MA agents via `Beta.Agents.List` and linear-scan for an agent whose `name == "<project>__<role>"` and `archived_at == null`. If exactly one match exists → adopt it (store its `id` + `version` in the cache), then treat as a cache hit (step 3). If zero matches → `client.Beta.Agents.New`. If multiple matches exist → log a warning naming all matching IDs, adopt the most recently updated, and surface the orphans via `orchestra agents ls`.
5. Record `{agent_id, version, spec_hash}` in both `~/.config/orchestra/agents.json` and the run's `registry.json`.

This list-and-adopt step (4) exists specifically to handle cache loss: a fresh laptop, a wiped `~/.config`, a CI run without persistent home. Without it, every cache-miss would create a duplicate MA agent with the same name (MA does not enforce name uniqueness), consume rate-limit budget, and pollute `orchestra agents ls`.

Agent names use `<project>__<role>` (double underscore) to namespace across projects without collisions.

**File lock portability:** cross-platform advisory locking on `~/.config/orchestra/agents.json` uses `github.com/gofrs/flock` (wraps `LockFileEx` on Windows, `flock(2)` on POSIX). Any concurrent `orchestra run` or `orchestra agents *` operation takes the lock for the duration of a read-modify-write cycle; contention is rare (operations are short) but correctness matters more than latency. P1.3 includes a cross-platform concurrency test.

### 9.2 `EnsureEnvironment`

Same pattern as `EnsureAgent`, keyed per project. One env per project by default; teams can override via `environment_override` in config.

### 9.3 `StartSession`

```go
s, err := mc.Beta.Sessions.New(ctx, BetaSessionNewParams{
    Agent:         BetaSessionNewParamsAgentUnion{OfString: &req.Agent.ID},
    EnvironmentID: req.Env.ID,
    VaultIDs:      req.VaultIDs,
    Resources:     toMAResources(req.Resources),
})
```

No prompt is sent here. The initial `user.message` is sent separately via `Session.Send`, by the engine, after the stream is opened.

### 9.4 `Session.Events`

Race-free pattern (from MA docs):

```go
stream := mc.Beta.Sessions.Events.StreamEvents(ctx, id, ...) // open first
// Engine sends initial user.message here
for stream.Next() {
    evt := stream.Current()
    out <- translate(evt)
}
```

On transport error, the backend transparently reopens the stream, uses `Beta.Sessions.Events.ListAutoPaging` to build a seen-set, and skips already-delivered event IDs.

### 9.5 Tool confirmation

Default permission policy on orchestra-managed agents is `always_allow`. No interactive confirmation loop in v1. (Future work: surface `always_ask` to the CLI / SDK consumer.)

### 9.6 Artifact flow (D5, expanded)

Under the MA backend, cross-session artifact flow is mediated by **GitHub**, not the Files API. The MA Files API has no session-scoped view of files written inside a container (verified by spike T1; see [SPIKE-ma-io-findings.md](./SPIKE-ma-io-findings.md) §Q1). The MA product is not designed to treat container outputs as first-class artifacts: outputs leave the container only via external writes (git push, API calls) or as text in the event stream.

#### Default flow (repo-backed deliverable)

1. Each team is given a `github_repository` session resource pointing at the project's repo (or a scoped fork), mounted at `/workspace/repo`, checked out to the branch declared by the team's upstream dependencies (or `main` for tier-0 teams). Auth comes from the resource's own `authorization_token` field (a GitHub PAT), sourced host-side per §12.2. The token is write-only at the API and rotatable mid-session via `Sessions.Resources.Update`. Repositories are cached across sessions that reuse the same URL, so fan-out-heavy tiers start faster on warm caches.
2. The team's prompt includes, as a standard suffix: *"Your working copy of the repo is at `/workspace/repo`. Commit your changes on a new branch named `orchestra/<team>-<run-id>` and push. Do not open a PR. Report the branch name and the final commit SHA in your last message."*
3. On `session.status_idle + stop_reason: end_turn`, orchestra resolves the branch head via the GitHub API (`GET /repos/{owner}/{repo}/branches/{branch}`), **not** by parsing the agent's prose. If the branch doesn't exist or is identical to its base, the team is marked `failed` with `last_error: "no branch pushed"`.
4. Orchestra records `{url, branch, base_sha, commit_sha}` as a `repository_artifacts[]` entry in `state.json`. Optionally opens a PR via the GitHub API, behind a config flag — not on by default (see Q8).
5. Downstream teams that depend on A start sessions with a `github_repository` resource checked out to A's recorded branch. Multi-upstream fan-in becomes multiple mount points: `/workspace/upstream/<team-a>/`, `/workspace/upstream/<team-b>/`, ... — each its own `github_repository` resource.

#### Secondary flow (small inputs, no repo)

The host-side orchestrator uploads small input files via the Files API (`POST /v1/files`) and attaches them as `resources: [{type: "file", file_id: ...}]` on the session at creation. This is for cases like "team A writes a one-page plan.md that team B should read" with no repo involved — produced text summaries continue to be captured from the final `agent.message` and inlined into the downstream prompt. The 100-file-resource per-session cap remains, but is not load-bearing because input fan-in is small by construction.

#### What's gone

Removed from the design:

- `ListProducedFiles` via `Beta.Files.List(scope_id=session_id)` — the field is rejected by the API.
- `DownloadFile` to `.orchestra/results/<team>/<filename>` — there is nothing to download.
- `max_artifact_mb` config knob — there is no file download path to cap.
- `artifacts[]` field in `state.json` (per-team file list with file_id + local path + SHA).

Repository artifacts (`repository_artifacts[]`) replace all of it. If a team genuinely needs to publish a non-repo binary (a rendered PDF, a compiled wheel), it does so by **committing the artifact to the repo** alongside the code — same branch/SHA tracking, no second code path.

#### PR creation

Two paths are available; orchestra picks one via `backend.managed_agents.open_pull_requests` (§12.2):

- **Default (`false`): host-side.** Orchestra calls the GitHub API directly from the CLI process after a team pushes. Atomic with run completion; doesn't grant the agent extra GitHub permissions.
- **Opt-in (`true`) via the GitHub MCP.** Declare `https://api.githubcopilot.com/mcp/` as an MCP server on the team's agent and supply vault-credentialed auth via `vault_ids` (a `static_bearer` credential bound to that URL). MCP toolsets default to `permission_policy: always_ask`; orchestra must explicitly set `always_allow` on the `mcp_toolset` entry, otherwise every PR-tool call will block the session on `stop_reason: requires_action` waiting for a `user.tool_confirmation`. See [permission-policies docs](https://platform.claude.com/docs/en/managed-agents/permission-policies).

Both paths need the PAT to have `repo` scope for private repos or `public_repo` for public ones.

### 9.7 Teammates (members) and coordinator

Under `managed_agents`, these are ignored:

- If a team has `members:`, `orchestra validate` emits a warning: *"team %q has members; members are not supported under backend: managed_agents and will be ignored. The lead will run solo."* `orchestra run` proceeds, ignoring members.
- If `coordinator.enabled: true` is set, `orchestra validate` emits: *"coordinator is not supported under backend: managed_agents; use backend: local if you need a coordinator."* `orchestra run` proceeds without the coordinator session.

No runtime code checks are needed beyond these validations — members and coordinators are simply never instantiated under the MA spawner.

---

## 10. State & resumption

### 10.1 `.orchestra/` layout (MA backend)

```
.orchestra/
├── state.json            # live run state
├── registry.json         # agent/env IDs used by this run
├── results/
│   └── <team>/
│       └── summary.md    # final agent.message text (only); no produced files
├── logs/
│   └── <team>.ndjson     # append-only event log (one JSON event per line)
└── archive/
    └── <run-id>/         # previous runs, moved here on next `orchestra run`
        └── {state.json, results/, logs/}
```

No `runs/<run-id>/` + symlink layout. State for the active run is flat; previous runs are moved to `archive/<run-id>/` when a new run starts. This keeps existing skills (`/orchestra-monitor`, `/orchestra-inbox`, etc.) working unchanged against the flat paths and avoids the Windows-symlink issue flagged in review.

**Archive pruning:** by default, keep the 20 most recent archived runs; older ones are deleted on the next `orchestra run`. Configurable via `defaults.archive_keep` in `orchestra.yaml` (0 disables pruning, retains everything). Archived state under `archive/<run-id>/` is small now that produced files no longer live there (see §9.6); pruning bias is conservative more for log volume than artifact volume.

**Note on messaging folders:** `.orchestra/messages/` is a local-backend-only subtree, created by the local backend when it initializes. The MA backend never creates it. Skills that read `.orchestra/messages/` remain local-backend-only (which they effectively already are — MA has no file-bus).

### 10.2 `state.json` schema (MA backend)

```json
{
  "project": "chatbot",
  "backend": "managed_agents",
  "run_id": "2026-04-17T09-22-33-abc123",
  "started_at": "2026-04-17T09:22:33Z",
  "environment_id": "env_01...",
  "teams": {
    "backend": {
      "status": "running",
      "agent_id": "agent_01...",
      "agent_version": 3,
      "session_id": "sesn_01...",
      "last_event_id": "evt_01...",
      "last_event_at": "2026-04-17T09:30:01Z",
      "result_summary": "",
      "repository_artifacts": [
        {
          "url": "https://github.com/org/repo",
          "branch": "orchestra/backend-20260417",
          "base_sha": "5b7e...",
          "commit_sha": "abc123...",
          "pull_request_url": null
        }
      ],
      "cost_usd": 0.42,
      "duration_ms": 45000,
      "input_tokens": 12000,
      "output_tokens": 3400,
      "cache_read_input_tokens": 18000,
      "cache_creation_input_tokens": 2200
    }
  }
}
```

All writes use `internal/fsutil.AtomicWrite` (write `.tmp` → `os.Rename`). Critically, **every in-process write to `state.json` goes through a single `Workspace.UpdateTeamState(team, mutator)` funnel that holds the process-level `sync.Mutex` for the full read-modify-write cycle.** Per-team event-loop goroutines must not call `SaveState(snapshot)` directly — that would be a lost-update hazard (two goroutines both load a stale snapshot, both mutate, both write; the second clobbers the first's update). The mutator-closure pattern makes the RMW atomic within the process. Cross-process safety (`orchestra run` + `orchestra status` in parallel) is handled by `status` being read-only — it reads state.json but never rewrites it, so there is no concurrent writer outside the single engine process. `orchestra msg` / `orchestra interrupt` similarly never write to local state (see §11).

### 10.3 Resume flow

`orchestra resume`:

1. Read `.orchestra/state.json`. Error if not MA-backend (`backend: local` doesn't support resume in v1).
2. For each team with `status in {running, idle}`:
   a. `Beta.Sessions.Get(session_id)`.
   b. If session is `terminated`: finalize from last persisted state; mark team `terminated`; record partial results.
   c. If session is `running`/`idle`/`rescheduling`: open event stream, list events (paginated), seed seen-set, tail live events, dedupe. Resume the engine's event-loop for this team.
   d. **If `Sessions.Get` returns the session as `idle` with `stop_reason: requires_action`** (tool confirmation pending when we last disconnected — or new ones that accumulated while we were offline): read `stop_reason.requires_action.event_ids` from the *live* response, and only issue `user.tool_confirmation` for those specific IDs. Do not replay the NDJSON log scanning for historical `agent.tool_use` entries — those may already be resolved, and the idempotency of `user.tool_confirmation` on a resolved `tool_use_id` is not documented. If the agent's permission policy is `always_allow` (v1 default), auto-`allow` each currently-pending ID; otherwise surface to the CLI. Re-check `Sessions.Get` after sending confirmations to verify `stop_reason` cleared.
3. Once all surviving teams are attached, continue DAG progression: schedule next-tier teams whose dependencies are complete.

If orchestra died between "session idle" and "repository artifacts recorded," resume re-resolves the team's branch via the GitHub API and re-records `repository_artifacts[]`. The operation is idempotent: the branch head is the source of truth, and re-reading it cannot lose information. There are no per-file retries because there are no per-file downloads.

---

## 11. Human steering

Two new CLI commands, MA-backend-only:

| Command | Action |
| --- | --- |
| `orchestra msg <team> "<text>"` | Reads `.orchestra/state.json` to resolve the team's `session_id`, then sends a `user.message` event to that session via the MA API. Writes nothing to local state or logs. |
| `orchestra interrupt <team>` | Reads `.orchestra/state.json` for `session_id`, then sends a `user.interrupt` event. Optionally followed by `orchestra msg <team> "<new direction>"`. Writes nothing locally. |

**State writes are intentionally one-way: the MA API is the only thing these commands touch.** The running `orchestra run` process already streams every event from the session (including the user-side events it sent and receives back as part of the session's event history) and persists them to `.orchestra/logs/<team>.ndjson` through its single writer. Having `orchestra msg` also write to that file from a separate process would race the engine's writer (whole-file atomic replace has no lost-update protection across processes) and duplicate entries the engine will log anyway. Audit of human-sent messages comes from the MA event stream — each `user.message` event carries the human's text and is persisted exactly once by the engine.

These commands do not require `orchestra run` to be attached — steering the session works in all cases — but the audit/log trail only updates if a reader (running `orchestra run` or a dedicated `orchestra tail`) is consuming the stream.

No file-bus writes, no forwarding loop, no custom tools.

---

## 12. CLI & config changes

### 12.1 CLI

All v1 commands (`init`, `validate`, `plan`, `run`, `spawn`, `status`) continue to work. Under MA backend:

- `orchestra run` routes to `ManagedAgentsSpawner`.
- `orchestra status` reads `state.json` and, if MA-backend, also calls `Beta.Sessions.Get` for live status. If the API call fails (network down, API key missing, rate-limited), `status` falls back to disk-only data and prints a banner line (`! live MA status unavailable: <reason>; showing last persisted state`). The command always exits 0 on successful disk read — it's a reporting tool, not a probe.
- `orchestra spawn <team>` starts a single MA session for one team (no DAG).

New commands (MA-backend only):

- `orchestra resume` — §10.3.
- `orchestra msg <team> "<text>"` — §11.
- `orchestra interrupt <team>` — §11.
- `orchestra sessions ls` — list MA sessions this orchestra has active.
- `orchestra agents ls` / `orchestra agents prune` — manage cached agent registry.

### 12.2 Config schema

Additions to `orchestra.yaml` (all backwards-compatible):

```yaml
name: chatbot

# NEW in v2.
backend:
  kind: local              # or: managed_agents    (default: local in v2.0)

  # Only used when kind == managed_agents:
  managed_agents:
    environment:
      name: chatbot-dev
      networking:
        type: limited
        allowed_hosts: ["api.example.com", "github.com"]
        allow_package_managers: true
      packages:
        pip: ["httpx", "pydantic"]
        npm: ["express"]
        apt: []                 # git 2.43.0 is preinstalled (spike-confirmed); listing it here is a no-op
    repository:                 # required for any team that produces a code-shaped artifact
      type: github_repository
      url: https://github.com/org/repo
      mount_path: /workspace/repo
      # Auth: orchestra reads a GitHub PAT (priority: GITHUB_TOKEN env var →
      # ~/.config/orchestra/config.json `github_token` → actionable error) and passes it
      # per-session as resources[].authorization_token. The token is write-only at the API
      # layer and never persisted to orchestra.yaml or state.json.
    vault_ids: []               # optional — only needed if a team's agent declares an MCP server
                                # requiring auth (e.g. the GitHub MCP for PR creation). Vaults
                                # are MCP-auth-only; they do not surface inside the container.
    agent_pinning: auto         # "auto" (latest) | "pinned"
    open_pull_requests: false   # if true, orchestra opens a PR via the GitHub API per team-pushed branch on completion

defaults:
  model: claude-opus-4-7
  max_turns: 200
  archive_keep: 20              # NEW: cap on archived runs under .orchestra/archive/
coordinator: { ... }            # ignored under managed_agents (warning)

teams:
  - name: backend
    lead: { ... }
    members: [...]              # ignored under managed_agents (warning)
    tasks: [...]
    depends_on: []
    # NEW: per-team override of the project-level environment.
    # If omitted, the project env applies. Fields merge shallowly over the project config.
    environment_override:
      networking: {type: unrestricted}
      packages:
        pip: ["torch"]          # only this team needs torch
```

A v1 config without the `backend:` block defaults to `backend.kind: local`. No migration required for existing users.

---

## 13. Implementation phases

### Phase 1: MA backend (blocking ship)

Each chapter is a mergeable PR. The spike is complete (see [SPIKE-ma-io-findings.md](./SPIKE-ma-io-findings.md)) and all open gates are resolved; P1.1 can start immediately.

0. **~~P1.0.5 — Vault credential confirmation.~~** *Resolved from docs (2026-04-18); no live probe needed.* The [vaults docs](https://platform.claude.com/docs/en/managed-agents/vaults) state every credential in a vault binds to an `mcp_server_url` and is injected at the Anthropic MCP gateway — vaults never surface credentials inside the container. GitHub-push auth flows through `resources[].authorization_token` on the `github_repository` resource instead (see [GitHub docs](https://platform.claude.com/docs/en/managed-agents/github), §9.6, and §8 `ResourceRef`). P1.4+ is unblocked.
1. **P1.1 — Spawner interface + relocate local backend.** Define `pkg/spawner.Spawner`, `Session`, `Event` union. Move `internal/spawner/` behind the interface as `pkg/spawner/local.go`. Migrate every call site that currently imports `github.com/itsHabib/orchestra/internal/spawner` (today: `cmd/run.go`, `cmd/spawn.go`, and any engine code in `cmd/` or a future `internal/engine/`) to the new package path. CLI keeps working with `backend: local` (default). Diff must be purely structural — zero behavior change, zero new tests required beyond what exists; `make test && make vet` green. *Spike-independent.*
2. **P1.2 — Anthropic Go SDK wiring + smoke test.** Add `github.com/anthropics/anthropic-sdk-go` at a pinned tag in `go.mod`. Implement `orchestra env show` that creates and tears down an MA environment. This chapter also validates API-key sourcing and the beta header. *Spike-independent.*
3. **P1.3 — `ManagedAgentsSpawner.EnsureAgent` + `EnsureEnvironment`.** Registry cache in `~/.config/orchestra/` with `gofrs/flock`. Spec-hash-based drift detection. List-and-adopt reconcile on cache miss (§9.1 step 4). `orchestra agents ls` / `prune` commands. Cross-platform concurrency test (spawn two goroutines both doing `EnsureAgent` on the same role; assert one creates, one adopts). *Spike-independent.*
4. **P1.1.5 — Prompt builder refactor.** *Spike-dependent; sized post-spike.* Refactor `internal/injection/builder.go` so the prompt is built from a `Capabilities` struct: `{HasFileBus, HasMembers, ArtifactPublishMode}`. Local backend passes `{true, team.HasMembers(), none}`; MA backend passes `{false, false, <mode>}` where `<mode>` comes from spike findings (likely `none` if Q1 works, `bash-upload` if Q2 needs explicit publish). Current prompt sections about `/loop` polling, file-bus bash recipes, and teammate assignment become conditional on the relevant capability being true. All v1 tests must still pass (prompts under `backend: local` are byte-identical).
5. **P1.4 — `StartSession` + `Events` + `Send` + watchdog.** Single-team end-to-end using a realistic fixture (not a synthetic "write no files" task): pick the simplest `examples/` project that produces a single textual deliverable (e.g. a summary.md-only task). Stream-first ordering (§6 step 3a). Include the per-team timeout watchdog (§9 intro). Result summary persisted to `.orchestra/results/<team>/summary.md`.
6. **P1.5 — Repo-backed artifact flow.** Wire `github_repository` resources into `StartSession`. After a team reaches `idle + end_turn`, resolve the expected feature branch via the GitHub API (`GET /repos/{owner}/{repo}/branches/{branch}`); on success, record `repository_artifacts[]` (url, branch, base_sha, commit_sha) in `state.json`; on missing branch, mark `failed` with `last_error`. Mount the upstream's pushed branch on each downstream session as a `github_repository` resource at `/workspace/upstream/<team>/`. If `open_pull_requests: true`, open a PR via the GitHub API. Fixture: an `examples/` project where team A modifies a file in `/workspace/repo`, pushes branch `orchestra/<team>-<run-id>`, and team B reads from that branch. No Files API calls in this chapter — that path no longer exists.
7. **P1.6 — Multi-team DAG runs.** Full tier-parallel orchestration under MA. Port a concrete picklist of examples (see *Test fixtures* below). For each, document `members:` sections that get ignored and any prompt edits needed for MA. Rate-limit retry and burst semaphore exercised under load.
8. **P1.7 — Validation warnings.** `orchestra validate` emits warnings for `members:` and `coordinator:` under MA backend. `orchestra validate` auto-migrates v1 configs by inserting a minimal `backend: local` block and prints the diff. Refuses to write if the YAML has comments that would be lost; prints the intended diff and exits.
9. **P1.8 — `orchestra resume`.** Read state, probe sessions via `Sessions.Get`, dedupe events, continue DAG. Handles archived + terminated + mid-tool-confirmation cases (§10.3d). Artifact re-download uses SHA verification not just existence check.
10. **P1.9 — Human steering CLI.** `orchestra msg`, `orchestra interrupt`, `orchestra sessions ls`, `orchestra sessions rm`. `msg`/`interrupt` are strictly one-way to MA (§11); `sessions rm` is the explicit destructive counterpart to archive-by-default on run exit.
11. **P1.10 — Docs + examples + flip default.** README section for MA backend, one full example under `backend: managed_agents`. Cost delta measurement vs `backend: local` on that example (§14.4). Default backend stays `local`; document the opt-in.

**Test fixtures under MA (P1.6 picklist).**

| Example | Ports cleanly? | Notes |
| --- | --- | --- |
| `examples/miniflow` | Partially | miniflow's "local webserver" tasks need `allow_package_managers: true` + `go` package in env. Team with `members:` runs as solo lead; member tasks collapse into lead tasks. |
| *(additional examples picked during P1.6)* | — | Any example using the file-bus `inbox/outbox` pattern for mid-run coordination must be flagged as "local-backend only"; do not attempt to port. |

The Phase 1 regression bar is: every MA-ported example passes under `backend: managed_agents` AND the unported examples still pass under `backend: local`.

### Phase 2: SDK extraction

3 chapters:

11. **P2.1 — Stabilize `pkg/` surface.** Finalize exported types. Add godoc. Document stability tier (experimental / stable).
12. **P2.2 — Embed example.** `examples/embed/main.go`: a ~100-line Go program that runs orchestra as a library, not via the CLI. Validates the surface is usable.
13. **P2.3 — Tag `v0.1.0`.**

### Later (not in this design)

- Team-with-members under MA (requires multiagent GA).
- Coordinator under MA.
- Cross-team messaging primitives under MA (custom tools or MCP).
- Stalled-team reconciler.
- Verify-on-complete checks.
- Server-mode / hosted orchestra.

---

## 14. Open questions

Resolve during implementation; none block the design (except where noted).

1. **~~Files API + repo I/O semantics.~~** **Resolved (2026-04-18).** Spike T1/T3/T4 confirmed `Files.List(scope_id=session_id)` returns 400 *"unknown field scope_id"*; no `/v1/sessions/{id}/files` endpoint exists; `session.resources` is input-only. The Files API cannot be the artifact medium. Repo I/O via `github_repository` resources is the primary path; Files API remains useful only for small host-side input uploads. See [SPIKE-ma-io-findings.md](./SPIKE-ma-io-findings.md) §Q1 and the §9.6 rewrite.
2. **~~Artifact size / format filtering.~~** **Resolved as moot.** No file download path exists, so there is nothing to filter or cap. `max_artifact_mb` was removed from the config schema.
3. **Rate limits at scale.** 60 creates/min. A 10-team cold run with fresh cache is ~11 creates (10 agents + 1 env). 20 teams is ~21. With the list-and-adopt reconcile step (§9.1 step 4), warm caches are even cheaper — most runs create 0 agents. Per-team work is now: 1 session create + 1 GitHub API call (branch read), no file downloads. Measure during P1.6 and document the supported ceiling.
4. **Cost vs. `claude -p`.** MA uses API tokens; `claude -p` rides the user's subscription. Do a pilot run of 2–3 `examples/` under both backends during P1.10 and document the delta. If 3x+, add a `max_budget_usd` safeguard.
5. **Prompt builder refactor scope.** Narrowed by the spike. Under MA, the artifact-publish prompt section is a fixed git-commit-and-push instruction (always the same shape: branch name `orchestra/<team>-<run-id>`, no PR opened by the agent). No branching on "upload via X." The prompt builder still needs a backend-aware `Capabilities` struct: `{HasFileBus, HasMembers, GitPushTarget}`, but `GitPushTarget` is just the upstream branch name resolved from dependencies. P1.1.5 chapter remains.
6. **~~Repository artifact source of truth.~~** **Resolved.** GitHub API is the source of truth: branch + base_sha + commit_sha + (optional) pull_request_url, recorded by orchestra after `idle + end_turn`. Agent prose is never parsed.
7. **~~Vault credential mechanics.~~** **Resolved from docs (2026-04-18).** Vaults are MCP-auth-only: every credential binds to an `mcp_server_url` (only `mcp_oauth` and `static_bearer` auth types exist, both requiring the URL) and is injected at the Anthropic MCP gateway, never into the container. GitHub-push auth uses `resources[].authorization_token` directly on the `github_repository` resource — write-only at the API, rotatable via `Sessions.Resources.Update`, and cached across sessions that reuse the same URL. See [vaults docs](https://platform.claude.com/docs/en/managed-agents/vaults) and [GitHub docs](https://platform.claude.com/docs/en/managed-agents/github).
8. **PR creation.** Open. Recommended default: orchestra host-side via the GitHub API, gated by `backend.managed_agents.open_pull_requests` (default false). Atomic with run completion, easy to pause/resume, doesn't require giving the agent extra GitHub permissions. The agent only ever pushes a branch. The alternative — declare the GitHub MCP (`https://api.githubcopilot.com/mcp/`) on the agent and let it open PRs — works but requires (a) a vault-credentialed `static_bearer` entry bound to that MCP URL and (b) setting `permission_policy: always_allow` on the `mcp_toolset` entry, since MCP toolsets default to `always_ask` and would otherwise block every tool call on `stop_reason: requires_action`.

---

## 15. Out of scope / future work

- Team members under MA (via multiagent threads, once GA).
- Coordinator under MA.
- Cross-team messaging under MA (custom tools, MCP server, or both).
- Stalled-team reconciler, verify-on-complete checks, duration anomaly flags.
- Server-mode / orchestra daemon.
- Hosted orchestra.
- Non-Anthropic backends (OpenAI, local Ollama).
- Mesh / gossip orchestration modes.
- Bundled TUI or web UI.
- Scheduled / cron-triggered runs.

---

## 16. Appendix: event-type mapping

| MA event | Orchestra event | In NDJSON log? | Updates state.json? |
| --- | --- | --- | --- |
| `agent.message` | `AgentMessageEvent` | yes | `result_summary` on final |
| `agent.thinking` | `AgentThinkingEvent` | yes | no |
| `agent.tool_use` | `AgentToolUseEvent` | yes | counters only |
| `agent.tool_result` | `AgentToolResultEvent` | yes | no |
| `agent.mcp_tool_use` | `AgentMCPToolUseEvent` | yes | no |
| `agent.mcp_tool_result` | `AgentMCPToolResultEvent` | yes | no |
| `agent.custom_tool_use` | `AgentCustomToolUseEvent` | yes | not used in v1 |
| `agent.thread_context_compacted` | `AgentThreadContextCompactedEvent` | yes | counter |
| `session.status_running` | `SessionStatusRunningEvent` | yes | `team.status` |
| `session.status_idle` | `SessionStatusIdleEvent` | yes | `team.status`; behavior by `stop_reason`: `end_turn` → resolve team's expected feature branch via the GitHub API, record `repository_artifacts[]`, mark team complete; `requires_action` → pause team goroutine, inspect `event_ids`, auto-confirm if policy allows (else surface to CLI), resume; `max_turns` → mark team `stalled`, record `last_error`; `error`/unknown → mark team `failed`, persist `last_error`, do not retry automatically |
| `session.status_rescheduled` | `SessionStatusRescheduledEvent` | yes | `team.status` |
| `session.status_terminated` | `SessionStatusTerminatedEvent` | yes | `team.status`, finalize |
| `session.error` | `SessionErrorEvent` | yes | `last_error` |
| `span.model_request_end` | `SpanModelRequestEndEvent` | yes | `team.usage` (tokens) |

Events not listed (multiagent thread events, outcome events) are persisted to the NDJSON log but otherwise ignored in v1.

---

## 17. Review checklist

- [x] [SPIKE-ma-io.md](./SPIKE-ma-io.md) — completed; findings in [SPIKE-ma-io-findings.md](./SPIKE-ma-io-findings.md); §5 D5, §9.6, §10, §12.2, §13, §14, §16 amended (2026-04-18)
- [x] ~~P1.0.5 vault confirmation~~ — resolved from docs (2026-04-18); vaults are MCP-only, GitHub auth via `resources[].authorization_token`
- [ ] Spawner interface (§8) — sign-off on exported types
- [ ] `state.json` schema (§10.2) — sign-off on `repository_artifacts[]` shape (no `artifacts[]`)
- [ ] CLI surface (§12.1) — sign-off on new commands (including one-way `orchestra msg` behavior in §11)
- [ ] Config schema (§12.2) — sign-off on `repository:` being required and `vault_ids` being optional (MCP-auth-only) under `managed_agents`, plus `open_pull_requests` default
- [ ] Phase 1 ordering (§13) — confirm P1.5 ("Repo-backed artifact flow") replaces the deleted Files API chapter
- [ ] Session exit lifecycle (§6 step 5) — confirm archive-by-default is the right call
