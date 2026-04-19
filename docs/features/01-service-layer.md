# Feature: Service layer

Status: **Shipped** (PR #4, merged to main as 028d1cc)
Owner: @itsHabib
Depends on: [00-store-interface.md](./00-store-interface.md) (shipped in PR #2)
Relates to: [p13-registry-cache.md](./p13-registry-cache.md); [DESIGN-v2.md](../DESIGN-v2.md) §10 (state & resumption), §9.1 (agent cache choreography).
Target: lands after P1.3 (registry cache) so the agent-cache choreography that service layer owns is real code, not hypothetical.

---

## 1. Overview

`pkg/store.Store` is correctly scoped as a dumb CRUD primitive — its methods name persistence verbs (`LoadRunState`, `PutAgent`, `AcquireRunLock`), not domain operations. That's the right shape for a data-access layer. But it means callers have to compose those primitives themselves to express the actual domain intent:

- `cmd/run.go` hand-writes the run-lifecycle choreography: acquire exclusive run lock → archive prior run → seed new state → mark teams running → mark teams complete → release lock. ~60 lines of orchestration glue stapled to Cobra command setup.
- `cmd/spawn.go` reimplements a subset: acquire (now shared) run lock → read state → build prompt. Different enough from `run` to diverge, similar enough that a change to locking semantics has to be made in both.
- P1.3 will add `EnsureAgent` — acquire per-key lock → `GetAgent` → spec-hash compare → `Beta.Agents.Update` or `Beta.Agents.List`+adopt or `Beta.Agents.New` → `PutAgent` → touch `LastUsed`. That choreography has nowhere natural to live; if we don't give it a home, it lands as a method on `ManagedAgentsSpawner` and the pattern mutates every time a new caller needs a variant.

Three callers already, and the Store has 15 methods. Each new caller is a fresh opportunity to forget an invariant (release the lock, touch `LastUsed`, check the archived flag). The fix is not to shrink `Store`. It is to introduce a **service layer** above it: narrow domain types that own the choreography and take `Store` as a dependency. Callers talk to services; services talk to Store; Store talks to bytes.

This doc proposes three services: `run.Service`, `agentcache.Service`, `envcache.Service`. They replace direct `Store` usage in `cmd/` and absorb the choreography currently in `cmd/run.go`, `cmd/spawn.go`, and (after P1.3 lands) `pkg/spawner/managedagents/`.

---

## 2. Requirements

**Functional.**

- F1. `run.Service` owns run-lifecycle choreography: `Begin`, `RecordTeamStart`, `RecordTeamComplete`, `RecordTeamFail`, `Snapshot`, `End`. `Begin` takes the run lock + archives + seeds new state atomically from the caller's perspective. `End` releases the lock.
- F2. `agentcache.Service` owns the `EnsureAgent` / `List` / `Prune` choreography from P1.3. `envcache.Service` is the parallel type for environments.
- F3. No `cmd/` package imports `pkg/store` after migration. `cmd/` constructs services in `main()` (already where `olog.New()` lives) and calls service methods.
- F4. Services are the only code that composes `Store` primitives. Store itself gains no new methods.
- F5. Each service is a concrete type with its `Store` dependency injected via constructor. No service interfaces until a second implementation exists — don't abstract the abstraction.

**Non-functional.**

- NF1. **Testability.** Each service is unit-testable against `memstore.MemStore` with no filesystem access. Engine-level smoke/e2e tests continue to pass unchanged.
- NF2. **Concurrency safety.** Services are safe for concurrent use within a single process. Cross-process safety is delegated to `Store` (`AcquireRunLock`, `WithAgentLock`). Services add no new locking primitives.
- NF3. **No service-level state.** Services hold a `Store`, a clock, and impl-specific clients (e.g. `MAClient` in `agentcache`). No caching, no state machine state, no lifecycle hooks beyond constructor/method calls. If it looks like state, it belongs in `Store`.
- NF4. **Error classification is additive.** Services wrap store sentinels via `fmt.Errorf("%w: ...", store.ErrX)`. They do not swallow, rewrap, or reclassify store errors — callers `errors.Is` against store sentinels through the service boundary.
- NF5. **Rollback-ability.** The migration is one PR per service. Each PR is `git revert`-clean.

---

## 3. Scope

### In-scope

- `internal/run/` (or `pkg/run/`, TBD in §10.1) — `run.Service` with the methods listed in §4.1.
- `internal/agentcache/` — `agentcache.Service` absorbing P1.3's `EnsureAgent` body.
- `internal/envcache/` — `envcache.Service`, mirror of `agentcache` for environments.
- Migration of `cmd/run.go`, `cmd/spawn.go`, `cmd/status.go`, `cmd/init_cmd.go` to use services.
- Retirement of `internal/workspace/` — its run-state surface moves to `run.Service`; its registry/result/log/message helpers either move to a sibling service or stay as standalone package-level helpers (§5.5).
- Unit tests per service against `memstore`.
- Updated `AGENTS.md` / `CLAUDE.md` project-structure section.

### Out-of-scope

- **Splitting the `Store` interface into `RunStateStore` / `AgentStore` / `EnvStore`.** Becomes optional once services exist: each service already depends on exactly one domain's methods, so the interface size is a documentation nit, not an architecture problem. If a reviewer insists, it's a five-minute follow-up.
- **Generic `Registry[T any]` abstraction.** Reduces Store duplication but multiplies call-site churn — see 00-store-interface.md §6. Revisit only if a third cached-resource type (secrets, cached runs, whatever) appears.
- **Daemon mode / RPC boundary.** Nothing about services as designed here prescribes a transport. A future hosted impl adds a gRPC shim in front of the services; service types stay in-process.
- **Replacing `spawner.Spawn` or `spawner.SpawnBackground`.** Spawners already have a clean boundary; they don't touch `Store` directly.
- **Message-bus refactor.** `internal/messaging` is already a thin file-backed bus and not a `Store` consumer; it doesn't need to become a service. Folds under `run.Service` lifecycle (bus init happens inside `Begin`) without needing its own type.
- **New CLI surface.** `orchestra agents ls` / `orchestra agents prune` ship with P1.3, not this doc.

---

## 4. Data model / API

No new persistent types. Services are stateless-apart-from-deps and do not add anything the Store persists.

### 4.1 `run.Service`

```go
package run

import (
    "context"
    "time"

    "github.com/itsHabib/orchestra/internal/config"
    "github.com/itsHabib/orchestra/internal/messaging"
    "github.com/itsHabib/orchestra/pkg/store"
)

// Service owns the run lifecycle: lock, archive, seed, team transitions,
// release. All state mutations go through Store; Service composes them
// into invariants.
type Service struct {
    store store.Store
    clock func() time.Time
}

func New(s store.Store) *Service { return &Service{store: s, clock: time.Now} }

// Active represents a live run holding the exclusive run lock. Release
// is called exactly once by End (or deferred by the caller on panic paths).
type Active struct {
    RunID   string
    State   *store.RunState
    Bus     *messaging.Bus
    release func()
}

// Begin acquires the exclusive run lock, archives any prior active run,
// seeds fresh state + registry, and initializes the message bus.
// Returns an Active holding the lock until End is called.
func (s *Service) Begin(ctx context.Context, cfg *config.Config) (*Active, error)

// Snapshot reads the current run state lock-free (atomic-rename reads).
// Safe to call from orchestra status without blocking an active run.
func (s *Service) Snapshot(ctx context.Context) (*store.RunState, error)

// RecordTeamStart transitions a team to "running" and stamps start time.
func (s *Service) RecordTeamStart(ctx context.Context, team string) error

// RecordTeamComplete transitions a team to "done" and records result counters.
func (s *Service) RecordTeamComplete(ctx context.Context, team string, result *TeamResult) error

// RecordTeamFail transitions a team to "failed" with the error summary.
func (s *Service) RecordTeamFail(ctx context.Context, team string, cause error) error

// End releases the run lock. Safe to call multiple times; subsequent
// calls are no-ops.
func (s *Service) End(a *Active) error
```

### 4.2 `agentcache.Service`

```go
package agentcache

import (
    "context"

    "github.com/itsHabib/orchestra/pkg/store"
)

// MAClient is the minimal surface agentcache needs from the Managed
// Agents API. Defined here so the service can be unit-tested against
// a fake, without pulling the full SDK into tests.
type MAClient interface {
    GetAgent(ctx context.Context, id string) (*Agent, error)
    ListAgents(ctx context.Context, opts ListOpts) ([]Agent, string, error)
    CreateAgent(ctx context.Context, spec AgentSpec) (*Agent, error)
    UpdateAgent(ctx context.Context, id string, expectedVersion int, spec AgentSpec) (*Agent, error)
}

type Service struct {
    store store.Store
    ma    MAClient
    hash  SpecHasher // canonical-form hasher per store-doc §4.4
    clock func() time.Time
}

// EnsureAgent returns a valid MA agent handle for the given spec,
// reusing the cached ID when spec-hash matches, updating on drift,
// adopting an existing MA agent when cache is cold, and creating only
// as a last resort. Holds WithAgentLock around the whole flow so
// concurrent EnsureAgent on the same key is safe across processes.
func (s *Service) EnsureAgent(ctx context.Context, spec AgentSpec) (AgentHandle, error)

// List returns all cached agent records sorted by key. Touches nothing.
func (s *Service) List(ctx context.Context) ([]store.AgentRecord, error)

// Prune deletes cache records with LastUsed older than opts.MaxAge and,
// when opts.Apply is true, archives the corresponding MA agents.
func (s *Service) Prune(ctx context.Context, opts PruneOpts) (PruneReport, error)
```

`envcache.Service` has the same shape with `EnvRecord` / `EnsureEnvironment` / env-specific MA client surface.

### 4.3 What services do *not* do

- **Do not wrap Store method-for-method.** `run.Service` does not expose `LoadRunState` or `UpdateTeamState`. Callers need a domain operation (`RecordTeamComplete`), not a persistence verb. If a consumer wants raw state access for read-only reporting, it calls `Snapshot` or uses `Store` directly — the latter explicitly allowed for read-only diagnostics.
- **Do not own concurrency primitives.** `run.Service.Begin` holds a lock by delegating to `Store.AcquireRunLock`. It does not introduce a mutex, semaphore, or queue of its own. If an invariant needs a new primitive, that primitive belongs in Store.
- **Do not coordinate across services.** `run.Service` does not call `agentcache.Service`. If a flow needs both (e.g. "start a run and prewarm the agent cache"), that composition lives in `cmd/` or in a higher-level `engine` type — not in the services themselves. Services are leaves.

---

## 5. Engineering decisions

### 5.1 Services instead of a fatter `Workspace`

`internal/workspace.Workspace` today mixes run-state, team registry, per-team result files, log writers, message-bus path, and (since 00-store-interface) Store delegation. It is already a god-type; we're not making it worse. Splitting into focused services is the cleanup. `Workspace` either disappears, or shrinks to the helpers that legitimately share a lifetime (result files, log writers) and gets renamed.

### 5.2 Concrete services, not interfaces

`run.Service` is `type Service struct { ... }`, not `type Service interface { ... }`. Reasons:

1. **One implementation expected.** Service interfaces exist to support multiple backends. The backend pluralism lives at the `Store` layer; services compose one Store.
2. **Mock-for-mocking is a smell.** If a caller wants to unit-test against a fake `run.Service`, it means that caller has its own logic worth testing without driving a real run — i.e. that caller should itself be a service, and we haven't identified it yet.
3. **Constructor injection is enough.** Tests build `run.New(memstore.New())` directly. No DI framework, no interface-per-dep.

If a second implementation ever emerges (a hosted-orchestra remote `run.Service`?), extract an interface then. Don't front-load the flexibility.

### 5.3 Invariants move out of `cmd/`

Right now `cmd/run.go:49-57` expresses a critical invariant — "hold the run lock across archive + seed" — as adjacent Go statements. If a future contributor inlines `runOrchestration`, reorders, or short-circuits on an error path, the invariant quietly breaks. Moving this into `run.Service.Begin` makes the invariant a type-level property: if you have an `*Active`, the lock is held; if you don't, it isn't. `cmd/run.go` shrinks to "parse config, call Begin, run tiers, call End."

This is the real payoff. The interface-is-big complaint is a symptom; the disease is that invariants live in the glue instead of in types.

### 5.4 Composition order — `run` first, `agentcache`/`envcache` after P1.3

`run.Service` is a refactor of code that exists today. Low-risk, immediate payoff in `cmd/run.go` readability, validates the pattern before P1.3 commits to it. Ships standalone.

`agentcache` / `envcache` services wrap code P1.3 has not yet written. Forcing P1.3 to land *as* a service in its initial PR means the P1.3 reviewer is evaluating two concerns at once (cache choreography correctness + service-layer shape). Cheaper to let P1.3 land as a set of methods on `ManagedAgentsSpawner`, then refactor into `agentcache.Service` as a follow-up — the lift is purely "extract struct."

The order is therefore: this doc → `run.Service` PR → P1.3 lands → `agentcache.Service` extract PR → `envcache.Service` extract PR.

### 5.5 What happens to `internal/workspace/`

Three categories of current `Workspace` responsibilities, routed three ways:

- **Run state + registry + archive + lock** → `run.Service`. This is ~80% of the current Workspace surface by line count.
- **`ReadResult` / `WriteResult` / `LogWriter`** → move to `internal/workspace/results.go` and `internal/workspace/logs.go` as package-level helpers. They are pure I/O helpers with no lifecycle; they don't need a service.
- **`MessagesPath`** → the message bus gets its path directly from config or from `run.Active.Bus`. The helper disappears.

`internal/workspace/` as a package probably survives but shrinks to ~50 lines of file-location helpers. If that's small enough to fold elsewhere, do that in the same PR; if not, leave it.

### 5.6 Error propagation

Services wrap store sentinels when adding context, never replace them:

```go
// in run.Service.Snapshot
state, err := s.store.LoadRunState(ctx)
if err != nil {
    if errors.Is(err, store.ErrNotFound) {
        return nil, err // bubble up untouched — callers discriminate on ErrNotFound
    }
    return nil, fmt.Errorf("load run state: %w", err)
}
```

Callers continue to use `errors.Is(err, store.ErrNotFound)` across the service boundary. No `run.ErrNoActiveRun` alias — that's just renaming `ErrNotFound` for the sake of it.

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Services as concrete types | (a) Interfaces with a single impl each. (b) Add interfaces only where a test fake is needed. | (a) premature abstraction. (b) is what we end up at if a test ever needs a fake service — defer until then. |
| One service per domain (`run` / `agentcache` / `envcache`) | (a) One `Workspace` service that does all three. (b) One service per CLI command. | (a) is the god-type we're leaving. (b) multiplies services and makes cross-command reuse (status reading run state, spawn reading run state) awkward. Domain is the right axis. |
| Services do not depend on each other | (a) `run.Service` calls `agentcache.Service` during `Begin` to prewarm. | Keeps services leaves; composition is the caller's job. If a real flow needs prewarm, it's a few lines in `cmd/run.go`, not a hidden dependency edge. |
| Services own choreography, not caching | (a) Service-level LRU for agent records. | The Store-doc §3 already forbids read-through caching at the Store layer; pushing it one layer up has the same problem (staleness, invalidation). MA already backs the source of truth; cache records are already the cache. |
| Migrate `run.Service` first, `agentcache` after P1.3 | (a) Land all three services in one PR. (b) Do `agentcache` first because its choreography is gnarlier. | (a) bundles a speculative refactor with real new code. (b) forces P1.3 into a pattern during initial development, increasing review complexity. Incremental is cheaper. |
| Concrete `MAClient` interface inside `agentcache` | (a) Inject the full SDK client. (b) Share one `MAClient` interface across agentcache + envcache. | (a) pulls SDK into unit tests. (b) merges two halves that happen to have similar shape now but will drift (env archive is a different endpoint). Local interface per service is honest. |

---

## 7. Testing

### 7.1 Service unit tests

Each service package ships `*_test.go` alongside. All tests use `memstore.MemStore` as the `Store` dependency — no `t.TempDir()`, no filesystem.

**`run.Service` test surface.**

- `Begin` on a fresh store: seeds state, returns an `*Active`, holds the lock (proved by a second `Begin` timing out against `ErrLockTimeout`).
- `Begin` after a prior run: archives the prior state (observable via `store.ArchiveRun` call count on a shim) before seeding.
- `RecordTeamStart` then `RecordTeamComplete`: team transitions through expected states, counters accumulate.
- `RecordTeamFail` records the cause without clobbering unrelated teams.
- `End` releases the lock, subsequent `Begin` succeeds.
- Concurrent `RecordTeamStart` / `RecordTeamComplete` on different teams: all writes land (already covered by storetest, reproved here at service level).
- `Snapshot` reads lock-free while a `Record*` call is in flight: no deadlock.

**`agentcache.Service` test surface.**

- `EnsureAgent` warm cache + matching spec-hash: zero `Create`/`Update` calls, one `Get` (archived check), `LastUsed` touched.
- `EnsureAgent` warm cache + spec-hash drift: `Update` called, cache record updated, `LastUsed` touched.
- `EnsureAgent` cold cache + existing MA agent with matching name: `List`+adopt, `PutAgent`.
- `EnsureAgent` cold cache + no MA agent: `Create`, `PutAgent`.
- `EnsureAgent` concurrent calls same key: serialized via `WithAgentLock`, exactly one `Create` (not N).
- `Prune` dry-run: no `DeleteAgent`, no MA archive calls; report matches expected keys.
- `Prune` apply: `DeleteAgent` called per record, MA archive called per record.

**`envcache.Service`** parallels the agent cases.

### 7.2 Migration regression

Existing `smoke_test.go` / `e2e_test.go` pass unchanged after the `cmd/` migration. If they drift, that is the regression signal — we reverse the migration and investigate before continuing.

### 7.3 What we explicitly do not test

- Store-level invariants (atomic writes, flock behavior, archive semantics). Those belong to `storetest`; re-testing them at the service layer is redundant.
- MA SDK behavior. Services use a fake `MAClient` in tests; the real SDK is covered by P1.3's integration tests, not service tests.

---

## 8. Rollout / Migration

Three sequential PRs. Each independently mergeable and revertible.

**PR A — `run.Service` + `cmd/` migration.**

1. New package `internal/run/` with `Service`, `Active`, `TeamResult`.
2. `cmd/run.go`, `cmd/spawn.go`, `cmd/status.go`, `cmd/init_cmd.go` migrated. Post-migration, these files do not import `pkg/store` or construct `filestore.FileStore` directly.
3. `internal/workspace/` shrinks per §5.5. The file-helper residue moves to `internal/workspace/{results,logs}.go` or disappears.
4. Unit tests per §7.1 for `run.Service`.
5. `smoke_test.go` / `e2e_test.go` pass unchanged.

Before/after line counts recorded in the PR description: `cmd/run.go` expected to drop from ~490 lines to ~250.

**PR B — `agentcache.Service` extract from P1.3.**

Depends on P1.3 landing. Pure refactor: moves `ManagedAgentsSpawner.EnsureAgent` body into `agentcache.Service.EnsureAgent`, introduces `MAClient` local interface, rewires the spawner to call the service. Test suite ports over.

**PR C — `envcache.Service` extract.**

Mirror of PR B for environments.

**Ordering guardrail.** PR B does not merge before P1.3. PR C does not merge before PR B (shared MAClient interface patterns).

**Rollback.** Each PR is `git revert`-clean. PR A is the largest blast radius; if it proves unstable, the revert restores `internal/workspace/` and direct Store usage in `cmd/`.

---

## 9. Observability & error handling

- Services log at `slog` DEBUG on method entry with elapsed-ms on exit. Same cheap-and-useful tracing policy as Store (00-store-interface.md §9).
- Service errors wrap their cause: `fmt.Errorf("run.Begin: %w", err)`. Store sentinels stay reachable via `errors.Is`.
- No service-level metrics in this feature. Same reasoning as Store: add them when orchestra-in-prod needs them, not as a speculative plumbing exercise.
- Service construction in `main()` is the natural place to wire a shared `slog.Logger` into every service. Defer the structured-logging pass until at least one service is in production use.

---

## 10. Open questions

1. **Package location — `internal/run/` vs `pkg/run/`.** Services are consumed only by `cmd/`, which lives in the same module. `internal/` is the conservative default (no external import, free to rename). `pkg/` is the right choice only if a hosted-orchestra binary outside this module ever imports the service. Lean `internal/` until evidence says otherwise.
2. **What disappears of `internal/workspace/`.** §5.5 is a sketch. The real answer is visible only after `run.Service` is written and we see what genuinely doesn't fit. Revisit during PR A, not now.
3. **Does the message bus belong inside `run.Active`?** Currently proposed yes (bus lifecycle == run lifecycle). But `orchestra msg` is a long-running non-run operation that also wants the bus. If that surfaces, the bus becomes its own concern with its own lifetime and `run.Active` stops owning it.
4. **Naming — `run.Service` vs `run.Coordinator` vs `engine.Run`.** `Service` is the least interesting name and therefore the most honest one. DESIGN-v2 uses "engine" for the DAG executor; we'd be colliding vocabulary. Pick `Service` unless a reviewer argues otherwise.
5. **Do agent cache methods (`List`, `Prune`) warrant their own service, separate from `EnsureAgent`?** Splitting by operation class (mutation vs read-only) has no real benefit here; they share state and a dependency. Kept in one service until proven otherwise.

---

## 11. Acceptance criteria

- [ ] `internal/run/` package exists with `run.Service`, `run.Active`, unit tests against `memstore`.
- [ ] `cmd/run.go`, `cmd/spawn.go`, `cmd/status.go`, `cmd/init_cmd.go` no longer import `pkg/store` or `pkg/store/filestore`.
- [ ] `internal/workspace/` is reduced per §5.5 (or eliminated).
- [ ] `smoke_test.go` / `e2e_test.go` pass unchanged.
- [ ] `cmd/run.go` line count reduced by ≥40% (choreography moved out).
- [ ] `make test && make vet && make lint` green.
- [ ] Follow-up tasks tracked (not shipped): `agentcache.Service` extract (depends on P1.3), `envcache.Service` extract (depends on `agentcache`).
