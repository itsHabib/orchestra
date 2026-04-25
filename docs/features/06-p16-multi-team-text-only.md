# Feature: P1.6 (text-only) — Multi-team DAG under Managed Agents

Status: **Shipped** (PR #12, commit ccfac18)
Owner: @itsHabib
Depends on: [p14-ma-session-lifecycle.md](./p14-ma-session-lifecycle.md) (shipped)
Supersedes ordering in: [DESIGN-v2.md](../DESIGN-v2.md) §13 — P1.6 runs **before** P1.5 (repo-backed artifact flow). See §1.
Relates to: [05-p15-repo-artifact-flow.md](./05-p15-repo-artifact-flow.md) (now deferred to after this chapter).
Target: two or more MA teams with `depends_on` edges, each producing a text deliverable that is inlined into downstream prompts. No GitHub. No repo mounts. No Files API.

---

## 1. Why this chapter exists (and why it runs before P1.5)

DESIGN-v2 §13 ordered the Phase 1 MA rollout as: single-team (P1.4) → repo-backed artifact flow (P1.5) → multi-team DAG (P1.6). That ordering couples two concerns — making DAG orchestration work under MA, and adding GitHub as a cross-team data substrate — into a single jump. They are separable.

Managed Agents containers are isolated per session and the Files API has no view of container-produced files (spike T1, `docs/SPIKE-ma-io-findings.md` §Q1), so DESIGN-v2 picked GitHub as the substrate for **non-text** cross-team data. But most DAG fan-out in orchestra today is **text** — a planner hands an analyst a plan, the analyst hands a synthesizer an analysis. The final `agent.message` from the upstream team is already captured as a result summary (p14 writes `summary.md`) and is already inlined into downstream prompts on the local backend via `internal/injection/builder.go` — and, as of p14, on the MA backend too: `cmd/run_ma.go:185` passes `state` into `injection.BuildPrompt` exactly like the local path.

**The plumbing for a text-only multi-team MA DAG is already in place.** This chapter's job is to prove it end-to-end, add a multi-team fixture, and harden the pieces that weren't exercised by the p14 single-team path (concurrent session creation, rate-limit handling, ordered tier transitions with real MA latency). Once that ships, P1.5 (repo-backed) layers on top as additive capability for teams whose deliverable is code.

The practical payoff: we get a working MA DAG — the load-bearing claim of the whole backend — without taking on GitHub integration complexity first. If the text-only DAG reveals shape problems in the MA backend (concurrent rate limits, tier-wide timeouts, session ordering quirks), we fix them in a small surface before stacking P1.5's artifact plumbing on top.

---

## 2. Requirements

**Functional.**

- F1. A `backend: managed_agents` config with two or more teams and `depends_on` edges runs to completion. Each team's final `agent.message` is persisted to `.orchestra/results/<team>/summary.md` (already done in p14) and made available to downstream teams via the existing prompt injection path.
- F2. Tier scheduling under MA matches the local backend's semantics: parallel within a tier, serial across tiers. `cmd/run.go:runTier` already handles this and is backend-agnostic; this chapter's job is to exercise it end-to-end with real MA sessions.
- F3. Concurrent session creation within a tier respects MA's 60-creates/min org limit. A client-side burst semaphore (default concurrency 20, configurable via `defaults.ma_concurrent_sessions`) throttles `StartSession` calls. Retry-after and 429 handling already exists in `pkg/spawner/managed_agents_session.go` (`withRetry`); the semaphore is additive protection against burst.
- F4. Tier failure propagation works as today: if any team in a tier fails, the tier fails and the run fails. Downstream tiers do not start. State reflects `status: failed` per team and `last_error`.
- F5. Downstream prompts see upstream result summaries via the existing `injection.BuildPrompt(team, project, state, ...)` call where `state.Teams[upstream].ResultSummary` is the final `agent.message` text. No new injection code; the p14 wire-up already feeds the right `state`.
- F6. `cmd/runs show <run-id>` renders per-team status, cost, session ID, and tier across a multi-team run. (Already supported by PR #7; this chapter verifies the display with multiple teams.)
- F7. A new `examples/ma_multi_team/` fixture with a planner → analyst two-team configuration, runnable via `orchestra run`, serves as the canonical example.

**Non-functional.**

- NF1. **No new hard dependencies.** This chapter uses what's already in the tree: p14 session lifecycle, PR #6 agent cache, PR #7 runs CLI. No GitHub client, no PAT plumbing, no new config schema.
- NF2. **Isolation verified.** Parallel teams in a tier run in independent sessions with independent containers, independent event streams, and independent `TeamState` writes through the single-writer `Store` mutator path (p14 §4). No cross-team interference.
- NF3. **Rate-limit safety exercised.** The test fixture and integration tests intentionally create concurrent sessions to verify the semaphore + retry layers cope with 429 under load. Not a stress test — a smoke test that the paths are live.
- NF4. **Local backend untouched.** `backend: local` multi-team DAG runs produce byte-identical results before and after this chapter. The only diffs are on the MA path and in tests/fixtures.

---

## 3. Scope

### In-scope

- Concurrency semaphore on `ManagedAgentsSpawner.StartSession` (if not already present). Configurable via `defaults.ma_concurrent_sessions` (default 20 per DESIGN-v2 §9).
- `examples/ma_multi_team/` fixture: two-team (planner, analyst) where analyst `depends_on` planner. Plain-text deliverables.
- Opt-in integration test `test/integration/ma_multi_team/` that runs the fixture against live MA (skipped unless `ORCHESTRA_MA_INTEGRATION=1`).
- Unit tests for concurrent tier execution with mock MA spawner: prove `runTier` fires N `StartSession` calls in parallel, waits for all, orders across tiers correctly.
- Any rate-limit-related bug fixes surfaced by F3. Expected to be small if anything.
- Update `cmd/run_ma.go`'s validator warning from "ignored in P1.4" → "ignored in P1.6" (or drop the version pin entirely; see §5.4).

### Out-of-scope

- **GitHub / repo-backed artifacts.** Deferred to P1.5 ([05-p15-repo-artifact-flow.md](./05-p15-repo-artifact-flow.md)). This chapter validates text-only flow.
- **Files API uploads/downloads.** Not needed for text-only.
- **Resume.** P1.8.
- **Human steering CLI (`orchestra msg`, `orchestra interrupt`).** P1.9.
- **Coordinator / members under MA.** Still no-ops with warnings; unchanged from p14.
- **Cross-tier optimization (starting a downstream team as soon as its specific upstreams finish, not the whole tier).** DAG stays tier-gated. Tier-gated scheduling is explicit in DESIGN-v2 §6 and is the simpler, more predictable model.
- **Stalled-team reconciler.** Per DESIGN-v2 non-goals; hard-timeout semantics from p14 stay.

---

## 4. Data model / API

No new persisted types. No new CLI commands. No new config fields except:

```go
// In internal/config/schema.go — Defaults.
type Defaults struct {
    // ... existing fields ...
    MAConcurrentSessions int `yaml:"ma_concurrent_sessions,omitempty" json:"ma_concurrent_sessions,omitempty"`
}
```

`ResolveDefaults` sets `MAConcurrentSessions = 20` if zero. Validator rejects negative values.

The semaphore lives on the `ManagedAgentsSpawner`:

```go
// In pkg/spawner/managed_agents.go.
type ManagedAgentsSpawner struct {
    store     store.Store
    client    *anthropic.Client
    startSem  chan struct{} // buffered, capacity == MAConcurrentSessions
}

func NewManagedAgentsSpawner(store store.Store, client *anthropic.Client, opts ...Option) *ManagedAgentsSpawner
// Option: WithConcurrency(n int)
```

`StartSession` acquires a slot on `startSem` before calling `Beta.Sessions.New` and releases it after the stream is established (or on error). The slot is held only for the create-and-open-stream window; subsequent event traffic is not throttled by the semaphore because the create-rate limit (60/min) is what we care about, not aggregate traffic.

---

## 5. Engineering decisions

### 5.1 Ordering — P1.6 before P1.5

DESIGN-v2 §13's ordering bundles two concerns (DAG under MA, and GitHub as substrate). Splitting them lets the first concern ship alone. Reasons:

1. Text-only fan-out is a real user case. A planner-then-writer DAG doesn't need code artifacts.
2. Failure isolation. If MA multi-team reveals concurrency or ordering bugs, we fix them in a small surface. With P1.5 bundled, a failing fixture could be either the DAG or the repo plumbing.
3. P1.5 is additive on top of a working multi-team DAG. Its repo flow activates per-team based on config; text-only teams keep working the same way.

### 5.2 Prompt builder stays as-is

p14 already neutralized member/coordinator/file-bus fragments for MA prompts (`cmd/run_ma.go:183`: `maTeam.Members = nil` before calling `BuildPrompt`, plus a trailing MA note). Nothing in this chapter touches the prompt builder. The `Capabilities`-struct refactor proposed in DESIGN-v2 P1.1.5 is not required for text-only multi-team runs — upstream result summaries are already flowing via `state`.

P1.5 will introduce `Capabilities.ArtifactPublish`, which is that chapter's concern. P1.6 doesn't need it.

### 5.3 Semaphore, not leaky bucket

MA's rate limit is 60 creates/min org-wide. A leaky bucket tracking time-windowed create counts would be the academically correct solution. A capacity-20 semaphore is what DESIGN-v2 §9 specifies and is operationally sufficient: 20 in-flight creates means the create rate is bounded by average session startup latency × 20. Under observed p14 latency (~2s per create), that's ~10 creates/sec peak — below the 60/min cap at steady state. The retry layer already in `pkg/spawner/managed_agents_session.go` handles the residual cases where burst exceeds the cap.

If a future run bumps into the cap regularly, replace the semaphore with a bucket. Not before.

### 5.4 Drop the "in P1.4" pin in validation warnings

`internal/config/schema.go:211` warns *"coordinator is ignored for backend.kind=managed_agents in P1.4"*. The version pin promises that P1.5 or P1.6 changes this. Neither does. Change the message to drop the phase reference: *"coordinator is not supported under backend.kind=managed_agents"*. Same for the members warning. Tiny cleanup, bundled with this chapter because it's the first chapter after P1.4 that actually exercises the MA path in a multi-team shape.

### 5.5 No new observability beyond what p14 gives

p14's per-team NDJSON log, summary, and `cmd/runs show` are sufficient for multi-team. A "run-level event log" aggregating across teams has been suggested; it is genuinely useful but is not required for correctness. Defer to a later chapter if the need surfaces operationally.

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Ship multi-team DAG text-only first, GitHub flow after | DESIGN-v2 ordering: GitHub flow first, then multi-team | Text-only flow validates DAG concerns in isolation; GitHub is additive. Matches the shape local backend shipped with. |
| Fixed capacity semaphore | Token bucket; no client-side throttle (rely on 429 retry only) | DESIGN-v2 prescribes a burst cap; 429-only throttling is reactive and burns retry budget. Semaphore is cheap and predictable. |
| Tier-gated scheduling | Graph-edge scheduling (start team as soon as specific upstreams finish) | Tier-gated is the existing model. Graph-edge adds complexity with no user request behind it. |
| One `examples/` fixture | Multiple fixtures per common DAG shape | One representative fixture proves the primitive works. Adding more is cheap and can happen as examples accumulate. |
| No prompt-builder refactor | Bundle DESIGN-v2's `Capabilities` struct | Text-only path doesn't need it. P1.5 introduces it where it earns its keep. |
| Keep hard-timeout semantics from p14 | Add stall detection / reconciler | DESIGN-v2 non-goal; p14 hard-timeout is operationally sufficient. |

---

## 7. Testing

### 7.1 Unit tests — concurrency semaphore

`pkg/spawner/managed_agents_test.go` gains:

- Default capacity 20 (or whatever `Defaults.MAConcurrentSessions` resolves to) honored when config leaves it zero.
- `WithConcurrency(n)` option overrides.
- 25 concurrent `StartSession` calls with capacity 20: at most 20 SDK `Beta.Sessions.New` calls in flight at any moment (observed via a blocking fake client that signals when each call starts/finishes).
- Error during `StartSession` releases the slot (verify by observing a 21st call proceeding after the failed one returns).

### 7.2 Unit tests — tier orchestration under MA

`cmd/run_ma_test.go` (already exists from p14) gains:

- Two teams, team B depends on team A. Mock `startTeamMAForTest` returns fake sessions; assert team A's `TeamState.ResultSummary` is present in team B's prompt (parse the captured prompt argument).
- Three parallel teams in a single tier with independent sessions: all start before any finish (prove parallelism; no serial execution).
- Tier 0 fails → tier 1 never starts → run exits non-zero; `state.json` shows tier 0 team `failed` and tier 1 teams never transitioned from `pending`.

### 7.3 Integration test — live MA

`test/integration/ma_multi_team/`:

- Two teams. Planner: *"Write a three-bullet outline for a blog post about sourdough. Return the outline as your final message."* Analyst (depends on planner): *"Expand each bullet in the upstream's outline into a paragraph."*
- Skipped unless `ORCHESTRA_MA_INTEGRATION=1`.
- Asserts: both teams reach `status: done`; analyst's final summary references content from the planner's outline (regex on at least one planner bullet appearing in analyst's summary, proving the injection path works end-to-end).
- Cleanup: MA sessions get archived by orchestra's normal exit path (DESIGN-v2 §6 step 5).

### 7.4 What we explicitly do not test

- Stress levels near the 60-creates/min cap. Integration cost is real; waits are inevitable. The semaphore unit test proves the client-side cap is respected; crossing that boundary is MA's concern.
- Teams with `members:` under MA. Still ignored with a warning; p14 already verified that path.
- Resume across orchestra restarts. P1.8.

---

## 8. Rollout

Single PR, because the surface is small:

1. Add `Defaults.MAConcurrentSessions` + validator default.
2. Add semaphore to `ManagedAgentsSpawner.StartSession`.
3. Update validation warning messages (§5.4).
4. Add `examples/ma_multi_team/` fixture (two teams, planner → analyst).
5. Add unit tests per §7.1, §7.2.
6. Add integration test per §7.3 (opt-in).
7. Update README: mention multi-team MA runs and the example.

**Rollback.** PR revert. No schema changes to unwind. Existing single-team MA runs are unaffected either way.

---

## 9. Observability & error handling

- Semaphore acquire/release at DEBUG via `slog` (`ma_start_session_slot` counters). Useful for diagnosing "my run is slow" vs "my teams are rate-limited."
- Existing per-team NDJSON log and `.orchestra/results/<team>/summary.md` unchanged.
- `cmd/runs show <run-id>` already renders multi-team status (PR #7).
- Tier boundaries logged at INFO (existing).

---

## 10. Open questions

1. **Default concurrency value.** DESIGN-v2 §9 says 20. Reasonable; revisit only if 20 proves too low or too high against the 60/min cap. Leave configurable.
2. **Does the semaphore need to cover `EnsureAgent` / `EnsureEnvironment` too?** Those also hit `Beta.Agents.New` / `Beta.Environments.New` which count against the 60/min create budget. On a cold cache, N teams in one tier → N agent creates + N session creates = 2N creates. Safer bet: the same semaphore wraps all three create calls, not just sessions. Decide during implementation — if the per-team lifecycle (`ensure → ensure → start`) is serial per team, a single slot per team is enough. (Today it is serial — see `cmd/run_ma.go:startTeamMA`.)
3. **Should the example fixture use members on the planner to demonstrate the "members ignored" warning surfaces correctly in a real run?** Marginal value; skip unless someone asks.
4. **Error propagation across tier boundaries.** If team A in tier 0 fails with a transient MA 503, does the run continue with partial results? Today's answer is no — any tier failure fails the run. Matches local behavior. Don't change.
5. **Observability: do we want a run-level aggregated event log** (`.orchestra/logs/run.ndjson`) in addition to per-team logs? Useful for `orchestra status` to tail live. Orthogonal to P1.6; not required.

---

## 11. Acceptance criteria

- [ ] `Defaults.MAConcurrentSessions` parses; default 20 applied.
- [ ] `ManagedAgentsSpawner.StartSession` throttles via semaphore; unit test proves at most N in-flight.
- [ ] Two-team MA fixture (`examples/ma_multi_team/`) exists.
- [ ] `cmd/run_ma_test.go` covers two-team ordering, parallel tier execution, tier-failure short-circuit.
- [ ] Integration test at `test/integration/ma_multi_team/` runs against live MA when `ORCHESTRA_MA_INTEGRATION=1`; skipped otherwise.
- [ ] `cmd/runs show <run-id>` displays all teams + tiers correctly on a multi-team run.
- [ ] Validation warnings no longer say "in P1.4".
- [ ] `backend: local` multi-team runs produce byte-identical output before and after this chapter.
- [ ] `make test && make vet && make lint` green.
- [ ] Follow-up tracked: P1.5 (repo-backed artifact flow, [05-p15-repo-artifact-flow.md](./05-p15-repo-artifact-flow.md)) — landable on top of this chapter.
