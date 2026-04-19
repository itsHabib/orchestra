# Feature: P1.4 — MA session lifecycle (StartSession + Events + Send)

Status: **Proposed**
Owner: @itsHabib
Depends on: [00-store-interface.md](./00-store-interface.md) (shipped), [p13-registry-cache.md](./p13-registry-cache.md), P1.2 spawner scaffolding (existing in `pkg/spawner/`).
Relates to: [DESIGN-v2.md](../DESIGN-v2.md) §6 (architecture), §8 (Spawner interface), §9.3 (`StartSession`), §9.4 (`Session.Events`), §10.2 (`state.json` schema), §13 phase P1.4, §16 (event mapping).
Target: single-team end-to-end MA run producing a text-only deliverable. No repo resources, no resume, no steering.

**Deviation from DESIGN-v2 §13 P1.4.** The upstream phase spec says *"StartSession + Events + Send + watchdog"*, where the watchdog sends a `user.interrupt` + status-check nudge on silence and transitions to `stalled` only on persistent silence. This doc replaces the nudge-then-stall watchdog with a hard per-team context deadline that maps directly to `failed`. Rationale: `stalled` is functionally identical to `failed` until P1.8 (resume) lands, and the nudge is a product hypothesis with no empirical data behind it. v1's existing `defaults.timeout_minutes` + context-deadline pattern is reused verbatim. See §5.4.

---

## 1. Overview

