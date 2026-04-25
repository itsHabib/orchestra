# Feature: P1.9-A — Human steering CLI (orchestra → agent)

Status: **Proposed**
Owner: @itsHabib
Depends on: P1.4 ([p14-ma-session-lifecycle.md](./p14-ma-session-lifecycle.md)) (shipped) — `Session.Send` already supports `user.message` and `user.interrupt`. P1.6 ([06-p16-multi-team-text-only.md](./06-p16-multi-team-text-only.md)) (shipped) — multi-team session targeting needs to exist before steering targets become non-trivial.
Relates to: [DESIGN-v2.md](../DESIGN-v2.md) §11 (Human steering); future P1.9-B (agent → human via `requires_action`); future P1.9-C (tier gates).
Target: a Go program (CLI today, SDK consumer tomorrow) can send a message or an interrupt to a specific running team's MA session, and list the sessions an active run owns.

---

## 1. Why this chapter exists (and what it explicitly defers)

Today, an orchestra MA run is fire-and-forget. Once `orchestra run` is going, there is no first-class way to:

- send a corrective message to a team that's heading off the rails ("don't add a database, use the existing JSON store"),
- interrupt a team that's looping on something useless,
- inspect which sessions an active run owns.

The MA SDK already supports the underlying primitives. `Session.Send(UserEventTypeMessage)` and `Session.Send(UserEventTypeInterrupt)` are wired in `internal/spawner/managed_agents_session.go` and used today only for delivering each team's initial prompt. The session ID and team-name mapping is already persisted in `store.TeamState.SessionID`. What's missing is the CLI surface that lets a human (or, after P2.0, a Go program) deliver those events to a specific team during a live run.

This chapter ships the **outbound** half of human-in-the-loop only:

- `orchestra msg --team <name> --message <text>` — deliver a `user.message`.
- `orchestra interrupt --team <name>` — deliver a `user.interrupt`.
- `orchestra sessions ls` — show the active run's teams, statuses, session IDs, and last event IDs.

It deliberately does NOT ship:

- **Agent → human asks** (the `requires_action` / `user_tool_confirmation` flow). That's P1.9-B; it requires changes to the MA event translator and a new "waiting" team state. Strictly bigger.
- **Tier gates** (`pause_after: true` per team). That's P1.9-C; useful even without P1.9-B but separable.
- **`orchestra sessions rm`** (terminating sessions). That's a destructive op with its own design questions (does it count as "failed" or "cancelled" in the result? does it propagate to dependent teams?). Defer to P1.9-D.

Splitting the outbound and inbound directions matters because the outbound piece is small (~1 PR over existing primitives), genuinely additive, and unblocks the dogfood apps' "the user wants to nudge a running run" use case immediately. The inbound piece needs more design and live-MA verification of the tool-confirmation flow.

---

## 2. Requirements

**Functional.**

