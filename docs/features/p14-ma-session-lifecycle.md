# Feature: P1.4 — MA session lifecycle (StartSession + Events + Send + watchdog)

Status: **Proposed**
Owner: @itsHabib
Depends on: [00-store-interface.md](./00-store-interface.md) (shipped), [p13-registry-cache.md](./p13-registry-cache.md), P1.2 spawner scaffolding (existing in `pkg/spawner/`).
Relates to: [DESIGN-v2.md](../DESIGN-v2.md) §6 (architecture), §8 (Spawner interface), §9.3 (`StartSession`), §9.4 (`Session.Events`), §9 intro (watchdog), §10.2 (`state.json` schema), §13 phase P1.4, §16 (event mapping).
Target: single-team end-to-end MA run producing a text-only deliverable. No repo resources, no resume, no steering.

---

## 1. Overview

P1.3 delivers stable agent/env IDs in the cache. P1.5 delivers repo-backed artifacts. P1.4 is the thinnest possible slice between them — the phase where an orchestra run first touches `Beta.Sessions.New`, `Beta.Sessions.Events.StreamEvents`, and `Beta.Sessions.Send`. One team. One MA session. Text-only output (a summary.md written from the agent's final message). No GitHub repo mounted, no resume path, no human steering, no tool-confirmation loop.

The point of shipping P1.4 narrow is to prove the session lifecycle plumbing before layering any of the harder chapters on top. If stream-first ordering, event-to-state translation, and the watchdog all work against a trivial text task, P1.5 can focus on `github_repository` resources and branch resolution without reopening the basics.

Concretely, P1.4 lands:

1. **`ManagedAgentsSpawner.StartSession`** — wraps `Beta.Sessions.New` using the agent/env handles P1.3 caches produce.
2. **Stream-first engine helper** — opens `Beta.Sessions.Events.StreamEvents` *before* sending the initial `user.message`, because MA only delivers events emitted after the stream is open.
3. **Event translator** — maps MA events to the orchestra event union per DESIGN-v2 §16 and updates `state.json` via `UpdateTeamState`.
4. **Watchdog** — one nudge on silence, then `stalled` if silence persists. No cancellation; resume is P1.8's job.
5. **Terminal handling** — on `session.status_idle + end_turn`, capture the final `agent.message` text and write it atomically to `.orchestra/results/<team>/summary.md`. Mark the team `done`.

---

## 2. Requirements

**Functional.**

- F1. `ManagedAgentsSpawner.StartSession(ctx, req) (Session, error)` creates an MA session via `Beta.Sessions.New` using handles from P1.3. No prompt sent yet.
- F2. `Session.Events(ctx) (<-chan Event, error)` opens `Beta.Sessions.Events.StreamEvents`, runs the translator goroutine, and emits translated orchestra events on the returned channel. Callable exactly once per session.
- F3. `Session.Send(ctx, UserEvent) error` delivers a `user.message` or `user.interrupt` to MA.
- F4. The engine opens the event stream *before* calling `Send` with the initial prompt. Ordering is load-bearing — MA does not replay events emitted before the stream is open.
- F5. Every event observed on the stream is appended to `.orchestra/logs/<team>.ndjson` as raw MA JSON by the engine's single per-team log writer. State-affecting events (per §16) additionally call `UpdateTeamState` with the appropriate mutator.
- F6. On `session.status_idle + stop_reason: end_turn`, the translator writes the text content of the most recent `agent.message` to `.orchestra/results/<team>/summary.md` atomically and transitions the team to `done`.
- F7. Per-team watchdog: if no event arrives for `defaults.timeout_minutes` while `team.status == "running"`, send exactly one `user.interrupt` + `user.message` nudge. If silence persists for another `timeout_minutes` past the nudge, transition team to `stalled`, leave the MA session alive, return.
- F8. All MA API calls flow through the retry layer established in P1.2 (429/5xx exponential backoff + `Retry-After`, non-retryable 4xx fail fast).

**Non-functional.**

- NF1. **Stream-first invariant.** No code path may `Send` before `Events` has returned. Enforced by engine code structure; the `Session` type does not expose a sugar helper that hides the ordering.
- NF2. **Single writer invariants.** One writer for `state.json` (`UpdateTeamState` mutator funnel, DESIGN-v2 §10.2). One writer per `.orchestra/logs/<team>.ndjson` (the translator goroutine). The translator processes events sequentially — log append, then state update, then read the next event.
- NF3. **Transport recovery without duplicates.** On stream transport error, reopen the stream and use event IDs to dedupe per DESIGN-v2 §9.4. No persisted event appears twice in the NDJSON log or re-triggers a state transition.
- NF4. **Watchdog bounded.** At most one nudge per team per run. After the nudge the watchdog either stalls the team or exits cleanly when the team reaches a terminal state.
- NF5. **Rollback-clean.** One PR (or a tight series). `git revert` restores "P1.3 without sessions" without disturbing local-backend code.

---

## 3. Scope

### In-scope

- `pkg/spawner/managedagents/` — `session.go` (Session struct + StartSession), `translator.go` (MA event → orchestra event + state mutations), `watchdog.go`.
- Engine wiring: the `startTeamMA` helper that enforces stream-first ordering and hands off to the per-team event loop.
- State transitions: `running`, `idle`, `done`, `failed`, `stalled`, `terminated` — driven exclusively by events (§16 + §5.5).
- Result-summary capture: final `agent.message` text → `.orchestra/results/<team>/summary.md`.
- Token/cost counter accumulation from `span.model_request_end`.
- Unit tests for translator + watchdog + stream-first ordering.
- Opt-in integration fixture exercising a single-team end-to-end MA run.

### Out-of-scope

- **GitHub repo resources (`github_repository`).** P1.5. P1.4's fixture is text-only.
- **`orchestra resume`.** P1.8. Sessions orphaned by P1.4 interruption must be rerun.
- **`orchestra msg` / `interrupt` CLI steering.** P1.9. `Session.Send` is engine-internal in P1.4 (used by engine and watchdog only).
- **Tool confirmation loop (`stop_reason: requires_action`).** Mapped to `failed` with a descriptive `last_error`. v1 default is `always_allow` on orchestra-managed agents (DESIGN-v2 §9.5), so this path is only reached on misconfiguration.
- **Multiagent / thread events.** Per §16 those are persisted to NDJSON but otherwise ignored.
- **Local-backend changes.** P1.4 touches only the MA spawner implementation.
- **Members / coordinator under MA.** Already validation-warned in P1.0 and ignored (DESIGN-v2 §9.7).
- **PR creation / artifact branch resolution.** No `repository_artifacts[]` population in P1.4 — that lives with the repo-backed flow in P1.5.

---

## 4. Data model / API

No new persisted shape. `state.json` already has every field the translator writes (see DESIGN-v2 §10.2 and `pkg/store.TeamState` shipped in PR #2). This section pins the translator contract and the Session surface.

### 4.1 `Session` concrete type

```go
package managedagents

// Session is the per-team handle to an MA session. Owned by the engine's
// per-team goroutine for its lifetime; not safe for concurrent use across
// goroutines.
type Session struct {
    id       string
    client   anthropic.Client     // pinned SDK version, see DESIGN-v2 §9
    teamName string
    log      io.Writer            // engine's per-team NDJSON writer
    store    store.Store          // for UpdateTeamState
    clock    func() time.Time
}

// StartSession wraps Beta.Sessions.New. No prompt is sent here; the engine
// opens the event stream via Events and then calls Send.
func (m *ManagedAgentsSpawner) StartSession(ctx context.Context, req spawner.StartSessionRequest) (*Session, error)

// Events opens Beta.Sessions.Events.StreamEvents, starts the translator
// goroutine, and returns a channel of translated orchestra events. Call
// exactly once per session. The channel closes on terminal session state
// (idle+end_turn, terminated, unrecoverable error) or when ctx is done.
func (s *Session) Events(ctx context.Context) (<-chan Event, error)

// Send delivers a user event (message or interrupt) to MA. Used by the
// engine for the initial prompt and by the watchdog for nudges.
func (s *Session) Send(ctx context.Context, ev UserEvent) error

// Cancel closes the local stream reader. Does not archive the MA session
// (resumable later by P1.8). Safe to call multiple times.
func (s *Session) Cancel() error
```

### 4.2 Event translation

The full MA-event → orchestra-event mapping lives in DESIGN-v2 §16. P1.4 implements every row; `translator.go` has a comment at the switch statement pointing at §16 as the canonical spec.

**State-affecting events** — the subset that call `UpdateTeamState`:

| MA event | State mutation |
|---|---|
| `session.status_running` | `team.status = "running"`; stamp `started_at` if zero |
| `session.status_idle + end_turn` | `team.status = "done"`; `team.result_summary = <last agent.message text>`; stamp `ended_at`; write `summary.md` |
| `session.status_idle + requires_action` | `team.status = "failed"`; `team.last_error = "tool confirmation requested; not supported in v1"` |
| `session.status_idle + max_turns` | `team.status = "stalled"`; `team.last_error = "max turns reached"` |
| `session.status_idle + error` | `team.status = "failed"`; `team.last_error = <event payload>` |
| `session.status_terminated` | `team.status = "terminated"`; finalize counters |
| `session.error` | `team.last_error = <message>` (status unchanged; next idle/terminated decides) |
| `span.model_request_end` | accumulate `input_tokens`, `output_tokens`, `cache_{read,creation}_input_tokens`, `cost_usd` |
| `agent.*`, `session.status_rescheduled` | NDJSON log only; no state change |

Every event (state-affecting or not) is NDJSON-logged first. The translator does not fork per-event goroutines.

### 4.3 Watchdog

```go
// Watchdog observes event timestamps via a lastEventAt updated by the
// translator. Lifecycle:
//
//   loop:
//     sleep until now - lastEventAt >= timeout_minutes
//     if team.status != "running":        // finished while we slept
//       return
//     if !nudged:
//       Send(user.interrupt); Send(user.message("status check..."))
//       nudged = true
//       continue loop              // next breach takes another timeout
//     UpdateTeamState: status="stalled",
//                     last_error="no events for 2× timeout_minutes"
//     return
//
// At most one nudge per team per run.
```

The watchdog never calls `Sessions.Cancel` or archives the session — stalled sessions are deliberately left resumable.

---

## 5. Engineering decisions

### 5.1 Stream-first ordering is an engine concern, not a `Session` concern

Tempting refactor: make `StartSession` return an already-streaming session so callers can't get the ordering wrong. Rejected because `ResumeSession` (P1.8) wants different ordering — create-or-get, replay history, then stream. Coupling stream lifecycle to session creation forces one of the two code paths into awkward contortions.

Instead, the engine owns a helper:

```go
func (r *Run) startTeamMA(ctx context.Context, team *config.Team) (*Session, <-chan Event, error) {
    s, err := r.spawner.StartSession(ctx, r.reqFor(team))
    if err != nil {
        return nil, nil, err
    }
    events, err := s.Events(ctx)
    if err != nil {
        _ = s.Cancel()
        return nil, nil, err
    }
    if err := s.Send(ctx, initialPromptFor(team, r)); err != nil {
        _ = s.Cancel()
        return nil, nil, err
    }
    return s, events, nil
}
```

Any callsite that has a `Session` but not a `<-chan Event` is a bug. Review rule, not a type-level enforcement.

### 5.2 Single translator goroutine per team

The translator is strictly sequential: read event → append to NDJSON → dispatch state mutation (if any) → read next event. No goroutine-per-event, no buffered fan-out.

Reasoning: NDJSON order and state.json consistency are both expected to match event arrival order. Parallel dispatch introduces interleaving that makes the post-mortem log order diverge from the state transition order — debugging becomes worse. The translator is not the throughput bottleneck; MA's event rate is low.

### 5.3 Result-summary capture — last `agent.message` adjacent to `end_turn`

MA emits multiple `agent.message` events during a run (after each tool turn). The one that matters for `summary.md` is the final one — the message delivered immediately before the session goes idle with `end_turn`. The translator tracks the most recent `agent.message` text in memory and writes it to `summary.md` when `end_turn` arrives.

Alternatives considered:

- **Accumulate all `agent.message`s.** Includes mid-turn chatter; requires the user to know which section is the "answer." Worse UX.
- **Parse a marker out of the agent's prose.** Brittle. Agents do not reliably follow formatting instructions.
- **Let the agent write the summary to disk itself.** Requires `github_repository` or Files API — not available without repo (P1.5) and the Files API doesn't see container writes per SPIKE-ma-io-findings.md.

Intermediate messages remain in the NDJSON log for full-fidelity replay if a user ever wants them.

### 5.4 Watchdog: one nudge then stall; never cancel

Canceling a stalled session destroys the state P1.8 needs to resume it. Nudging forever wedges the team with prompt noise and can confuse the agent. One nudge + stall strikes a balance: the nudge handles the "agent is waiting on implicit input" common case; the stall bounds the damage when the agent is truly stuck.

Watchdog lives in the engine, not in the spawner package. Rationale: it calls `UpdateTeamState` and composes `Session.Send`; keeping it engine-side means the spawner package does not take a dependency on state semantics it otherwise doesn't touch.

### 5.5 Failure classification

Map of "something went wrong" to team state. `last_error` strings are informational — no programmatic parsing.

| Condition | `team.status` | `team.last_error` |
|---|---|---|
| `Beta.Sessions.New` returns non-retryable 4xx | `failed` | `start_session: <status> <body>` |
| Retries exhausted on transport/5xx | `failed` | `start_session: exhausted retries: <cause>` |
| `Events` fails to open | `failed` | `events: <cause>` |
| `Send(initial)` fails | `failed` | `send_initial: <cause>` |
| `session.status_idle + error` | `failed` | `session error: <event payload>` |
| `session.status_idle + requires_action` | `failed` | `tool confirmation requested; not supported in v1` |
| `session.status_idle + max_turns` | `stalled` | `max turns reached` |
| Watchdog: 2× timeout silence | `stalled` | `no events for 2× timeout_minutes` |
| `session.status_terminated` | `terminated` | (payload, if any) |
| Successful completion | `done` | — |

### 5.6 Raw MA JSON in NDJSON log, not translated events

Logs hold the raw `Beta.Sessions.Events` payloads. Translated orchestra events are a projection — recoverable from raw by rerunning the translator. Logging the projection instead throws away information (original field names, untranslated enum values, event IDs MA uses for dedupe on resume).

Cost: the NDJSON schema is coupled to the MA event schema. That's fine — the schema is already coupled at the translator, and logs are not a stable API we promise to external consumers.

### 5.7 Relationship to `run.Service` and `internal/workspace`

P1.4 writes state via `Workspace.UpdateTeamState` and logs via `Workspace.LogWriter` as they exist today. When [01-service-layer.md](./01-service-layer.md) lands, those calls move to `run.Service.Record*` and `run.Service.LogWriter` (or a sibling), identical semantics.

If 01-service-layer.md lands first, P1.4 is written directly against `run.Service`. If P1.4 lands first, the service-layer PR migrates P1.4's callsites as part of its mechanical diff. Either order is cheap.

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Text-only deliverable for the fixture | (a) Include repo checkout in P1.4 fixture. (b) Synthetic "write no files" task. | (a) conflates P1.4 and P1.5 — if the run fails, we don't know whether it's sessions or repo integration. (b) is not realistic per DESIGN-v2 §13 P1.4: "not a synthetic write-no-files task." Text-only summary is the simplest real task. |
| One nudge + stall, never cancel | (a) Cancel on first breach. (b) Nudge-forever. (c) Configurable policy. | (a) loses resumability. (b) risks prompt-spam and agent confusion. (c) is premature — we have no data yet on what policies users want. Start simple, add knobs when they earn their place. |
| Stream-first ordering enforced by engine helper, not `Session` API | (a) `StartSession` auto-streams. (b) `Session.Start` combines creation + stream + initial send. | (a) and (b) couple lifecycle to creation and force `ResumeSession` (P1.8) into a different shape. Engine helper keeps `Session` lifecycle-agnostic. |
| Summary = last `agent.message` before `end_turn` | (a) Accumulate all agent messages. (b) Agent writes summary to a file. | (a) is noisy. (b) requires repo/Files API we don't have yet. Final-message capture is the cheapest correct answer. |
| Fail on `requires_action` | (a) Implement tool-confirmation CLI in P1.4. (b) Auto-allow all. | (a) is a whole feature (CLI + MCP toolset handling). (b) overrides MA's `always_ask` globally. Failing fast is honest and non-destructive. |
| Raw MA JSON in NDJSON log | (a) Translated orchestra events in log. (b) Both. | Raw is reprocessable and doesn't lose information. Translated is recoverable from raw. Both is duplicated storage for no upside. |
| Watchdog in engine, not spawner | (a) Watchdog inside `Session`. | Watchdog calls `UpdateTeamState` and composes `Send`; putting it in the spawner leaks state semantics into the persistence backend abstraction. |

---

## 7. Testing

### 7.1 Translator unit tests

`pkg/spawner/managedagents/translator_test.go` with a fake event feeder:

- One test case per row of DESIGN-v2 §16. Feed the MA event, assert the orchestra event emitted and any state mutation recorded against a `memstore.MemStore`.
- `session.status_idle + end_turn` with a preceding `agent.message`: verifies summary text is captured and `summary.md` is written (mocked file sink).
- `span.model_request_end` sequence: counters accumulate across multiple events, not overwrite.
- Unknown event type: logs at warn and continues without state change or panic.

### 7.2 Watchdog unit tests

`watchdog_test.go` with a synthetic clock:

- No breach → no nudge, no state change.
- Exactly one breach: one `Send(user.interrupt)` + one `Send(user.message)`, `nudged = true`, no state change.
- Two breaches: state transitions to `stalled` with `last_error == "no events for 2× timeout_minutes"`. No third Send.
- Events arriving just before the deadline reset the timer correctly.

### 7.3 Stream-first ordering enforcement

Test for the engine helper (`startTeamMA`) using a fake spawner that records the order of `StartSession`, `Events`, `Send`. Must always be 1 → 2 → 3.

### 7.4 Integration fixture (opt-in)

`test/integration/ma_single_team/` — a minimal `examples/`-style project with one team whose prompt asks for a short markdown summary of a fixed input. Requires `ORCHESTRA_MA_INTEGRATION=1` and a real API key in `ANTHROPIC_API_KEY`. Assertions:

- Process exits 0.
- `.orchestra/state.json` has `team.status == "done"`, non-zero token counters, populated `result_summary`.
- `.orchestra/results/team-a/summary.md` exists and is non-empty.
- `.orchestra/logs/team-a.ndjson` contains `session.status_running`, at least one `agent.message`, and `session.status_idle` with `stop_reason: end_turn`.

CI runs unit tests by default. The integration fixture runs only when `ORCHESTRA_MA_INTEGRATION=1` is set (a separate GitHub Actions workflow with a secret-backed key, triggered manually or on release).

### 7.5 Failure-mode tests

Against a fake MA client:

- `StartSession` → non-retryable 4xx: team marked `failed` with the expected prefix.
- `StartSession` → 500 then 200: retry layer runs, team completes.
- Stream transport error then reopen: events replayed up to the seen-set cutoff, no NDJSON duplicates, no repeated state mutations.

---

## 8. Rollout / Migration

Single PR, ~600-900 LOC. MA-backend had zero callers before P1.4 (opt-in via `backend.kind: managed_agents` in config), so no migration of existing users.

Internal PR order:

1. **`pkg/spawner/managedagents/session.go`** — Session struct, StartSession, Events (skeleton that reads from `StreamEvents` and forwards raw events), Send, Cancel.
2. **`translator.go`** — MA event → orchestra event mapping per §16; `UpdateTeamState` dispatch. Unit tests alongside.
3. **`watchdog.go`** — watchdog goroutine + synthetic-clock tests.
4. **Engine wiring** — `startTeamMA` helper and the MA-backend branch of `runTeam` / `runTier` in `cmd/run.go` (or `run.Service.StartTeam` if 01-service-layer.md landed first).
5. **Integration fixture** — `test/integration/ma_single_team/` + opt-in build tag + README.
6. **Docs** — tick DESIGN-v2 §13 P1.4 and §17 review checklist items; update AGENTS.md/CLAUDE.md project-structure section.

**Rollback.** `git revert`. MA-backend code is self-contained in its own packages; local backend untouched.

**Dependencies.** Merges after P1.3 (needs `EnsureAgent` / `EnsureEnvironment` returning valid handles). Independent of 01-service-layer.md — whichever lands second migrates to the other.

---

## 9. Observability & error handling

- NDJSON log append is the first step for every received event. If anything downstream panics, the log has the last event for post-mortem.
- State transitions emit `slog.Info` with `team`, `from`, `to`, `session_id`, `event_id`. Post-mortem friendly without opening the NDJSON.
- Watchdog: `slog.Warn` on first breach (nudge sent); `slog.Error` on stall.
- Retry layer: attempts at `slog.Debug` (op, attempt, wait); final failure at `slog.Error`.
- No Prometheus/OTel in P1.4. Cache-hit rate, session-create latency, translator throughput — all earn their place when orchestra-in-prod asks for them, not now.
- Translator panics on a malformed event: caught by the goroutine, `slog.Error` with the raw event JSON and the `session_id`, skip the event, continue. One bad event should not take down the run.

---

## 10. Open questions

1. **Persist `last_event_id` / `last_event_at` in `state.json` during P1.4?** DESIGN-v2 §10.2 schema has the fields; P1.8 (resume) is where they get consumed. Option: populate them in P1.4 even with no reader, so the schema is stable when P1.8 arrives. Lean: populate now.
2. **Default `timeout_minutes` for MA sessions.** v1 inherited 30. MA sessions doing heavy refactor work may legitimately take longer; nudging too early produces noise. Defer tuning until the integration fixture has produced empirical timing data.
3. **Where does `startTeamMA` live?** If 01-service-layer.md lands first: `run.Service`. Otherwise: a private method on `orchestrationRun` in `cmd/run.go`. Non-blocking.
4. **CLI surfacing of "v1 does not support tool confirmation".** Error message is descriptive but terminal. Future work (P1.9 or later) may add an `orchestra confirm <team>` CLI. Out of scope here.
5. **Translator behavior on unknown MA event types.** Current plan: warn-and-skip. Alternative: surface as `failed` so the user sees it. Skip is more forgiving and matches v1's "don't break the run on a single weird event" instinct.

---

## 11. Acceptance criteria

- [ ] `pkg/spawner/managedagents/` contains `session.go`, `translator.go`, `watchdog.go` with accompanying `_test.go` files.
- [ ] Every row of DESIGN-v2 §16 has a corresponding translator unit-test case.
- [ ] Watchdog unit tests cover no-breach, one-breach (nudge), and two-breach (stall) paths.
- [ ] Stream-first ordering test passes for `startTeamMA`.
- [ ] Opt-in integration fixture (`test/integration/ma_single_team/`) runs end-to-end against a real MA backend and all assertions in §7.4 hold.
- [ ] `state.json` after integration run: `team.status == "done"`, populated token counters and `result_summary`.
- [ ] `.orchestra/results/<team>/summary.md` exists and matches the final `agent.message`.
- [ ] `.orchestra/logs/<team>.ndjson` has at least `session.status_running`, `agent.message`, and `session.status_idle` entries.
- [ ] DESIGN-v2 §13 P1.4 checkbox ticked; §17 review-checklist items for §8 / §10.2 / event mapping revisited if the implementation surfaced shape changes.
- [ ] `make test && make vet && make lint` green.