P1.3 delivers stable agent/env IDs in the cache. P1.5 delivers repo-backed artifacts. P1.4 is the thinnest possible slice between them — the phase where an orchestra run first touches `Beta.Sessions.New`, `Beta.Sessions.Events.StreamEvents`, and `Beta.Sessions.Send`. One team. One MA session. Text-only output (a summary.md written from the agent's final message). No GitHub repo mounted, no resume path, no human steering, no tool-confirmation loop.

The point of shipping P1.4 narrow is to prove the session lifecycle plumbing before layering any of the harder chapters on top. If stream-first ordering, event-to-state translation, and the deadline-enforced event loop all work against a trivial text task, P1.5 can focus on `github_repository` resources and branch resolution without reopening the basics.

Concretely, P1.4 lands:

1. **`ManagedAgentsSpawner.StartSession`** — wraps `Beta.Sessions.New` using the agent/env handles P1.3 caches produce.
2. **Stream-first engine helper** — opens `Beta.Sessions.Events.StreamEvents` *before* sending the initial `user.message`, because MA only delivers events emitted after the stream is open.
3. **Event translator** — maps MA events to the orchestra event union per DESIGN-v2 §16 and updates `state.json` via `UpdateTeamState`.
4. **Hard per-team timeout** — one `context.WithTimeout(ctx, defaults.timeout_minutes)` per team; deadline expiry cancels the local stream and marks the team `failed`. No nudging, no `stalled` state.
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
- F7. Per-team hard timeout: the engine wraps each team's event loop in `context.WithTimeout(ctx, defaults.timeout_minutes)`. On deadline, the event loop observes `ctx.Done()`, calls `Session.Cancel()` to close the local reader, and transitions the team to `failed` with `last_error: "timeout: no events for N minutes"`. The MA session itself is not archived (resumable later once P1.8 is built, if we care to).
- F8. All MA API calls flow through the retry layer established in P1.2 (429/5xx exponential backoff + `Retry-After`, non-retryable 4xx fail fast).

**Non-functional.**

- NF1. **Stream-first invariant.** No code path may `Send` before `Events` has returned. Enforced by engine code structure; the `Session` type does not expose a sugar helper that hides the ordering.
- NF2. **Single writer invariants.** One writer for `state.json` (`UpdateTeamState` mutator funnel, DESIGN-v2 §10.2). One writer per `.orchestra/logs/<team>.ndjson` (the translator goroutine). The translator processes events sequentially — log append, then state update, then read the next event.
- NF3. **Transport recovery without duplicates.** On stream transport error, reopen the stream and use event IDs to dedupe per DESIGN-v2 §9.4. No persisted event appears twice in the NDJSON log or re-triggers a state transition.
- NF4. **Timeout bounded and non-recursive.** One deadline per team per run. Deadline expiry does not retry the session — it marks the team `failed` and moves on.
- NF5. **Rollback-clean.** One PR (or a tight series). `git revert` restores "P1.3 without sessions" without disturbing local-backend code.

---

## 3. Scope

### In-scope

- `pkg/spawner/managedagents/` — `session.go` (Session struct + StartSession), `translator.go` (MA event → orchestra event + state mutations).
- Engine wiring: the `startTeamMA` helper that enforces stream-first ordering, wraps the event loop in a `context.WithTimeout`, and hands off to the per-team event loop.
- State transitions: `running`, `idle`, `done`, `failed`, `terminated` — driven by events (§16) and by the team's context deadline (§5.5).
- Result-summary capture: final `agent.message` text → `.orchestra/results/<team>/summary.md`.
- Token/cost counter accumulation from `span.model_request_end`.
- Unit tests for translator + timeout behavior + stream-first ordering.
- Opt-in integration fixture exercising a single-team end-to-end MA run.

### Out-of-scope

- **GitHub repo resources (`github_repository`).** P1.5. P1.4's fixture is text-only.
- **`orchestra resume`.** P1.8. Sessions orphaned by P1.4 interruption must be rerun.
- **`orchestra msg` / `interrupt` CLI steering.** P1.9. `Session.Send` is engine-internal in P1.4 (used only for delivering the initial prompt).
- **Tool confirmation loop (`stop_reason: requires_action`).** Mapped to `failed` with a descriptive `last_error`. v1 default is `always_allow` on orchestra-managed agents (DESIGN-v2 §9.5), so this path is only reached on misconfiguration.
- **Multiagent / thread events.** Per §16 those are persisted to NDJSON but otherwise ignored.
- **Local-backend changes.** P1.4 touches only the MA spawner implementation.
- **Members / coordinator under MA.** Already validation-warned in P1.0 and ignored (DESIGN-v2 §9.7).
- **PR creation / artifact branch resolution.** No `repository_artifacts[]` population in P1.4 — that lives with the repo-backed flow in P1.5.

---

## 4. Data model / API

No new persisted shape. `state.json` already has every field the translator writes (see DESIGN-v2 §10.2 and `pkg/store.TeamState` shipped in PR #2). This section pins the translator contract and the Session surface.

### 4.1 `PendingSession` and `Session` — two types to make stream-first a compile-time invariant

```go
package managedagents

// PendingSession is a created-but-not-streaming MA session. It has no Send
// method, so the compiler refuses any code path that tries to deliver a
// user event before the event stream is open. The only exits are Stream
// (which returns a Session) and Cancel.
type PendingSession struct {
    id       string
    client   anthropic.Client     // pinned SDK version, see DESIGN-v2 §9
    teamName string
    log      io.Writer            // engine's per-team NDJSON writer
    store    store.Store          // for UpdateTeamState
    clock    func() time.Time
}

// StartSession wraps Beta.Sessions.New. No prompt is sent here, no stream
// is opened. Returns a PendingSession the caller must either Stream or
// Cancel.
func (m *ManagedAgentsSpawner) StartSession(ctx context.Context, req spawner.StartSessionRequest) (*PendingSession, error)

// Stream opens Beta.Sessions.Events.StreamEvents, starts the translator
// goroutine, and returns a Session (Send-capable) along with the event
// channel. Call exactly once per PendingSession; after Stream returns
// successfully the PendingSession must not be used again (treat as moved).
// The channel closes on terminal session state (idle+end_turn, terminated,
// unrecoverable error) or when ctx is done.
func (p *PendingSession) Stream(ctx context.Context) (*Session, <-chan Event, error)

// Cancel closes a PendingSession that will never be streamed. Does not
// archive the MA session (resumable later by P1.8). Idempotent: the first
// call returns the close error if any; subsequent calls return nil.
func (p *PendingSession) Cancel() error

// Session is a streaming MA session with Send-capability.
type Session struct {
    // internal fields promoted from PendingSession plus stream state
}

// ID returns the MA session ID. Used by the engine to persist session_id
// in state.json (load-bearing for P1.8 resume).
func (s *Session) ID() string

// Send delivers a user event (message or interrupt) to MA. In P1.4 the
// only caller is the engine delivering the initial prompt immediately
// after Stream returns. Returns ctx.Err() on deadline or cancellation;
// returns the wrapped MA error on API failure.
func (s *Session) Send(ctx context.Context, ev UserEvent) error

// Cancel closes the local stream reader. Does not archive the MA session.
// Idempotent: the first call returns the close error if any; subsequent
// calls return nil.
func (s *Session) Cancel() error
```

**Why two types.** The single-type design (`Session` with both `Events` and `Send`) enforces the stream-first invariant only by reviewer vigilance — any future callsite can call `Send` before `Events`, and the bug is a missed early event at runtime. Splitting into `PendingSession` / `Session` moves the invariant to the compiler: `Send` is unreachable on a `PendingSession`; `Stream()` is the one-way door. P1.5 (repo wiring), P1.8 (resume), P1.9 (`orchestra msg`) each add new callsites — this keeps them honest.

The cost is one extra type and one "treat as moved after `Stream`" convention (commented at the call site). Worth it.

### 4.2 Event translation

The full MA-event → orchestra-event mapping lives in DESIGN-v2 §16. P1.4 implements every row; `translator.go` has a comment at the switch statement pointing at §16 as the canonical spec.

**State-affecting events** — the subset that call `UpdateTeamState`:

| MA event | State mutation |
|---|---|
| `session.status_running` | `team.status = "running"`; stamp `started_at` if zero |
| `session.status_idle + end_turn` | **write `summary.md` first** (atomic rename); *then* `team.status = "done"`, `team.result_summary = <last agent.message text>`, stamp `ended_at`. If the summary write fails, transition `failed` with `last_error: "summary_write: <cause>"` instead — see §5.5 |
| `session.status_idle + requires_action` | `team.status = "failed"`; `team.last_error = "tool confirmation requested; not supported in v1"` |
| `session.status_idle + max_turns` | `team.status = "failed"`; `team.last_error = "max turns reached"` |
| `session.status_idle + error` | `team.status = "failed"`; `team.last_error = <event payload>` |
| `session.status_terminated` | `team.status = "terminated"`; finalize counters |
| `session.error` | `team.last_error = <message>` (status unchanged; next idle/terminated decides). **Caveat:** if `session.error` is followed by a stream drop that never reconnects, the team remains `running` until the per-team deadline fires (§4.3). |
| `span.model_request_end` | accumulate `input_tokens`, `output_tokens`, `cache_{read,creation}_input_tokens`, `cost_usd` |
| `agent.*`, `session.status_rescheduled` | NDJSON log only; no state change |

Every event (state-affecting or not) is NDJSON-logged first. The translator does not fork per-event goroutines.

### 4.3 Timeout behavior

Each team's event loop runs inside a `context.WithTimeout(ctx, defaults.timeout_minutes)` derived from the engine's run context. On deadline:

1. The event-loop goroutine observes `ctx.Done()`, stops reading from the stream, and calls `Session.Cancel()` to close the local reader.
2. It calls `UpdateTeamState` to transition team state to `failed` with `last_error: "timeout: no events for N minutes"`.
3. The MA session is **not** archived. The container stays alive until MA's own idle timeout reclaims it, or until a future `orchestra resume` (P1.8) attaches and picks up where we left off.

There is no nudge, no "one strike" leniency, no separate watchdog goroutine. A team that doesn't reach a terminal MA state within its deadline is a failure from orchestra's perspective. If we later decide a softer policy earns its complexity (after P1.8 makes `stalled` semantically distinct from `failed`, or after real data shows nudges recover stuck sessions often enough to matter), we add it then.

### 4.4 Event dedup across stream reopen

DESIGN-v2 §9.4 says the backend "transparently reopens the stream, uses `Beta.Sessions.Events.ListAutoPaging` to build a seen-set, and skips already-delivered event IDs." P1.4 is the first implementation. The rules:

**Seen-set shape.** A bounded ring buffer of the most recent `N` event IDs observed on this session. `N = 512` (roughly 5-10× the worst realistic event burst between drops; cheap to hold in memory). Ring eviction is FIFO — oldest IDs fall off as new ones arrive. No allocation churn after steady state.

**Where dedup runs.** Before NDJSON append and before translation. The contract is "each event is processed at most once end-to-end." Logging a duplicate would corrupt post-mortems; translating a duplicate would double-count tokens.

```
read raw event e from stream
if e.id in seen_set:
    continue          // silent skip
seen_set.push(e.id)
append(e) to NDJSON
translate(e)
```

**Reopen choreography.** On transport error from `StreamEvents`, the retry layer (§2 F8) decides whether to retry. If it does, the reconnect path reopens the stream *and* calls `Beta.Sessions.Events.ListAutoPaging(after: last_event_id)` to backfill events emitted during the drop. Every event from the backfill goes through the same dedup gate; everything new on the reopened stream likewise. Duplicates are common across this boundary — that's why the seen-set exists.

**Persistence for P1.8.** Every state-affecting event's ID is written to `team.last_event_id` in `state.json` via `UpdateTeamState` (the `session.*` and `span.model_request_end` rows in §4.2 all set it). On `orchestra resume` (P1.8), the engine reads `last_event_id` from `state.json`, seeds the seen-set with just that one ID, and calls `ListAutoPaging(after: last_event_id)` to replay. This closes Open Q #1 (yes, populate `last_event_id` in P1.4 even though the reader lands in P1.8 — the dedup story depends on it).

**What the seen-set does not protect against.** A session whose MA-side event log mutates retroactively (events renumbered, IDs reused). Neither the spec nor the SDK allows for this, so we treat it as out-of-scope. If it ever happens, duplicates become visible and we redesign.

---

## 5. Engineering decisions

### 5.1 Stream-first ordering enforced by the `PendingSession` → `Session` type split

Tempting refactor: make `StartSession` return an already-streaming session so callers can't get the ordering wrong. Rejected because `ResumeSession` (P1.8) wants different ordering — create-or-get, replay history, then stream. Coupling stream lifecycle to session creation forces one of the two code paths into awkward contortions.

Instead, the type system splits the two phases: `StartSession` returns a `PendingSession` (no `Send`), and the one-way `Stream()` call hands back a `Session` (with `Send`). The engine helper threads that:

```go
func (r *Run) startTeamMA(ctx context.Context, team *config.Team) (*Session, <-chan Event, error) {
    pending, err := r.spawner.StartSession(ctx, r.reqFor(team))
    if err != nil {
        return nil, nil, err
    }
    s, events, err := pending.Stream(ctx)
    if err != nil {
        _ = pending.Cancel()
        return nil, nil, err
    }
    if err := s.Send(ctx, initialPromptFor(team, r)); err != nil {
        _ = s.Cancel()
        return nil, nil, err
    }
    return s, events, nil
}
```

`ResumeSession` (P1.8) will have its own counterpart that produces a `*Session` via a different constructor (create-or-get → replay → `Stream`), but it will still go through the same `PendingSession` → `Session` choke point so the Send-is-forbidden-before-stream invariant holds there too.

Nothing about this forces callers to use the helper — but any callsite that tries to send an event must have called `Stream()` first, because a `PendingSession` has no `Send` method. Review still matters, but a missed `Stream()` is a compile error, not a runtime race.

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

### 5.4 Hard timeout over nudge-then-stall

DESIGN-v2 §9 intro proposes a watchdog: on silence, send `user.interrupt` + a status-check `user.message`; if silence persists for another `timeout_minutes`, mark the team `stalled` and leave the session alive. We depart from that here.

**Why a hard timeout is enough for P1.4.**

1. **`stalled` is functionally identical to `failed` without P1.8.** Both end the team, both require a full rerun, both surface the same way in `orchestra status`. The one thing `stalled` is supposed to buy — "the MA session is still alive, `orchestra resume` can attach to it later" — does not exist until P1.8 ships. Shipping `stalled` now is shipping vocabulary without semantics.
2. **The nudge is a product hypothesis with no data.** We do not know whether `user.interrupt` + "status check" actually unsticks agents that would otherwise have hung. Shipping it commits us to a policy we have not tested; removing it later is harder than adding it later.
3. **v1's hard-timeout pattern is proven.** The existing local-backend spawner uses `context.WithTimeout(ctx, defaults.timeout_minutes)` and kills subprocess on expiry. Users already know the knob and its semantics. Matching behavior under MA backend is the principle of least surprise.

**When to revisit.** Either (a) P1.8 lands and `stalled` becomes meaningfully different from `failed` (resumable vs not), or (b) we see enough real-world stuck-session events in integration runs to know a nudge would help. Until then the extra machinery is speculative.

**What we keep from the DESIGN-v2 proposal.** The `defaults.timeout_minutes` config knob (already present in v1) and the principle that the MA session is *not* archived on orchestra-side timeout — leaving MA's own idle reaper to reclaim the container, and leaving the session attachable if we later build resume.

**Cost caveat the DESIGN-v2 proposal glossed over, and this doc inherits.** If `defaults.timeout_minutes` is shorter than MA's own idle reaper window (currently not documented publicly but historically ~60 min), an orchestra-timed-out session keeps billing against the org until MA reclaims it. Nothing in P1.4 is watching. Users on metered plans should either set `timeout_minutes` close to MA's reaper, or accept the window. A future `orchestra sessions rm` (P1.9) closes the loop; for P1.4 we document the cost surface in `orchestra status` output ("MA session left alive for P1.8 resume").

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
| `session.status_idle + max_turns` | `failed` | `max turns reached` |
| Per-team deadline exceeded (`ctx.Done()`) | `failed` | `timeout: no events for N minutes` |
| `end_turn` received but `summary.md` write fails | `failed` | `summary_write: <cause>` |
| `UpdateTeamState` returns error mid-translation | `failed` | `state_write: <cause>` — translator stops, stream reader exits |
| `session.status_terminated` | `terminated` | (payload, if any) |
| Successful completion | `done` | — |

### 5.6 Raw MA JSON in NDJSON log, not translated events

Logs hold the raw `Beta.Sessions.Events` payloads. Translated orchestra events are a projection — recoverable from raw by rerunning the translator. Logging the projection instead throws away information (original field names, untranslated enum values, event IDs MA uses for dedupe on resume).

Cost: the NDJSON schema is coupled to the MA event schema. That's fine — the schema is already coupled at the translator, and logs are not a stable API we promise to external consumers.

### 5.7 Relationship to `run.Service` and `internal/workspace`

P1.4 writes state via `Workspace.UpdateTeamState` and logs via `Workspace.LogWriter` as they exist today. When [01-service-layer.md](./01-service-layer.md) lands, those calls move to `run.Service.Record*` and `run.Service.LogWriter` (or a sibling), identical semantics.

If 01-service-layer.md lands first, P1.4 is written directly against `run.Service`. If P1.4 lands first, the service-layer PR migrates P1.4's callsites as part of its mechanical diff. Either order is cheap.

### 5.8 Error propagation inside the translator

`UpdateTeamState` can fail (I/O error on the file store, disk full, permissions). `summary.md` write can fail for the same reasons. `Send` can fail on ctx cancellation or MA API error. The translator's policy is **fail loudly over continue on stale state** — any of the above propagates as a team-`failed` transition rather than getting logged and swallowed.

| Source of error | Action |
|---|---|
| `UpdateTeamState` returns error | `slog.Error` with `team`, `session_id`, `event_id`. Best-effort write a fallback `UpdateTeamState` with status=`failed` and `last_error: "state_write: <cause>"`. If that also fails, log and exit the translator. Do not continue processing events — the in-memory view and persisted view have diverged and subsequent events would lie. |
| `summary.md` write fails on `end_turn` | Do not transition to `done`. Transition to `failed` with `last_error: "summary_write: <cause>"` instead. The user sees a failed team in `orchestra status`, which matches reality — the deliverable is not on disk. |
| `Session.Send(initial)` returns ctx error | Translator exits cleanly; the engine helper's `_ = s.Cancel()` tears down the stream. State transition driven by whoever canceled the context (usually the run-level timeout). |
| Translator panic on unrecognized event shape | Recovered, `slog.Error` with raw JSON + event ID, event skipped, translator continues reading. This is distinct from "recognized shape but mutator panicked" — that case transitions the team to `failed` because the invariant that produced the shape likely affects future events too. |

The divergence between "skip an unrecognized event and keep going" vs "fail the team on a mutator panic" is intentional: the former protects us from MA adding a new event type we don't know about (shouldn't kill the run), the latter protects us from a latent bug that the next identical event will also trigger (keep killing the run sounds better than silently desyncing).

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Text-only deliverable for the fixture | (a) Include repo checkout in P1.4 fixture. (b) Synthetic "write no files" task. | (a) conflates P1.4 and P1.5 — if the run fails, we don't know whether it's sessions or repo integration. (b) is not realistic per DESIGN-v2 §13 P1.4: "not a synthetic write-no-files task." Text-only summary is the simplest real task. |
| Hard per-team timeout, no nudge | (a) Nudge-then-stall per DESIGN-v2 §9 intro. (b) Configurable per-team policy. | (a) ships vocabulary (`stalled`) without semantics until P1.8 lands; the nudge itself is an untested product hypothesis. (b) is premature — we have no data yet on what policies users want. Hard timeout matches v1's proven pattern and is trivially upgradable later if evidence warrants it. |
| Stream-first ordering enforced by engine helper, not `Session` API | (a) `StartSession` auto-streams. (b) `Session.Start` combines creation + stream + initial send. | (a) and (b) couple lifecycle to creation and force `ResumeSession` (P1.8) into a different shape. Engine helper keeps `Session` lifecycle-agnostic. |
| Summary = last `agent.message` before `end_turn` | (a) Accumulate all agent messages. (b) Agent writes summary to a file. | (a) is noisy. (b) requires repo/Files API we don't have yet. Final-message capture is the cheapest correct answer. |
| Fail on `requires_action` | (a) Implement tool-confirmation CLI in P1.4. (b) Auto-allow all. | (a) is a whole feature (CLI + MCP toolset handling). (b) overrides MA's `always_ask` globally. Failing fast is honest and non-destructive. |
| Raw MA JSON in NDJSON log | (a) Translated orchestra events in log. (b) Both. | Raw is reprocessable and doesn't lose information. Translated is recoverable from raw. Both is duplicated storage for no upside. |
| Timeout enforced in engine, not spawner | (a) Spawner wraps its own API calls in deadlines. | The deadline is tied to the *team's* event loop, not to any individual API call; the engine owns team lifecycle. Spawner-side deadlines would have to be wired per-call and would not cover the stream-reader loop itself. |

---

## 7. Testing

### 7.1 Translator unit tests

`pkg/spawner/managedagents/translator_test.go` with a fake event feeder:

- One test case per row of DESIGN-v2 §16. Feed the MA event, assert the orchestra event emitted and any state mutation recorded against a `memstore.MemStore`.
- `session.status_idle + end_turn` with a preceding `agent.message`: verifies summary text is captured and `summary.md` is written. **At least one variant uses the real `fsutil.AtomicWrite` against `t.TempDir()`** so the `.tmp` → `os.Rename` path is exercised; the failure variants (read-only dir, path collision) use the same real sink to assert the correct `failed` transition with `last_error: "summary_write: ..."`.
- `span.model_request_end` sequence: counters accumulate across multiple events, not overwrite.
- Unknown event type: logs at warn and continues without state change or panic.

### 7.2 Timeout tests

`session_test.go` with a fake event feeder and a test-controlled context:

- No timeout breach (events arrive regularly before deadline) → team completes normally; no timeout error.
- Deadline expires while the stream is quiet → state transitions to `failed` with `last_error` matching `/^timeout: no events/`; `Session.Cancel` called; MA session not archived.
- Deadline expires mid-run after partial progress → team transitions to `failed`; previously-accumulated token counters and the in-progress `result_summary` are preserved in `state.json` (not zeroed).

### 7.3 Stream-first ordering enforcement

The type split in §4.1 does most of the work: any code path that has a `PendingSession` and calls `Send` fails to compile. A thin test on `startTeamMA` still verifies the helper's own ordering — using a fake spawner that records the sequence of `StartSession` → `Stream` → `Send` — but this is belt-and-suspenders, not the primary enforcement.

### 7.4 Integration fixture (opt-in)

`test/integration/ma_single_team/` — a minimal `examples/`-style project with one team whose prompt asks for a short markdown summary of a fixed input. Requires `ORCHESTRA_MA_INTEGRATION=1` and a real API key in `ANTHROPIC_API_KEY`. Assertions:

- Process exits 0.
- `.orchestra/state.json` has `team.status == "done"`, non-zero token counters, populated `result_summary`, and **non-empty `team.agent_id`, `team.agent_version`, `team.session_id`, `team.last_event_id`** — P1.8 resume will need all four and this is the first place they're written.
- `.orchestra/results/team-a/summary.md` exists and is non-empty.
- `.orchestra/logs/team-a.ndjson` contains `session.status_running`, at least one `agent.message`, and `session.status_idle` with `stop_reason: end_turn`.

CI runs unit tests by default. The integration fixture runs only when `ORCHESTRA_MA_INTEGRATION=1` is set (a separate GitHub Actions workflow with a secret-backed key, triggered manually or on release).

### 7.5 Failure-mode tests

Against a fake MA client and a real file sink (`t.TempDir()`):

- `StartSession` → non-retryable 4xx: team marked `failed` with the expected prefix.
- `StartSession` → 500 then 200: retry layer runs, team completes.
- **Stream reopen with overlapping events.** Feed the translator events `[1, 2, 3]`, force a stream-reopen boundary, then feed `[2, 3, 4, 5]` (MA's `ListAutoPaging` replay behavior). Assertions: NDJSON contains exactly `[1, 2, 3, 4, 5]` in order with no duplicates; `team.last_event_id` advances monotonically; no state mutation fires twice for the same event ID. Uses the real seen-set implementation, not a stub.
- **`UpdateTeamState` fails mid-run.** Inject an error from the store; assert the translator attempts the fallback `failed` transition, logs loudly, and exits. Stream reader observes channel close.
- **`summary.md` write fails on `end_turn`.** Point the results dir at a read-only path; assert `team.status == "failed"`, `last_error` matches `/^summary_write/`, and no partial `summary.md` is left behind.
- **Mutator panic vs unrecognized-event panic.** Inject a panic from the `session.status_running` mutator → team fails with `state_write` or equivalent loud error. Separately, inject an event whose type the translator doesn't know → warn-and-continue, no state change.

---

## 8. Rollout / Migration

Single PR, ~1200-1500 LOC. (Initial estimate was 600-900; adversarial review calibration upward — translator + full §16 coverage + per-row tests + fake-event feeder + seen-set + dedup tests + timeout tests + stream-first test + integration fixture + engine branch is meaningfully more work than a typical narrow phase.) MA-backend had zero callers before P1.4 (opt-in via `backend.kind: managed_agents` in config), so no migration of existing users.

Internal PR order:

1. **`pkg/spawner/managedagents/session.go`** — Session struct, StartSession, Events (skeleton that reads from `StreamEvents` and forwards raw events), Send, Cancel.
2. **`translator.go`** — MA event → orchestra event mapping per §16; `UpdateTeamState` dispatch. Unit tests alongside.
3. **Engine wiring** — `startTeamMA` helper (stream-first ordering + `context.WithTimeout` wrap) and the MA-backend branch of `runTeam` / `runTier` in `cmd/run.go` (or `run.Service.StartTeam` if 01-service-layer.md landed first). Timeout tests alongside.
4. **Integration fixture** — `test/integration/ma_single_team/` + opt-in build tag + README.
5. **Docs** — tick DESIGN-v2 §13 P1.4 and §17 review checklist items (noting the watchdog deviation); update AGENTS.md/CLAUDE.md project-structure section.

**Rollback.** `git revert`. MA-backend code is self-contained in its own packages. The one caveat: the engine wiring in `cmd/run.go` (or `run.Service`) adds a backend-branch to `runTeam`/`runTier`. If anything else lands on those files between P1.4 and a hypothetical revert, expect a small merge conflict on the branch fork — not a true coupling, just the usual cost of two changes touching one function.

**Dependencies.** Merges after P1.3 (needs `EnsureAgent` / `EnsureEnvironment` returning valid handles). Independent of 01-service-layer.md — whichever lands second migrates to the other.

---

## 9. Observability & error handling

- NDJSON log append is the first step for every received event. If anything downstream panics, the log has the last event for post-mortem.
- State transitions emit `slog.Info` with `team`, `from`, `to`, `session_id`, `event_id`. Post-mortem friendly without opening the NDJSON.
- Per-team deadline expiry: `slog.Error` with `team`, `session_id`, and the configured `timeout_minutes`. The MA session is intentionally left alive, and the log entry says so.
- Retry layer: attempts at `slog.Debug` (op, attempt, wait); final failure at `slog.Error`.
- No Prometheus/OTel in P1.4. Cache-hit rate, session-create latency, translator throughput — all earn their place when orchestra-in-prod asks for them, not now.
- Translator reaction to bad events splits by cause: **unrecognized event shape** (MA added a new event type we don't handle) → `slog.Warn` with the raw JSON and event ID, event skipped, translator continues. **Mutator panic on a recognized shape** (latent bug in a state-affecting branch) → `slog.Error`, team transitions to `failed` via §5.8's fallback, translator exits. The difference is intentional — the first protects us from MA innovation, the second protects us from a bug the next identical event would retrigger.

---

## 10. Open questions

1. **Default `timeout_minutes` for MA sessions.** v1 inherited 30. MA sessions doing heavy refactor work may legitimately take longer; failing too early wastes the agent's partial progress and leaves a billable session alive for the MA-reaper window (§5.4). Defer tuning until the integration fixture has produced empirical timing data.
2. **Where does `startTeamMA` live?** If 01-service-layer.md lands first: `run.Service`. Otherwise: a private method on `orchestrationRun` in `cmd/run.go`. Non-blocking.
3. **CLI surfacing of "v1 does not support tool confirmation".** Error message is descriptive but terminal. Future work (P1.9 or later) may add an `orchestra confirm <team>` CLI. Out of scope here.

**Closed during adversarial review:**

- ~~Persist `last_event_id` / `last_event_at` in P1.4?~~ **Yes.** The dedup story in §4.4 depends on it; P1.8 inherits a stable schema.
- ~~Translator behavior on unknown MA event types?~~ **Warn-and-continue**, distinct from "mutator panic on recognized shape" (which fails the team). See §5.8 and §9.

---

## 11. Acceptance criteria

- [ ] `pkg/spawner/managedagents/` contains `session.go` and `translator.go` with accompanying `_test.go` files.
- [ ] `PendingSession` (no `Send` method) / `Session` type split exists; refuses to compile a callsite that `Send`s before `Stream`.
- [ ] Every row of DESIGN-v2 §16 has a corresponding translator unit-test case.
- [ ] Seen-set + dedup tests cover the reopen-with-overlap scenario; `team.last_event_id` advances monotonically and no NDJSON duplicates appear.
- [ ] Timeout tests cover no-breach, deadline-expired, and partial-progress-then-timeout paths.
- [ ] Failure-mode tests for `summary.md` write failure, `UpdateTeamState` failure, and mutator-panic-vs-unrecognized-event distinction all pass.
- [ ] Stream-first ordering test passes for `startTeamMA` (belt-and-suspenders over the type split).
- [ ] Opt-in integration fixture (`test/integration/ma_single_team/`) runs end-to-end against a real MA backend and all assertions in §7.4 hold — including non-empty `agent_id`, `agent_version`, `session_id`, `last_event_id` in `state.json`.
- [ ] `.orchestra/results/<team>/summary.md` exists and matches the final `agent.message`.
- [ ] `.orchestra/logs/<team>.ndjson` has at least `session.status_running`, `agent.message`, and `session.status_idle` entries.
- [ ] DESIGN-v2 §13 P1.4 checkbox ticked; §17 review-checklist items for §8 / §10.2 / event mapping revisited if the implementation surfaced shape changes.
- [ ] `make test && make vet && make lint` green.