- F1. `orchestra msg --team <name> --message <text> [--workspace .orchestra/]` finds the active run's session for `<name>` and delivers a `user.message` event directly to MA via the SDK (not through the running orchestrator's `Session.Send`, which lives in a different process). Exits 0 on accepted-by-MA, non-zero on team not found / not running / send failure.
- F2. `orchestra interrupt --team <name> [--workspace .orchestra/]` does the same with `user.interrupt`. Same exit semantics.
- F3. `orchestra sessions ls [--workspace .orchestra/]` prints a table with one row per team in the active run whose status is `running` (the only steerable state — `pending` teams have no MA session ID yet). `--all` includes `pending` / `done` / `failed` / `terminated` rows for inspection. Columns: `team`, `status`, `steerable` (yes/no — `yes` iff `status == "running"` and `session_id != ""`), `session_id`, `agent_id`, `last_event_id`, `last_event_at`. Exits 0 with an empty table if no active run. (`sessions ls` is a list operation; absent input is an empty result, not an error. `msg` and `interrupt` need a target and exit non-zero on no-active-run; see F5.)
- F4. All three commands are no-ops on `backend: local` runs and print an actionable error: `"steering is only supported under backend: managed_agents (local-backend steering tracked as P1.9-E)"`. Deliberate scope cut for this chapter — see §3 out-of-scope and §5.8.
- F5. `orchestra msg` and `orchestra interrupt` target the **active** run. If there is no active run, both exit non-zero with `"no active orchestra run in <workspace>"`. (`sessions ls` exits 0 with an empty table in the same condition; see F3.) The lookup uses a lock-free read of `state.json` (the file is atomically written) — no shared lock dependency; see §5.3.
- F6. `orchestra msg` and `orchestra interrupt` require the named team to be in `status: running` (matching F3's `Steerable` definition). If the team is `pending`, `done`, `failed`, or `terminated`, exit non-zero with the current status in the error message. The check is best-effort (TOCTOU: the team can transition between read and send); on send-time MA errors, the CLI surfaces them with the actual MA response.
- F7. The send is **at-least-once** under transient errors — MA's `Beta.Sessions.Events.Send` is not idempotency-keyed and the CLI does not deduplicate. `--help` documents this: in the rare 5xx-then-success case, the agent may observe the event twice. `--no-retry` flag bypasses retry entirely for callers that need at-most-once (interrupts especially).
- F8. The MA event translator (`internal/spawner/managed_agents_session.go`) is extended to recognize `user.message` and `user.interrupt` events. When MA echoes a steered event back through the running session's stream, the translator: (a) advances `LastEventID` / `LastEventAt`, (b) emits a `r.reportMAEvent` log line so coordinators tailing `orchestra run` output see `[team] human: <text>`, (c) does NOT mutate team status. Without this extension, F3's `last_event_at` column is stale and the run log gives no visible cause for the team's behavior change.
- F9. The steering helper code lives in `internal/spawner/steering.go` (next to `toSessionEventSendParams` which it reuses). It does NOT live in a new `internal/steering/` package — that proposal had to import `managedSessionEventsAPI` (unexported) and would have duplicated the SDK-adapter glue. See §5.5.

**Non-functional.**

- NF1. **Single-writer invariant preserved.** The store mutator funnel (`store.UpdateTeamState`) is owned by the running orchestration process. P1.9-A's CLI talks to MA, not to the store. This keeps the existing concurrency story unchanged: only one process ever writes team state, and that process is `orchestra run`.
- NF2. **Lock-free state read.** The CLI reads `state.json` without acquiring the run lock. The file is atomically written (write-temp-then-rename) so a snapshot is always consistent; the data may be stale but is never torn. Acquiring a shared lock cross-process is a separate problem this chapter declines to take on (the `gofrs/flock` shared-lock semantics on Windows are mandatory and untested in this repo — see §10.2).
- NF3. **No new persistence.** P1.9-A reads from existing `store.TeamState` fields (`SessionID`, `Status`); it writes nothing. MA's echo of the steered event is observed and recorded by the running orchestrator's event translator (extended in F8), not by the CLI.
- NF4. **Token / API budget.** Each `orchestra msg` is one MA `Beta.Sessions.Events.Send` call. The dollar cost is rounding-error per invocation. No per-run quota or rate limit is needed; the CLI is human-driven and rate-limited by humans typing.
- NF5. **Backend-agnostic CLI plumbing.** The cobra command definitions check `cfg.Backend.Kind` and dispatch; the MA-specific code lives in `internal/spawner/steering.go` (next to existing event-construction helpers). The CLI layer stays thin.
- NF6. **Cross-platform.** The CLI reads `state.json` (file I/O) and talks to MA over HTTPS. No platform-specific IPC, no Unix sockets, no named pipes. Works on Linux/macOS/Windows identically.

---

## 3. Scope

### In-scope

- New cobra commands: `orchestra msg`, `orchestra interrupt`, `orchestra sessions ls` (with `--all` flag).
- New helper functions in `internal/spawner/steering.go`:
  - `SendUserMessage(ctx, sessions managedSessionEventsAPI, sessionID, text string, retryAttempts int) error` — wraps `Beta.Sessions.Events.Send` with the workaround for the user.message Type bug already fixed in `toSessionEventSendParams`.
  - `SendUserInterrupt(ctx, sessions managedSessionEventsAPI, sessionID string, retryAttempts int) error` — same shape.
  - Both take an explicit `retryAttempts` so callers control the retry budget (CLI passes 0 by default for at-most-once on interrupts; passes the standard budget for messages).
- New exported factory in `internal/spawner`: `SessionEventsClient(ctx) (managedSessionEventsAPI, error)` — wraps `machost.NewClient` plus `&client.Beta.Sessions.Events`. Required because `managedSessionEventsAPI` is an internal interface; production callers (the new CLI commands) need a way to construct one without re-implementing the SDK adapter.
- **Translator extension** in `internal/spawner/managed_agents_session.go`:
  - Add `EventTypeUserMessage` and `EventTypeUserInterrupt` to the `EventType` enum.
  - Extend `translateAgentEvent` (or split out a `translateUserEvent`) to map the two MA event types to new `UserMessageEchoEvent` / `UserInterruptEchoEvent` orchestra-side events.
  - Wire `Session.apply` to handle the new events: bump `LastEventID` / `LastEventAt`, do not mutate status.
  - Wire `cmd/run_ma.go:reportMAEvent` to log a `[team] human: <text>` line on `UserMessageEchoEvent`.
- Unit tests for the helpers in `internal/spawner/steering_test.go` using fake `managedSessionEventsAPI`.
- Translator tests: feed each new event type through `translateMAEvent`, assert the SDK shape.
- CLI command tests in `cmd/{msg,interrupt,sessions}_test.go` using the existing test-substitute pattern.
- A live-MA test fixture: `test/integration/ma_steering/`. **Tests delivery, not model compliance.** The fixture sends a steering event and asserts (a) the event lands in `.orchestra/logs/<team>.ndjson` with the exact text, (b) `Beta.Sessions.Events.List` for the session contains it. It does NOT assert the agent obeys the steering — that's model-variance noise and burns budget on retries.
- Documentation: a "Steering a run" section in README under Backends, and a TESTING.md row for the new `make e2e-ma-steering` target.
- `pkg/orchestra` (P2.0) gains `SendMessage(ctx, workspaceDir, team, text) error`, `Interrupt(ctx, workspaceDir, team) error`, and `ActiveSessions(ctx, workspaceDir) ([]TeamSession, error)`. These are thin wrappers over the spawner helpers + state.json read.

**Prerequisite (lands as part of this chapter, before the steering CLI itself):**

- A small additive change to `internal/store.TeamState` (or a sibling helper) so that the cross-process state read is well-defined. Specifically: confirm `filestore.LoadRunState` works correctly when called from a process that did not call `Begin` (i.e., it does not require a held lock). If it does require a lock today, factor out a lock-free `ReadStateFile(workspaceDir)` helper. This should be a 30-line addition; if it grows beyond that, it warrants its own chapter.

### Out-of-scope

- **Agent → human direction** (`requires_action` translation, `user_tool_confirmation` reply). P1.9-B.
- **Tier gates** (config field that pauses orchestration between tiers). P1.9-C.
- **`orchestra sessions rm`** (terminate / archive a session early). P1.9-D.
- **Steering across runs.** The CLI targets the *active* run only. Historical sessions in `archive/` are not steerable (they're done).
- **Broadcast / fan-out send** (one message to all running teams). The dogfood apps haven't asked; if needed, easy to add later as `--team all`.
- **Streaming the agent's response** back to the CLI invocation. `orchestra msg` returns once MA accepts the event; the agent's response is logged to `.orchestra/logs/<team>.ndjson` like every other event. A "watch the response" command can come later if it earns its keep.
- **Authentication / authorization.** Whoever can read the workspace can steer. Workspace permissions are the access control surface; this chapter does not add a separate auth layer.
- **Local-backend steering.** The file message bus already exists for that. Adding parity here would mean wrapping the bus in CLI commands; out of scope, separable feature.

---

## 4. Data model / API

### 4.1 No new persisted types

P1.9-A reads from existing `store.TeamState` fields. It does not add new ones. The eventual MA echo of the inbound event is recorded as part of normal event-stream translation by the running `orchestra run` process.

### 4.2 Steering helpers in `internal/spawner/steering.go`

The original draft proposed a separate `internal/steering/` package. Adversarial review correctly pointed out that the steering helper needs `managedSessionEventsAPI` (unexported in `internal/spawner`) and the SDK adapter that constructs one. Either we export the interface (polluting the spawner's surface area) or we duplicate the adapter in the new package. Both are worse than keeping the helpers next to the existing event-construction code in `internal/spawner`.

```go
// In internal/spawner/steering.go

// SendUserMessage delivers a user.message event to sessionID. retryAttempts
// controls how many times the call retries on 429/5xx; 0 means at-most-once.
func SendUserMessage(ctx context.Context, sessions managedSessionEventsAPI, sessionID, text string, retryAttempts int) error

// SendUserInterrupt delivers a user.interrupt event to sessionID. Same
// retry semantics.
func SendUserInterrupt(ctx context.Context, sessions managedSessionEventsAPI, sessionID string, retryAttempts int) error

// SessionEventsClient constructs a managedSessionEventsAPI authenticated via
// the host API key (machost.NewClient). Used by CLI commands that need to
// send events from a process that didn't construct a Spawner.
func SessionEventsClient(ctx context.Context) (managedSessionEventsAPI, error)

// Sentinel errors. Stable across releases.
var (
    ErrNoActiveRun       = errors.New("no active orchestra run in workspace")
    ErrTeamNotFound      = errors.New("team not found in active run")
    ErrTeamNotRunning    = errors.New("team is not running")
    ErrLocalBackend      = errors.New("steering is only supported under backend: managed_agents")
    ErrNoSessionRecorded = errors.New("team has no recorded session id")
)
```

The CLI orchestrates the lookup itself (read state.json → get team's SessionID + Status → check status → call helper). No `Steerer` struct needed.

### 4.3 Lock-free state reader

```go
// In internal/store/filestore/ (or wherever LoadRunState lives)

// ReadActiveRunState reads state.json for the workspace without acquiring
// the run lock. The file is atomically written; the snapshot is consistent
// but may be stale. Returns ErrNoActiveRun if no active run exists.
//
// Use this from out-of-process callers (CLI commands run separately from
// the orchestrator). Callers needing a coherent multi-step view should
// continue to use SharedSnapshot.
func ReadActiveRunState(workspaceDir string) (*store.RunState, error)
```

If the existing `LoadRunState` is already lock-free against an atomically-written file, this helper is a one-line passthrough. The chapter pays for verifying that property; the helper makes the contract explicit.

### 4.4 `TeamSession` for `sessions ls`

```go
type TeamSession struct {
    Team         string
    Status       string
    Steerable    bool      // true iff Status == "running" && SessionID != ""
    SessionID    string
    AgentID      string
    LastEventID  string
    LastEventAt  time.Time
}
```

Lives in `internal/spawner` next to the helpers; aliased into `pkg/orchestra` after P2.0 lands.

### 4.5 New CLI commands

```
orchestra msg --team <name> --message <text> [--no-retry] [--workspace .orchestra]
orchestra interrupt --team <name> [--workspace .orchestra]   # always 0-retry
orchestra sessions ls [--all] [--workspace .orchestra]
```

Each is a thin cobra command in `cmd/`:

- `cmd/msg.go` — parses flags, validates `--team` and `--message` are non-empty, reads state via `ReadActiveRunState`, looks up `team.SessionID`, calls `spawner.SendUserMessage`, prints "ok" or the error.
- `cmd/interrupt.go` — same shape, no `--message` flag, calls `spawner.SendUserInterrupt` with `retryAttempts=0`.
- `cmd/sessions.go` — `ls` subcommand prints a tabular view via `cmd/format.go` helpers; default filters to `Steerable: true`, `--all` shows everything.

### 4.6 SDK surface (in `pkg/orchestra` after P2.0)

```go
// SendMessage delivers a user.message event to the named team in the active
// run at workspaceDir. Equivalent to `orchestra msg`.
func SendMessage(ctx context.Context, workspaceDir, team, text string) error

// Interrupt delivers a user.interrupt event. Equivalent to `orchestra interrupt`.
func Interrupt(ctx context.Context, workspaceDir, team string) error

// ActiveSessions returns the active run's per-team session info. Equivalent
// to `orchestra sessions ls`.
func ActiveSessions(ctx context.Context, workspaceDir string) ([]TeamSession, error)

type TeamSession = spawner.TeamSession  // alias from internal/spawner
```

These are thin wrappers around `internal/spawner` (where the steering helpers live per §5.7). Same alias-not-copy discipline as P2.0.

---

## 5. Engineering decisions

### 5.1 Cross-process: CLI talks to MA, not to the running orchestrator

Two options for delivering an inbound event:

(a) The CLI signals the running `orchestra run` process (Unix socket, named pipe, file watch on a queue directory), which then calls `Session.Send` from inside the same process that owns the store mutator funnel.

(b) The CLI talks directly to MA via its own SDK client, using the session ID it reads from `state.json`. The running process's event translator picks up MA's echo of the event and updates state.

Pick (b). Reasons:

1. **No new IPC surface.** Unix sockets / named pipes are platform-specific and add a server side to `orchestra run`. The MA API is the natural shared target.
2. **State stays single-writer.** The CLI never mutates `store.TeamState`. The running process's event translator is the only writer, just as today.
3. **Crash safety.** If `orchestra run` is mid-event-translation, an inbound MA send still gets queued by MA and observed by the next stream read. With option (a), if the CLI signals a dead `orchestra run`, the event is lost.
4. **Resumability.** P1.8 will need to resume runs across restarts. The CLI talking directly to MA means steering keeps working across resume — the new `orchestra run` process simply observes events sent during the gap. With option (a), steering during the gap fails.

The cost of (b) is one extra MA SDK client construction per CLI invocation (cheap; just a token). Worth it.

### 5.2 No store writes from the CLI

The temptation: write a "human steered" annotation to `state.json` so post-run analysis can see when interventions happened. Resist for P1.9-A. Reasons:

- It violates NF1 (single writer).
- MA's session event log already contains the user.message / user.interrupt events with timestamps; that's the source of truth.
- A post-run "annotation log" is a separate feature with its own design (where does it live? does it survive archive? does it count toward cost?). Don't bundle.

If post-run "show me the steering events" becomes a real ask, the cleanest implementation is a `cmd/runs show --events` extension that reads the per-team NDJSON log — no new persistence.

### 5.3 Active run = lock-free state read, not shared lock

The original draft proposed taking a shared run lock. Adversarial review pointed out (a) `gofrs/flock` shared-lock semantics on Windows are mandatory and untested in this repo, (b) the lock isn't actually needed since `state.json` is atomically written.

The corrected position: read `state.json` lock-free via a new `ReadActiveRunState(workspaceDir)` helper. The atomic-write pattern (write-temp-then-rename) guarantees a consistent snapshot; the data may be stale but is never torn. This avoids the shared-lock cross-process question entirely.

If there is no active run (no `state.json` exists), `ReadActiveRunState` returns `ErrNoActiveRun`. The CLI surfaces this as `"no active orchestra run in <workspace>"`.

### 5.4 Team-name targeting (not session-id)

The CLI takes `--team <name>`, not `--session <id>`. Reasons:

- Humans know team names from `orchestra.yaml`; session IDs are MA-internal.
- The CLI's lookup `state.Teams[team].SessionID` is one map access; trivial.
- Avoids the failure mode where a stale session ID (from a previous run) targets the wrong session under a re-used run.

`orchestra sessions ls` exposes the session ID for callers who really want it (debugging, support escalation), but `msg` / `interrupt` stay name-driven.

### 5.5 At-least-once with explicit `--no-retry` opt-out

The original draft claimed "at-most-once, no client-side retry." Adversarial review found that the underlying `Session.Send` path goes through `withRetry`, which retries 429/5xx — meaning a 5xx-then-success can deliver the event twice. The corrected position:

- `orchestra msg` defaults to **at-least-once with retry**. Duplicate delivery on transient errors is acceptable for messages (the agent might see "use the existing JSON store, not a database" twice — confusing but non-fatal).
- `orchestra msg --no-retry` and `orchestra interrupt` (always) use a **0-retry** path. Interrupts are the dangerous case — sending one twice could double-cancel a recovery cycle. Default to safety here.
- `--help` documents the at-least-once / at-most-once distinction explicitly.

The new helpers (`SendUserMessage` / `SendUserInterrupt` in `internal/spawner/steering.go`) take an explicit `retryAttempts` parameter so the CLI controls the retry budget per command. They do not call `Session.Send` (which uses the spawner's session-scoped retry config); they wrap `events.Send` directly with the chosen retry budget.

If MA returns a non-retryable error (e.g. session has already terminated), the CLI surfaces it directly. The user sees the error and decides what to do.

### 5.6 No streaming response

`orchestra msg` returns once MA accepts the event. Watching the agent's reaction means tailing `.orchestra/logs/<team>.ndjson` — a separate concern from "deliver the event." If a "follow mode" earns its keep later, it's an additive flag (`orchestra msg ... --follow`) that wraps the deliver + tail combo.

### 5.7 Helpers stay under `internal/spawner/`, no new package

The original draft proposed a new `internal/steering/` package on the theory that "steering is a CLI-and-SDK concern, not a spawner concern." Adversarial review showed the boundary doesn't pay for itself:

- The MA event-construction code (`toSessionEventSendParams`, the user.message Type-bug workaround) lives in `internal/spawner` and is exactly what steering needs.
- The `managedSessionEventsAPI` interface is unexported in `internal/spawner`. Putting steering elsewhere forces either exporting it (polluting the spawner's surface) or duplicating the SDK adapter.
- The CLI orchestrates state lookup itself; the helper just needs a session ID and a text — which is genuinely a spawner-level concern.

Decision: helpers live in `internal/spawner/steering.go`. The CLI wraps state-lookup + helper-call. No new package. If the helper grows into something with its own dependencies (state, logging policy, etc.), revisit the package split then.

### 5.8 Local backend deferred to P1.9-E with explicit acknowledgement

The local backend has a file message bus that already supports cross-team coordination, but it lacks a first-class **human → team** surface. A user steering a local-backend run today would hand-write to `.orchestra/messages/<team>/inbox/`. P1.9-A could ship local parity by extending the CLI commands to dispatch to the message bus, sharing ~95% of the cobra plumbing.

This chapter declines the work as a deliberate scope cut. The decision and its cost (a future chapter to add local steering, with a follow-up CLI semantic that mirrors this one) are tracked as **P1.9-E**. Reasoning: shipping the MA half cleanly in one PR is more valuable than shipping a backend-spanning surface that doubles the cobra-test matrix.

### 5.9 No control-socket / IPC alternative

The trade-off table considered Unix socket / named pipe / file-queue alternatives. A reviewer raised "have the orchestrator expose a control socket; the CLI is a thin client" — a real option that solves the IPC problem without each CLI invocation reaching MA directly. We dismiss it on:

- **Cross-platform parity**: Unix sockets and Windows named pipes have different semantics; the orchestrator gains a platform-shaped surface.
- **Operational complexity**: control sockets need lifecycle management (created on `Begin`, removed on `End`, garbage-collected on crash).
- **Resume**: P1.8 will resume runs across orchestrator restarts; control sockets don't survive restart while MA sessions do.

The MA-direct choice wins on all three, but the alternative is a real one and the doc owns that.

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| CLI talks directly to MA | (a) Unix socket / named pipe to `orchestra run` (b) File-queue picked up by orchestrator (c) Control-socket exposed by orchestrator | No new IPC surface; resilient across crash and resume; preserves single-writer store invariant; Windows / Unix parity (sockets and pipes differ); see §5.9 for full dismissal of (c) |
| Lock-free state read | Acquire shared run lock before reading | Cross-process shared-lock semantics on Windows are untested in this repo; the atomic-write pattern already gives consistent snapshots; one fewer load-bearing assumption |
| No store writes from CLI | Annotate state.json with steering events | Violates single-writer; MA's event log is the source of truth; "show me steering events" is a separate, additive feature |
| Team-name targeting | Session-ID targeting | Humans know names; session IDs are internal; avoids stale-ID failure mode |
| At-least-once with `--no-retry` opt-out | At-most-once with no client retry; or full at-most-once via idempotency keys | The existing retry path already retries 5xx; pretending otherwise is fictional. Idempotency keys would require SDK support that isn't there. Document the actual semantics; default `interrupt` to 0-retry where duplicate delivery is dangerous. |
| Translator extension in this chapter | Treat user.message echoes as UnknownEvent and log only | Without translation, F3's `last_event_at` column is stale and the run log gives no visible cause for behavior changes; observability tanks |
| Helpers in `internal/spawner/steering.go` | New `internal/steering/` package | The needed `managedSessionEventsAPI` is unexported in spawner; a new package would either pollute the spawner surface or duplicate the SDK adapter |
| `sessions ls` filters to steerable by default | List everything, let humans figure out which are steerable | Listing post-terminal sessions next to `msg --team X` advertises false steerability; the `--all` flag preserves the audit view |
| Live-MA fixture tests delivery, not compliance | Test that the agent obeys the steering | Model-variance noise; flaky-by-design; burns budget on retries; tells you nothing about orchestra correctness |
| Synchronous return | Streaming agent response via `--follow` | Additive flag possible later; deliver and observe are separable |
| Outbound only this chapter | Bundle agent → human asks (P1.9-B) | P1.9-B requires event-translator changes and a new "waiting" state; strictly bigger; outbound has standalone value |
| MA-only steering this chapter | Local-backend parity via the file bus | Tracked as P1.9-E; ship MA cleanly first; doubling the cobra-test matrix here is not worth the chapter scope creep |
| `orchestra sessions ls` here | Defer to P1.9-D with `sessions rm` | `ls` is a free observability win and shares the lookup code with `msg`/`interrupt`; `rm` is destructive and warrants its own design |

---

## 7. Testing

### 7.0 Prerequisite: cross-process state-read test

Before writing any steering code, land a small targeted test that exercises the lock-free state read from a second process:

- Spawn two `os/exec` invocations of the test binary against the same workspace.
- Process A: `orchestra run` against a long-running mock-claude script (or a hold-the-lock mode just for the test).
- Process B: read `state.json` via `ReadActiveRunState` while A holds the run lock; assert it returns the expected snapshot without blocking.
- Repeat under Windows (filestore uses `gofrs/flock` which has different semantics on `LockFileEx`).

If this test fails, the design's lock-free read assumption is wrong and we re-evaluate before any steering code lands.

### 7.1 Unit tests in `internal/spawner/`

- `SendUserMessage` happy path: fake `managedSessionEventsAPI` records the params; assert the `BetaSessionEventSendParams` payload has `events[0].type == "user.message"` and the right text.
- `SendUserMessage` retry behavior: with `retryAttempts=3`, fake returns 5xx twice then success; assert exactly 3 SDK calls. With `retryAttempts=0`, fake returns 5xx; assert exactly 1 SDK call and the error surfaces.
- `SendUserInterrupt` happy path + 0-retry: fake captures `events[0].type == "user.interrupt"`; with `retryAttempts=0` the 5xx error surfaces immediately.
- Translator tests: feed `{"type":"user.message","content":[{"type":"text","text":"hi"}]}` and `{"type":"user.interrupt"}` events through `translateMAEvent`; assert the new event types are produced.
- `Session.apply` tests: the new echo events bump `LastEventID` / `LastEventAt` but do not change team `Status`.

### 7.2 CLI integration tests

`cmd/msg_test.go`, `cmd/interrupt_test.go`, `cmd/sessions_test.go`:

- Build the orchestra binary (reuse `e2e_test.go` helpers).
- Seed a `.orchestra/state.json` via the store (memstore + manual save), simulating an active run with two teams.
- Invoke the CLI binary with the right flags against a stub MA endpoint (httptest.Server); assert exit code, stdout, and the MA payload received.
- Cover: happy path, missing team, wrong status (rejected by F6), no active run, local backend (rejected by F4), `--no-retry`, `sessions ls` filtering (default vs `--all`).

### 7.3 Live MA fixture — tests delivery, not model compliance

`test/integration/ma_steering/`:

- Single-team config with a deterministic, slow-by-design task (e.g., "introduce yourself in three paragraphs, one per minute"). The point is to keep the session `running` long enough to steer; not to test whether the model obeys.
- Helper script: `orchestra run` in the background, wait for `state.json` to show team `running`, then `orchestra msg --team intro --message "<sentinel-string>"`, wait for run to complete.
- Asserts (deterministic):
  - The CLI command exits 0.
  - `.orchestra/logs/intro.ndjson` contains an event with `type: user.message` and the exact `<sentinel-string>` text.
  - `Beta.Sessions.Events.List` for the session contains the same event (read post-run via the SDK; proves the event reached MA, not just the local log).
  - `state.Teams["intro"].LastEventID` advanced past the steering event ID.
- Does NOT assert the final summary reflects the steering — that's model variance.
- Opt-in via `ORCHESTRA_MA_INTEGRATION=1`. Hooked into `make e2e-ma-steering` and `make e2e-ma`.
- Cost estimate: ~$0.05–0.10 per run.

### 7.4 What we explicitly do not test

- Concurrent `orchestra msg` calls against the same team. MA is the serialization point; the CLI doesn't lock. If two humans steer at once, MA orders the events as it receives them.
- Whether the agent obeys the steered message. That's a model-behavior characterization, not an orchestra correctness check.
- CLI behavior during an `orchestra run` crash. Out of scope (P1.8 territory).
- Local-backend steering. The local CLI commands return the documented error; nothing further to test.

---

## 8. Rollout

Single PR, ~600-900 LOC plus tests:

1. New steering helpers in `internal/spawner/steering.go` and `internal/spawner/steering_test.go` (next to existing `toSessionEventSendParams`; `managedSessionEventsAPI` stays unexported and undeduplicated, per §5.7).
2. MA event translator extension for `user.message` / `user.interrupt` echoes (§F8).
3. Lock-free state reader (e.g. `store.ReadActiveRunState` or filestore equivalent — see §4.3).
4. New cobra commands: `cmd/msg.go`, `cmd/interrupt.go`, `cmd/sessions.go` (with `ls` subcommand).
5. SDK wrappers in `pkg/orchestra/` (assumes P2.0 has landed; if not, this chapter ships the wrappers without P2.0 — the SDK additions just live under `internal/` until P2.0 promotes them).
6. README "Steering a run" section under Backends.
7. TESTING.md row for `e2e-ma-steering`.
8. Live MA fixture under `test/integration/ma_steering/`.

**Rollback.** Pure revert. No schema changes, no migration, no behavioral change for existing `orchestra run` flows.

**Sequencing relative to P2.0 and P1.5.** Order doesn't matter at the file level — `cmd/msg.go` doesn't touch anything P1.5 or P2.0 modify. If P2.0 lands first, the SDK wrappers slot into `pkg/orchestra` directly. If P1.9-A lands first, the wrappers wait or land alongside P2.0.

---

## 9. Observability & error handling

- All three CLI commands print a one-line "ok" or a non-zero exit with a human-readable error. No JSON output by default; if dogfood apps want structured output, add `--json` later.
- DEBUG-level slog inside `internal/spawner/steering.go`: log the team, session ID, retry budget, and event type before the MA call; log the API result or wrapped error after. Surfaceable via a future `--verbose` CLI flag.
- The running orchestrator emits `[team] human: <truncated text>` lines via `r.reportMAEvent` whenever MA echoes a `user.message` event back through the stream — coordinators tailing `orchestra run` output see steering activity in real time. `[team] human: <interrupt>` for interrupts.
- Errors are wrapped with `fmt.Errorf("%w", err)` so callers can `errors.Is(err, spawner.ErrTeamNotFound)`. SDK callers see the same sentinels via `pkg/orchestra` aliases (P2.0).
- The `state.json` snapshot read is lock-free (§NF2). If `state.json` is missing or unreadable, `ReadActiveRunState` returns the underlying `os.PathError` wrapped as `ErrNoActiveRun` — the user-facing message is `"no active orchestra run in <workspace>"`.

---

## 10. Open questions

Genuinely open — not yet decided:

1. **Cross-process shared-lock behavior on Windows.** §NF2 declines to depend on it; the prerequisite test (§7.0) confirms a lock-free read works correctly. If the test surfaces problems even with the lock-free approach (e.g., `state.json` not actually atomically written on Windows), the design has to absorb a Windows-specific path. Lean: the existing atomic-write pattern (`fsutil`) handles Windows correctly, but we verify, not assume.
2. **Concurrent CLI calls and MA event ordering.** If two humans (or one human + a dogfood app) send messages near-simultaneously, MA orders them in arrival order. Document this in `orchestra msg --help` so users don't expect transactional ordering. No code change.
3. **`--team all` flag.** Easy to add (loop over teams in `running` status). Defer until a dogfood app asks; PR audit doesn't need it.
4. **Race between team transitioning and `msg` arriving.** If a team is `running` at lookup time but reaches `idle (end_turn)` between lookup and MA send, the message arrives at a session that has just finished its turn. MA's behavior here needs verification: queue for next turn, reject, or silently accept? Document during implementation based on observed behavior.
5. **Permission story.** Anyone with workspace read access can read `state.json`; anyone with `ANTHROPIC_API_KEY` can talk to MA. So steering authority equals "has the workspace AND has the key." Fine for v1. Role-based access (e.g. "ops can interrupt, only owner can msg") is an external concern if it ever materializes.
6. **`sessions ls --all` for archived runs.** Should `--all` also enumerate `archive/` directories so users can inspect historical sessions? Useful for support / debugging. Lean: yes — separate CLI flag (`--archived`). Doesn't change steering semantics.

Decided in this chapter (no longer open):

- **No active run sentinel.** `internal/spawner.ErrNoActiveRun` is exported; the lock-free `ReadActiveRunState` returns it cleanly. Documented in F5.
- **SDK error aliasing.** `pkg/orchestra` aliases the `internal/spawner.Err*` sentinels. Same alias pattern P2.0 uses for types.
- **No `--reason` flag for `interrupt`.** MA's `user.interrupt` has no body; a CLI-side reason is log-only and the timestamp suffices. Don't ship.

---

## 11. Acceptance criteria

- [ ] Cross-process state-read prerequisite test (§7.0) lands and passes on Linux/macOS/Windows.
- [ ] `internal/spawner/steering.go` exists with `SendUserMessage`, `SendUserInterrupt`, `SessionEventsClient`, `TeamSession`, error sentinels (`ErrNoActiveRun`, `ErrTeamNotFound`, `ErrTeamNotRunning`, `ErrLocalBackend`, `ErrNoSessionRecorded`).
- [ ] MA event translator recognizes `user.message` / `user.interrupt`; new `UserMessageEchoEvent` / `UserInterruptEchoEvent` types exist; `Session.apply` updates `LastEventID`/`LastEventAt` on echo; `cmd/run_ma.go:reportMAEvent` logs `[team] human: <text>` lines.
- [ ] `orchestra msg --team <name> --message <text>` delivers a `user.message` event to the named team's MA session.
- [ ] `orchestra msg --no-retry` and `orchestra interrupt --team <name>` use the 0-retry path.
- [ ] `orchestra sessions ls` defaults to running/pending teams with `Steerable: yes`; `--all` includes terminal teams with `Steerable: no`.
- [ ] `backend: local` runs return the documented "steering is only supported under backend: managed_agents (local-backend steering tracked as P1.9-E)" error from all three commands.
- [ ] `pkg/orchestra` (P2.0) aliases the steering sentinels and exposes `SendMessage` / `Interrupt` / `ActiveSessions` wrappers.
- [ ] `make test && make vet && make lint` green.
- [ ] `make e2e-ma-steering` (new target) runs the delivery-only live fixture; passes against real MA when `ORCHESTRA_MA_INTEGRATION=1`.
- [ ] README "Steering a run" section exists under Backends.
- [ ] TESTING.md updated with the new make target and a one-line cost note.
- [ ] Follow-ups tracked (not shipped): P1.9-B (agent → human asks via `requires_action`), P1.9-C (tier gates), P1.9-D (`sessions rm`), P1.9-E (local-backend steering parity via the file message bus).
