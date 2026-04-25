# Feature: `agents.Service` — extract MA + cache choreography out of `cmd/`

Status: **Draft**
Owner: @itsHabib
Depends on: [01-service-layer.md](./01-service-layer.md) (shipped), P1.3 ([p13-registry-cache.md](./p13-registry-cache.md)) (shipped), P1.4 ([p14-ma-session-lifecycle.md](./p14-ma-session-lifecycle.md)) (shipped)
Target: lands before the next MA-touching CLI surface (`orchestra sessions ls`, `envs gc`, anything else that calls the Managed Agents API from a command).

---

## 1. Overview

`01-service-layer.md` introduced `internal/run/run.Service` and called out `agentcache.Service` / `envcache.Service` as follow-ups "after P1.3 lands." P1.3 landed (PR #6). So did P1.4 (PR #8) and the workflow-first CLI (PR #7). The service-layer extract for agent/env choreography never happened, and the gap is now visible:

- `cmd/agents.go` imports `github.com/anthropics/anthropic-sdk-go`, constructs an MA client via `machost.NewClient()`, paginates `client.Beta.Agents.List(...)`, runs a worker pool over `client.Beta.Agents.Get(...)`, classifies results ("active" / "archived" / "missing" / "unreachable"), filters by `spawner.AgentCacheKeyFromMetadata`, and deletes cache records via `filestore.FileStore` — all inline in the CLI package.
- `cmd/runs_prune.go` (PR #7) duplicates the same shape: same SDK import, same pagination loop, same orphan-detection predicate, same filestore handshake, just keyed by run-agent-ID instead of cache-key.
- `cmd/run_ma.go` (PR #8) imports `internal/machost` to build an SDK client before handing off to `spawner`. The rest of its surface talks to `run.Service` — it is the only MA caller in `cmd/` that already uses the service-layer pattern.

Three callers, two of them with nearly identical pagination bodies. Every future command that touches MA will pile on a fourth.

This doc proposes `internal/agents/agents.Service` (name TBD in §10.1): the domain service that owns the MA-plus-cache operations `cmd/` currently hand-rolls. Callers get `List`, `Prune`, `Orphans`, `EnsureAgent`, `Get`. No `cmd/` file imports `anthropic-sdk-go` or constructs an MA client after migration.

This is **PR B** from `01-service-layer.md`'s rollout, with scope widened to cover the `ls` / `prune` / orphan-reconcile choreography that PR #7 added after that doc was written.

---

## 2. Requirements

**Functional.**

- F1. `agents.Service` owns: `EnsureAgent` (from P1.3 — already exists on `ManagedAgentsSpawner`, moves here), `Get(ctx, id) (Status, error)`, `List(ctx) ([]Summary, error)`, `Prune(ctx, PruneOpts) (*PruneReport, error)`, `Orphans(ctx, exclude func(key, agentID string) bool) ([]Orphan, error)`.
- F2. The MA pagination loop currently duplicated between `cmd/agents.go:listOrphanAgents` and `cmd/runs_prune.go:listAgentsMissingFromRuns` collapses into a single private method on the service. The predicate becomes a caller-supplied `exclude` callback.
- F3. The per-agent status worker pool in `cmd/agents.go:annotateAgentRows` moves into the service. Workers, channel, error classification all move; caller gets `[]Summary` back.
- F4. No `cmd/` file imports `github.com/anthropics/anthropic-sdk-go`, `anthropic-sdk-go/packages/param`, or `internal/machost` after migration. Clients build in `main()` / `root.go` and inject.
- F5. `cmd/agents.go` and `cmd/runs_prune.go` shrink to: parse flags → call service → format output. Formatting helpers (`formatCacheTime`, `printAgentRows`, `compactCell`, etc.) stay in `cmd/` — presentation is the CLI's job.
- F6. `EnsureAgent` (P1.3's body, currently on `pkg/spawner.ManagedAgentsSpawner`) moves to `agents.Service.EnsureAgent`. The spawner's `Spawn` method takes an `agents.Service` dependency and calls it. Spawner keeps session-lifecycle code (P1.4) — that is a different concern.

**Non-functional.**

- NF1. **Testability.** Service is unit-testable against an in-process `MAClient` fake and `memstore.MemStore`. No filesystem, no network, no real SDK.
- NF2. **Interface honesty.** The service depends on a local `MAClient` interface (the subset of the SDK it actually uses), not the concrete `*anthropic.Client`. Prevents SDK-version churn from rippling into service tests.
- NF3. **No new behavior.** This PR changes layering, not semantics. `orchestra agents ls` output is byte-identical before and after; likewise `agents prune`, `runs prune --reconcile`. Migration regression is a test failure, not a feature change.
- NF4. **One PR, reversible.** All three migrations (`agents`, `runs_prune`, `run_ma` spawner wiring) land together. If the PR is problematic, `git revert` restores today's layout.
- NF5. **No daemon/RPC prep.** `agents.Service` stays in-process. If hosted-orchestra ever happens, it wraps the service, not the other way round.

---

## 3. Scope

### In-scope

- New package `internal/agents/` with `agents.Service`, `Summary`, `Orphan`, `Status`, `PruneOpts`, `PruneReport`, local `MAClient` interface.
- Move `EnsureAgent` body from `pkg/spawner/managed_agents.go` (and helpers in `managed_agents_agent.go` / `managed_agents_cache.go` / `managed_agents_env.go` that are cache-policy rather than spawn-policy) into the service. Spawner keeps agent → session glue only.
- Collapse `cmd/agents.go:listOrphanAgents` and `cmd/runs_prune.go:listAgentsMissingFromRuns` into one `Service.Orphans(ctx, exclude)`.
- Collapse `cmd/agents.go:annotateAgentRows` worker pool into `Service.List(ctx)`.
- Rewire `cmd/agents.go`, `cmd/runs_prune.go`, `cmd/run_ma.go` and `pkg/spawner.ManagedAgentsSpawner` to consume the service.
- Unit tests per §7.
- Update `AGENTS.md` / `.claude/CLAUDE.md` project-structure section and `README.md` if it names `cmd/agents.go`.

### Out-of-scope

- **`envcache.Service`.** The environment-cache path is smaller and already has no `cmd/` caller. Defer until a second env-touching command appears, or fold into this PR only if the code naturally demands it.
- **Splitting `Store` by domain.** Same answer as `01-service-layer.md` §3: becomes a nit once services exist, not a blocker.
- **Removing `internal/machost`.** `machost` owns API-key resolution and client construction — that stays. `agents.Service` takes a constructed `*anthropic.Client` (or the `MAClient` interface) via constructor; it does not resolve keys itself.
- **Changing session-lifecycle code (P1.4).** `pkg/spawner/managed_agents_session.go` is untouched. It is legitimately a spawner concern (per-team session), not an agent-cache concern (user-scoped registry).
- **CLI surface changes.** No new commands, no flag renames. `agents ls`, `agents prune`, `runs prune`, `runs prune --reconcile` behave identically.
- **Error taxonomy.** Service wraps store/MA errors with context; does not introduce a new sentinel hierarchy.

---

## 4. Data model / API

No new persisted types. `agents.Summary`, `Orphan`, etc. are in-memory view types owned by the service.

### 4.1 `agents.Service`

```go
package agents

import (
    "context"
    "time"

    "github.com/anthropics/anthropic-sdk-go"
    "github.com/itsHabib/orchestra/internal/store"
)

// MAClient is the subset of the Anthropic Managed Agents surface the
// service depends on. Local interface so tests don't pull the full SDK.
type MAClient interface {
    GetAgent(ctx context.Context, id string) (*anthropic.BetaAgent, error)
    ListAgents(ctx context.Context, params anthropic.BetaAgentListParams) (*anthropic.BetaAgentListResponse, error)
    CreateAgent(ctx context.Context, params anthropic.BetaAgentNewParams) (*anthropic.BetaAgent, error)
    UpdateAgent(ctx context.Context, id string, params anthropic.BetaAgentUpdateParams) (*anthropic.BetaAgent, error)
    ArchiveAgent(ctx context.Context, id string) error
}

type Service struct {
    store   store.Store
    ma      MAClient
    clock   func() time.Time
    workers int
}

func New(s store.Store, ma MAClient, opts ...Option) *Service

// EnsureAgent returns a valid MA agent handle for the given spec,
// reusing the cached ID when spec-hash matches, updating on drift,
// adopting an existing MA agent when cache is cold, and creating only
// as a last resort. Holds WithAgentLock around the whole flow.
// (Migrated verbatim from ManagedAgentsSpawner.ensureAgent.)
func (s *Service) EnsureAgent(ctx context.Context, spec AgentSpec) (Handle, error)

// Get returns the live MA status of a cache record's agent: "active",
// "archived", "missing" (404), or "unreachable" (other error).
func (s *Service) Get(ctx context.Context, agentID string) (Status, error)

// List returns every cache record annotated with its live MA status.
// Runs Get concurrently across records (worker pool).
func (s *Service) List(ctx context.Context) ([]Summary, error)

// Prune evaluates staleness (missing, archived, or LastUsed < now-MaxAge)
// and, when opts.Apply is true, deletes the cache records. Does not
// archive the MA agents — see §5.3.
func (s *Service) Prune(ctx context.Context, opts PruneOpts) (*PruneReport, error)

// Orphans returns every orchestra-tagged MA agent not captured by
// exclude. Callers supply the predicate: cmd/agents passes a cache-key
// membership check; cmd/runs_prune passes a run-agent-ID check.
func (s *Service) Orphans(ctx context.Context, exclude func(key, agentID string) bool) ([]Orphan, error)
```

### 4.2 View types

```go
type Summary struct {
    Record store.AgentRecord
    Status Status
    Err    error // unreachable-class error, preserved for display
}

type Status string

const (
    StatusActive      Status = "active"
    StatusArchived    Status = "archived"
    StatusMissing     Status = "missing"     // MA 404
    StatusUnreachable Status = "unreachable" // other error
)

type Orphan struct {
    Key     string
    AgentID string
    Version int64
    Status  Status // "active" or "archived"
}

type PruneOpts struct {
    Apply    bool
    MaxAge   time.Duration
    Protect  func(key, agentID string) bool // optional; runs_prune uses this
                                            // to protect active-run agents
}

type PruneReport struct {
    Considered []store.AgentRecord
    Stale      []Summary // includes Reason via a helper
    Deleted    []string  // keys; empty when Apply == false
}
```

### 4.3 What the service does not do

- **Does not own printing.** `Summary`, `Orphan`, `PruneReport` are data. `cmd/agents.go` keeps `printAgentRows`, `printOrphanAgents`, etc. Presentation is the CLI's concern.
- **Does not own run-ID knowledge.** `Orphans` takes an `exclude` callback; the service does not know what a run is. `cmd/runs_prune.go` builds the exclude closure from `runAgentRefs`.
- **Does not depend on `run.Service`.** Services are leaves (see `01-service-layer.md` §5.2). Any composition ("prune caches after ending a run") lives in `cmd/` or a higher-level engine type.
- **Does not wrap `Store` method-for-method.** Callers that want raw cache records use `Store` directly — the service does not expose `ListAgents` as a passthrough.

---

## 5. Engineering decisions

### 5.1 Service boundary matches the domain, not the command

The natural slicing is not "one service per command" (`agentscli.Service`, `runsprunecli.Service`) but "one service per domain." Agents + agent cache are one domain; they share state, MA client, lock policy. That's the service. `cmd/agents.go` and `cmd/runs_prune.go` are two presentations of the same domain and should share the same service — which is precisely why the current code duplicates so much.

### 5.2 `Orphans(exclude)` instead of two methods

`listOrphanAgents(records)` (cache-key based) and `listAgentsMissingFromRuns(refs)` (run-agent-ID based) look different but are the same operation: "paginate MA, filter by orchestra metadata, subtract the caller's known set." Taking an `exclude func(key, agentID string) bool` callback unifies them. The callers own their own notion of "known."

Alternative considered: two methods, `OrphansByCacheKey` and `OrphansByRunRefs`. Rejected — the shared body is ~40 lines; duplicating it in the service just moves the duplication one layer down.

### 5.3 `Prune` deletes cache records only, not MA agents

Matches today's behavior (`cmd/runs_prune.go` comment: "intentionally deletes only local stale cache records... cache records are reusable across runs and do not yet carry a safe run-ownership boundary"). The service preserves that policy. A future `PruneOpts.ArchiveMA bool` knob can add MA-side archive when the ownership boundary is designed; not in this PR.

### 5.4 `EnsureAgent` moves off the spawner

P1.3 landed `EnsureAgent` as a method on `ManagedAgentsSpawner`. That was the right place at the time — no service layer existed for it. Now there is one, and keeping it on the spawner makes the spawner import the MA client's full caching surface. Move it.

Spawner post-migration:

```go
type ManagedAgentsSpawner struct {
    agents *agents.Service
    // ...session lifecycle fields unchanged
}

func (m *ManagedAgentsSpawner) Spawn(ctx, spec, ...) (Session, error) {
    handle, err := m.agents.EnsureAgent(ctx, spec)
    if err != nil { return nil, err }
    // proceed to session.New with handle.AgentID
}
```

No new invariant, just a shorter spawner.

### 5.5 Local `MAClient` interface vs concrete `*anthropic.Client`

Local interface. Reasons:

1. Unit tests get a fake without pulling `httptest` or mocking the SDK transport layer.
2. SDK version bumps touch one file (`agents/ma_adapter.go`) instead of every test.
3. The interface is narrow enough (5 methods) to be cheap; wider surfaces would argue against this.

The adapter is a thin `type sdkClient struct { *anthropic.Client }` with method signatures matching `MAClient`. Five lines.

### 5.6 Worker pool stays a service concern

`annotateAgentRows` currently runs 5 concurrent `Beta.Agents.Get` calls in `cmd/agents.go`. That's a performance optimization for the `ls` UX, not a CLI concern. Moves to the service, configurable via `WithWorkers(n)` option. Default 5 (preserves today's behavior).

### 5.7 No feature flag / staged rollout

The migration is behavior-preserving; feature-flagging adds a code path without adding value. Ship the refactor; if output diverges, that's the regression test firing, not a flag to toggle.

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Service owns MA SDK imports | (a) Keep SDK in `cmd/`, inject concrete client through a `Storer` interface. (b) Stick SDK in `pkg/spawner` and let `cmd/` talk to spawner. | (a) doesn't solve the problem — SDK is still in `cmd/`. (b) conflates spawn-policy (session) with cache-policy (agents); that's how we got here. |
| Single `Orphans(exclude)` method | Two methods, one per caller. | The shared body is the bulk of the method. Deduplicating it is the point. |
| `EnsureAgent` off the spawner | Leave it; add service for read/list paths only. | Splits cache policy across two packages. If the service owns `List`/`Prune`, it should own `Ensure` too. |
| Presentation stays in `cmd/` | Service returns rendered strings or `io.Writer`-style methods. | Services return data; CLIs render. Mixing the two is how cmd/ ended up owning both. |
| One PR for all migration | Three PRs (agents.go, runs_prune.go, run_ma.go + spawner). | The refactor is atomic at the boundary: half-migrating means two packages share the same SDK imports temporarily, worse than the status quo for reviewer cost. |
| Local `MAClient` interface | Take `*anthropic.Client` concretely. | SDK testing is painful without an interface; a 5-method surface is cheap. |
| Defer `envcache.Service` | Do both agent + env services together. | Env has zero `cmd/` callers. The payoff is concentrated on agents. Don't bundle speculative scope. |

---

## 7. Testing

### 7.1 Service unit tests (`internal/agents/service_test.go`)

All tests use `memstore.MemStore` + a `fakeMAClient` implementing the 5-method `MAClient` interface.

**`EnsureAgent` (ported from existing `pkg/spawner` tests).**

- Warm cache + matching spec-hash: one Get (archived check), zero Create/Update, `LastUsed` touched.
- Warm cache + spec-hash drift: Update called, cache record updated, `LastUsed` touched.
- Cold cache + existing MA agent with matching name: List + adopt (PutAgent), no Create.
- Cold cache + no MA agent: Create, PutAgent, `LastUsed` touched.
- Concurrent `EnsureAgent` on same key: serialized via `WithAgentLock`, exactly one Create.

**`Get`.**

- Active agent → `StatusActive`, nil err.
- Archived agent → `StatusArchived`, nil err.
- 404 → `StatusMissing`, nil err (not an error).
- Transport error → `StatusUnreachable`, err preserved.

**`List`.**

- N records, M workers: all records annotated, worker count honored.
- Mixed statuses: returned `[]Summary` ordering matches record order (for stable CLI output).

**`Prune`.**

- Dry-run: no `DeleteAgent`, report lists expected stale records with reasons.
- Apply: `DeleteAgent` called per stale record; missing-record errors (`store.ErrNotFound`) ignored.
- `Protect` callback: protected records never appear in `Stale`.
- `MaxAge == 0` with all records missing/archived: still prunes (status-based, not age-based).

**`Orphans`.**

- Empty cache, MA has orchestra-tagged agents → all returned.
- `exclude` returns true for half → only the other half returned.
- Pagination: two pages of MA results → both traversed, sorted output.

### 7.2 Migration regression

- `cmd/agents_test.go` / `cmd/runs_test.go` (existing) pass unchanged. They exercise the CLI surface; the surface doesn't change.
- Byte-for-byte output comparison on `agents ls` / `agents prune` / `runs prune --reconcile` against pre-migration golden captures. If output drifts, the PR is wrong.

### 7.3 What we explicitly do not test

- Real `*anthropic.Client` behavior. Integration tests (if any) live alongside P1.3/P1.4, not here.
- Store primitives. `storetest` owns those.
- Rendering. CLI tests cover that.

---

## 8. Rollout

Single PR, three mechanical moves + one consolidation:

1. **New package `internal/agents/`.** Service type, view types, `MAClient` interface, `sdkClient` adapter.
2. **Move `EnsureAgent` off the spawner.** Cut from `pkg/spawner/managed_agents_agent.go` (and cache/env helpers that are policy-level, not spawn-level). Spawner takes `*agents.Service` via constructor; `Spawn` delegates.
3. **Consolidate `cmd/agents.go` and `cmd/runs_prune.go` orphan loops.** Both call `svc.Orphans(exclude)` with different `exclude` closures.
4. **Rewire `cmd/run_ma.go` / `main.go` / `cmd/root.go`.** Construct `agents.Service` once at startup; pass to spawner and to the `runs`/`debug agents` command groups.

Drop imports from `cmd/`: `github.com/anthropics/anthropic-sdk-go`, `anthropic-sdk-go/packages/param`, `internal/machost` (except in the single place `agents.Service` is constructed — likely `cmd/root.go` or a `newAgentsService` helper).

Estimated churn:

- `cmd/agents.go`: 347 → ~130 lines (keep flag/cobra/format, drop SDK + pagination + workers).
- `cmd/runs_prune.go`: 159 → ~60 lines.
- `internal/agents/service.go`: new, ~400 lines (EnsureAgent body is the bulk).
- `pkg/spawner/managed_agents*.go`: net reduction (~200 lines move out).

**Rollback.** Single-PR revert restores today's layout; no schema changes, no persisted-format changes.

---

## 9. Observability & error handling

- Service wraps errors with method context: `fmt.Errorf("agents.Prune: %w", err)`. Store and MA sentinels remain reachable via `errors.Is`.
- `Status` enum is an error-classification discriminator, not an error itself. `Get` returns `(StatusUnreachable, err)` when the MA call failed; the CLI decides whether to show it.
- DEBUG-level `slog` on method entry/exit with elapsed-ms. Consistent with `run.Service`.
- No metrics in this PR.

---

## 10. Open questions

1. **Package name — `internal/agents/`, `internal/agentcache/`, `internal/mahost/`?** `01-service-layer.md` §10.4 argued for the least-interesting name. `agents` is shortest, matches the `agents` CLI noun, and leaves room for `sessions` / `envs` services at the same layer. Weak preference for `internal/agents`.
2. **Does `EnsureAgent` need to stay exported from spawner for backward compatibility?** No. No external consumer; single-module repo. Move it cleanly.
3. **Should `Prune` grow an `ArchiveMA` option now, for symmetry with the MA side?** No — defer until the run-ownership boundary is designed. Current behavior (local-only) is the safe default.
4. **Where is `agents.Service` constructed in `main`?** Natural spot is `cmd/root.go` alongside the logger (same pattern as `run.Service`). Subcommands get it via package-level singleton or explicit passing. Prefer explicit passing; avoid the singleton.
5. **Does this subsume `internal/machost`?** No. `machost` is API-key-resolution + SDK-client-construction. `agents.Service` depends on a constructed client. Two packages with distinct jobs.

---

## 11. Acceptance criteria

- [ ] `internal/agents/` package exists with `Service`, view types, `MAClient` interface, `sdkClient` adapter.
- [ ] `grep -r "anthropic-sdk-go" cmd/` returns zero results (migration complete).
- [ ] `cmd/agents.go` and `cmd/runs_prune.go` do not import `internal/machost`, `pkg/store/filestore`, or `anthropic-sdk-go`.
- [ ] `pkg/spawner.ManagedAgentsSpawner.Spawn` calls `agents.Service.EnsureAgent` instead of hosting the cache logic itself.
- [ ] `Orphans(exclude)` is the single orphan-pagination method; `listOrphanAgents` and `listAgentsMissingFromRuns` are gone.
- [ ] `agents ls`, `agents prune [--apply]`, `runs prune --reconcile` produce byte-identical output vs. pre-migration.
- [ ] `go test ./...`, `go vet ./...`, `make lint` green.
- [ ] Unit tests per §7.1 land alongside the service.
- [ ] Follow-up tracked (not shipped): `envcache.Service` — revisit when a second env-touching command appears.
