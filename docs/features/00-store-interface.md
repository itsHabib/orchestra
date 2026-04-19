# Feature: Store interface

Status: **Proposed**
Owner: @itsHabib
Target: lands before P1.3 (registry cache) so P1.3 codes against the interface from day one.
Relates to: [DESIGN-v2.md](../DESIGN-v2.md) §10 (state & resumption), §9.1 (agents.json cache).

---

## 1. Overview

Orchestra today hard-codes its persistence to the local filesystem. Every read and write of `state.json`, `registry.json`, agent-cache records, and team results goes through direct `os.ReadFile` / `fsutil.AtomicWrite` calls in `internal/workspace/workspace.go`. Fine for a laptop CLI — but it means:

1. **Tests touch disk.** Every workspace-exercising test spins up a real temp directory, reads/writes real files. Slower, more fragile, harder to simulate failure modes.
2. **No substitute backends.** If someone wants to run orchestra against a shared store (SQL, object store, a small HTTP service for multi-worker), every callsite has to be rewritten.
3. **The mutator pattern lives in one concrete type.** `Workspace.UpdateTeamState(team, fn)` (on which DESIGN-v2.md §10.2 depends for correctness) is a method on the filesystem struct. A second impl has to reinvent it.

We want orchestra to be usable in production by people who are not running it out of `~/pers/orchestra` on a laptop. That eventually means a real store. Introducing an interface now — *before* P1.3 adds a pile of new code written against the filesystem — is cheap insurance. Retrofitting later is not.

---

## 2. Requirements

**Functional.**

- F1. Expose a `Store` interface that fully covers read/write access to: run state (per-workspace), agent registry (per-user), environment registry (per-user), run archival.
- F2. Ship one concrete impl: `FileStore`, backed by the existing `.orchestra/` and `~/.config/orchestra/` layout.
- F3. Ship an in-memory impl: `MemStore`, for unit tests.
- F4. Provide a shared conformance suite (`storetest`) that both impls pass and any future impl must pass.
- F5. Preserve existing `Workspace` public method surface where it is still used by `cmd/` callers (thin forwarders OK); no callsite has to change purely because of the refactor.

**Non-functional.**

- NF1. **Atomic per-document writes.** Partial writes must never be visible to readers. FileStore uses `.tmp` + `os.Rename`.
- NF2. **In-process serialization of read-modify-write on run state.** Per DESIGN-v2.md §10.2, every team-state update funnels through a single mutex via the mutator-closure pattern.
- NF3. **Cross-process safety.** Enforced for (a) one active run per workspace (`run.lock`) and (b) read-modify-write on the user-scoped registry (per-key flock).
- NF4. **Determinism of `AgentRecord.SpecHash`.** Hashes computed on Linux, macOS, Windows from the same input produce the same output. A cache entry written on one platform reads correctly on another.
- NF5. **Rollback-ability.** The entire feature is one PR's worth of change; `git revert` cleanly reverts to v1 behavior.

---

## 3. Scope

### In-scope

- Interface and types for run state and user-scoped registries.
- `FileStore` impl matching today's on-disk layout.
- `MemStore` impl for tests.
- Conformance suite.
- Run-lock (`.orchestra/run.lock`) enforcing one active run per workspace.
- Refactor of `internal/workspace/` to sit behind the interface.
- Migration of call sites in `cmd/` and skills that parse `state.json` directly.

### Out-of-scope

Stated as explicit guardrails because this interface will tempt scope creep:

- **Not a service layer.** Persistence abstraction, not a hosted orchestra daemon. Auth, session ownership, multi-tenancy, worker fanout, observability pipelines are all separate features.
- **Not a database port.** No SQL/KV/Dynamo impl in this feature. The interface must be small enough that one is feasible later, but not so opinionated that it prescribes schema.
- **Not a transaction manager.** Per-document atomicity (the mutator closure). No cross-document transactions. `state.json` and `agents.json` are independent.
- **Not a log or results sink.** Append-only NDJSON event logs and `results/<team>/summary.md` stay outside the interface. See §5.4 for why and what the future read-side story looks like.
- **Not a cache layer.** Callers read through, not around. If a concrete impl wants to cache, that's the impl's concern.

"We have a Store interface" is not the same as "orchestra is prod-ready." It is *one* of several prerequisites; the others — observability, rate-limit accounting, auth, multi-tenancy, scale testing — are separate.

---

## 4. Data model

This doc is not about backwards-compat with v1's `state.json` shape. That shape changes; the migrating PR updates any reader (`cmd/status.go`, `/orchestra-monitor` skill, user `jq` recipes baked into docs) in lockstep.

### 4.1 Run state (per-workspace)

```go
type RunState struct {
    Project       string               `json:"project"`
    Backend       string               `json:"backend"`        // "local" | "managed_agents"
    RunID         string               `json:"run_id"`
    StartedAt     time.Time            `json:"started_at"`
    EnvironmentID string               `json:"environment_id,omitempty"`
    Teams         map[string]TeamState `json:"teams"`
}
// NOTE: `Backend` is a dispatch hint for `orchestra resume`. Not a
// multi-tenant discriminator, not a plugin key. The Store does not branch
// on it; the engine does, once, during resume.

type TeamState struct {
    Status     string    `json:"status"`               // pending|running|idle|completed|failed|stalled|terminated
    StartedAt  time.Time `json:"started_at,omitempty"`
    EndedAt    time.Time `json:"ended_at,omitempty"`
    DurationMs int64     `json:"duration_ms,omitempty"`

    // Shared across backends.
    SessionID     string `json:"session_id,omitempty"`
    LastError     string `json:"last_error,omitempty"`
    ResultSummary string `json:"result_summary,omitempty"`

    // Local-backend only.
    PID int `json:"pid,omitempty"`

    // MA-backend only.
    AgentID             string               `json:"agent_id,omitempty"`
    AgentVersion        int                  `json:"agent_version,omitempty"`
    LastEventID         string               `json:"last_event_id,omitempty"`
    LastEventAt         time.Time            `json:"last_event_at,omitempty"`
    RepositoryArtifacts []RepositoryArtifact `json:"repository_artifacts,omitempty"`

    // Usage counters (both backends populate; MA populates more).
    CostUSD                  float64 `json:"cost_usd,omitempty"`
    InputTokens              int64   `json:"input_tokens,omitempty"`
    OutputTokens             int64   `json:"output_tokens,omitempty"`
    CacheCreationInputTokens int64   `json:"cache_creation_input_tokens,omitempty"`
    CacheReadInputTokens     int64   `json:"cache_read_input_tokens,omitempty"`
}

type RepositoryArtifact struct {
    URL            string `json:"url"`
    Branch         string `json:"branch"`
    BaseSHA        string `json:"base_sha"`
    CommitSHA      string `json:"commit_sha"`
    PullRequestURL string `json:"pull_request_url,omitempty"`
}
```

### 4.2 User-scoped registry (agents + envs)

```go
type AgentRecord struct {
    Key       string    `json:"key"`          // "<project>__<role>"
    Project   string    `json:"project"`
    Role      string    `json:"role"`
    AgentID   string    `json:"agent_id"`     // MA resource ID
    Version   int       `json:"version"`      // MA-side version (for Beta.Agents.Update)
    SpecHash  string    `json:"spec_hash"`    // canonical sha256; see §4.4
    UpdatedAt time.Time `json:"updated_at"`   // last write by orchestra
    LastUsed  time.Time `json:"last_used,omitempty"` // touched on cache hit; drives prune
}
// NOTE: `Version` is not an optimistic-concurrency token. Two PutAgent
// calls race under flock and last-write wins; do not add
// expectedPrevVersion without reopening this doc.

type EnvRecord struct {
    Key       string    `json:"key"`          // "<project>__<env-name>"
    Project   string    `json:"project"`
    Name      string    `json:"name"`
    EnvID     string    `json:"env_id"`
    SpecHash  string    `json:"spec_hash"`
    UpdatedAt time.Time `json:"updated_at"`
    LastUsed  time.Time `json:"last_used,omitempty"`
}
```

### 4.3 Interface

```go
package store

import "context"

// Store persists orchestra's run state and cross-run registries.
// Implementations must be safe for concurrent use within a single process.
// Cross-process safety is a per-impl concern; FileStore uses advisory locks.
type Store interface {
    // Run-scoped state (one document per `.orchestra/` workspace).
    LoadRunState(ctx context.Context) (*RunState, error)
    SaveRunState(ctx context.Context, s *RunState) error
    UpdateTeamState(ctx context.Context, team string, fn func(*TeamState)) error
    ArchiveRun(ctx context.Context, runID string) error

    // Run-lock (cross-process).
    AcquireRunLock(ctx context.Context, mode LockMode) (release func(), err error)

    // User-scoped agent cache.
    GetAgent(ctx context.Context, key string) (*AgentRecord, bool, error)
    PutAgent(ctx context.Context, key string, rec AgentRecord) error
    DeleteAgent(ctx context.Context, key string) error
    ListAgents(ctx context.Context) ([]AgentRecord, error)

    // Per-key serialization around a user-driven operation (e.g., EnsureAgent).
    // Held across the callback; see §5.2 and P1.3 doc §4.5.
    WithAgentLock(ctx context.Context, key string, fn func(context.Context) error) error

    // User-scoped env cache (parallel to agent cache).
    GetEnv(ctx context.Context, key string) (*EnvRecord, bool, error)
    PutEnv(ctx context.Context, key string, rec EnvRecord) error
    DeleteEnv(ctx context.Context, key string) error
    ListEnvs(ctx context.Context) ([]EnvRecord, error)
    WithEnvLock(ctx context.Context, key string, fn func(context.Context) error) error
}

type LockMode int
const (
    LockExclusive LockMode = iota
    LockShared
)
```

### 4.4 Spec-hash canonical form

`AgentRecord.SpecHash` is defined here because the cache key depends on it. If a future impl canonicalizes differently from P1.3, the cache silently breaks.

1. **Input.** Hashable subset of `AgentSpec`: `Model`, `SystemPrompt`, `Tools`, `MCPServers`, `Skills`. Exclude `Metadata` (orchestra bookkeeping; changes should not trigger a version bump).
2. **`SystemPrompt` normalization.** Unicode NFC; CRLF → LF. No whitespace trim (trailing whitespace is semantically significant in prompts).
3. **Slice ordering.** `Tools`, `MCPServers`, `Skills` hashed in declared order. Reordering in `orchestra.yaml` intentionally bumps the hash (documented user expectation; easier to explain than "sometimes reorders matter").
4. **Map ordering.** Maps nested inside the above (e.g. `Tool.InputSchema`, `MCPServer.Metadata`) encoded with keys sorted lexicographically, recursively.
5. **Output.** `sha256:<hex>`.

P1.3 owns the canonicalizer implementation; this doc owns the rules. Divergence is a bug, not a design choice.

### 4.5 What's deliberately not in the data model

- **`TeamResult`** (local-backend per-team result file) and **`results/<team>/summary.md`** (MA equivalent) live outside the interface. Store owns state docs; separate helpers own append-on-complete artifacts.
- **`Registry` / `RegistryEntry`** (local-backend subprocess tracking) live outside the interface. Local-internal state with no MA parallel. `Workspace` keeps its existing registry helpers.
- **Event log files (`logs/<team>.ndjson`)**. Append stream; different performance envelope. See §5.4.

---

## 5. Engineering decisions

### 5.1 Mutator closure for `UpdateTeamState`

`UpdateTeamState(ctx, team, fn)` folds load-mutate-save into one atomic step inside the Store. The alternative — caller does `Load`, mutates, calls `Save` — has a lost-update hazard: two team goroutines load the same snapshot, each mutate a different team, each save, the second clobbers the first's unrelated change.

**Why per-team and not per-document?** Teams are the unit of concurrency in the engine. Two goroutines racing on the same team field is rare; two goroutines racing on different teams is the norm (tier-parallel execution). Per-team granularity lets those goroutines proceed independently after the mutator returns.

**Cross-team operations** (e.g. "if A completed, mark B runnable") do *not* use `UpdateTeamState`. They fall back to `LoadRunState` + mutate + `SaveRunState` under the run-lock; the run-lock serves as the cross-process mutex, and the engine's single-writer invariant (see §5.3) serves as the in-process mutex. Cross-team logic is kept in the engine, not the Store — the Store does not know about DAG semantics.

### 5.2 Per-key lock for user-scoped registry

`~/.config/orchestra/agents.json` is a single file shared across every orchestra project on the user's laptop. A naive "flock the whole file for the duration of `EnsureAgent`" wedges cross-project usage: a slow `run` on Project A blocks `orchestra agents ls` on Project B.

`WithAgentLock(ctx, key, fn)` scopes the flock to a sibling file `~/.config/orchestra/.<key>.lock`. Cross-key operations proceed concurrently. The terminal `PutAgent` call still takes a short whole-file flock to serialize the JSON write itself — but that window is milliseconds, not seconds-to-minutes.

**Bounded timeout.** `WithAgentLock` accepts a context deadline. No unbounded waits. On timeout, the caller gets a clear error naming the holding PID (from the lockfile body). Stale lockfiles — whose PID no longer exists — are claimed by the acquirer, with a warning.

### 5.3 Run-lock (cross-process, per-workspace)

`AcquireRunLock` creates `.orchestra/run.lock` containing a pidfile body and takes `gofrs/flock`. Callers:

| Command | Lock mode |
|---|---|
| `orchestra run`, `orchestra resume` | Exclusive |
| `orchestra spawn <team>` | Exclusive (for the duration of the single-team session) |
| `orchestra status` | **None** — reads `state.json` directly; tolerates a brief stale window because atomic-rename writes are never visible half-done |
| `orchestra msg`, `orchestra interrupt`, `orchestra sessions ls` | None (touch MA, don't write locally) |

Why `status` reads lock-free: read-side blocking feels bad (`status` during a long run should feel instant). Consistency-without-locks works because `fsutil.AtomicWrite` is `.tmp` + `os.Rename`; readers either see the old version or the new, never a partial.

### 5.4 Logs and results stay outside the interface

**Write side.** `logs/<team>.ndjson` is append-only high-volume. Funnelling every event through a generic `Store.AppendEvent` would force a per-event round trip on every future SQL/KV impl — punitive for what is essentially a tail pipe. `results/<team>/summary.md` is a single post-run file-write; the existing `WriteResult` helper covers it.

**Read side (admitted gap).** A future hosted impl whose CLI needs to read past events (`orchestra events <team> --since ...`) will need a log-read abstraction, and that abstraction will not be `Store`. When that time comes, a dedicated `EventReader` interface gets added, and the conformance suite stays focused on state docs. The conformance suite is intentionally split in that case — state and event-stream read APIs have different shapes and performance envelopes, and pretending otherwise makes both harder.

Not shipping that now. But not pretending the split doesn't exist either.

### 5.5 Implementations

**`FileStore`** — maps 1:1 to the existing `.orchestra/` + `~/.config/orchestra/` layout. Writes via `fsutil.AtomicWrite`. In-process mutex per instance for run-state operations; `gofrs/flock` on `run.lock` for cross-process; per-key flock for user-scoped reads-modify-writes.

**`MemStore`** — map-backed, `sync.RWMutex`, no I/O. Ships in `pkg/store/memstore/`. Used by engine + spawner tests to avoid the "temp dir per test" overhead.

**Future impls** — a real SQL/KV/Dynamo impl is explicitly out of scope. Constraints the interface imposes on any future impl are called out in §6 and §8.

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Mutator closure for per-team updates | (a) Raw `Load`/`Save` + caller discipline. (b) A generic `Transaction(fn)`. | (a) has a documented lost-update hazard (DESIGN-v2.md §10.2). (b) is more powerful but introduces transaction semantics the Store shouldn't own. Mutator closure is the minimum surface that prevents the hazard. |
| Run-lock with exclusive mode for `run`/`resume`; `status` reads lock-free | (a) No lock (assumed one run). (b) All readers take shared. | (a) is vibes — the critic of an earlier draft called it out. (b) is more principled but `status` becomes blocking during a long run, which feels bad. Lock-free reads are safe because atomic-rename writes are never half-visible. |
| Per-key locks for user-scoped registry | (a) Whole-file flock. (b) No cross-process safety. | (a) wedges multi-project usage on the same laptop. (b) causes silent duplicate agents on MA. Per-key is narrow enough for realistic contention and wide enough to prevent duplicates. |
| `RunState` as a single JSON doc with `map[string]TeamState` | (a) One file per team. (b) Normalize into a teams table at the filestore layer. | (a) multiplies atomic-write operations by team count, complicates archive. (b) is over-engineered for file I/O. Single doc is simpler and `UpdateTeamState` isolates the hotspot. Cost: a future SQL impl is nudged toward blob-per-run or a normalized teams table — flagged in §8 for whoever writes it. |
| Logs stay outside the interface | (a) Add `AppendEvent` + `ReadEvents`. | The append-side is a tail pipe; funnelling it through a generic interface is punitive. The read-side will need its own interface when the time comes (future `EventReader`). |
| Schema change not backwards-compat with v1 `state.json` | (a) Preserve v1 shape byte-for-byte. (b) Dual-write. | (a) forces awkward field placement forever. (b) doubles write costs. Schema change is one-PR and we update readers in lockstep. |

---

## 7. Testing

### 7.1 Conformance suite

`pkg/store/storetest/` exports `RunConformance(t *testing.T, factory func(t) Store)`. Covers every interface method with the same canonical sequence of operations. Both `FileStore` (against `t.TempDir()`) and `MemStore` run it. Any future impl must pass it unchanged.

Conformance cases, at minimum:

- Round-trip `SaveRunState` / `LoadRunState` (byte-for-byte on MemStore; field-for-field on FileStore).
- `UpdateTeamState` applies mutator, persists result.
- `UpdateTeamState` is atomic under N goroutines mutating the same team with increment-a-counter mutators — final counter == N.
- `UpdateTeamState` is atomic under N goroutines mutating *different* teams — all N writes land.
- `AcquireRunLock(LockExclusive)` blocks a second `AcquireRunLock` (via a goroutine with a deadline).
- `AcquireRunLock` with a stale-pid lockfile succeeds with a warning.
- `GetAgent` / `PutAgent` / `DeleteAgent` round-trip.
- `WithAgentLock` serializes a callback; a second acquirer with the same key blocks until the first releases.
- `WithAgentLock` timeout error surfaces the holding PID.

### 7.2 File-store-specific tests

- **Atomicity under crash.** Simulate a write failure between `.tmp` and rename; assert the existing file is untouched.
- **Cross-process flock.** Subprocess-based: two `go run` helpers call `PutAgent(same-key)` concurrently. Assert one value wins, JSON is valid. Runs on Linux, macOS, Windows in CI.
- **Run-lock rejects second `run`.** Subprocess helper holds the exclusive lock; second acquirer errors within the timeout window.
- **`status` during `run` is lock-free and consistent.** While a helper process is actively writing under `UpdateTeamState`, a separate read sees either the pre- or post-state, never partial.

### 7.3 Engine-integration tests

Existing `smoke_test.go` / `e2e_test.go` run unchanged after the migration. Any drift in local-backend behavior surfaces here. The PR description records a before/after run.

---

## 8. Rollout / Migration

Single PR. Atomic because the schema change is atomic. Order of operations inside the PR:

1. **New packages.** `pkg/store/` (interface + types), `pkg/store/memstore/`, `pkg/store/filestore/`, `pkg/store/storetest/`.
2. **`internal/workspace/` refactor.** `Workspace` keeps its existing public method surface (`ReadState` / `WriteState` / `UpdateTeamState` / `ReadRegistry` / `WriteRegistry` / `UpdateRegistryEntry` / `WriteResult` / `ReadResult` / `LogWriter` / `MessagesPath`). These become thin forwarders to `FileStore` or stay as Workspace-local helpers (registry/result/log/messages). Bulk of the mechanical diff lives here.
3. **Call site migration.** `cmd/run.go`, `cmd/spawn.go`, `cmd/status.go`, `cmd/init_cmd.go`, `internal/injection/coordinator.go`, `e2e_test.go`, `smoke_test.go`. Mostly type renames and one import rewrite.
4. **Skill/tool updates.** `.claude/skills/orchestra-monitor` parses `state.json` directly; its `jq` expressions move to the v2 shape. Shipped in the same PR.

**Rollback.** `git revert`. Self-contained.

Post-land, the existing `smoke_test.go` must pass unchanged (local-backend behavior proof). If smoke-test coverage is insufficient for this guarantee, that gap is addressed in the same PR by adding cases, not by waving past it.

Future impls (SQL/KV/Dynamo/hosted) inherit these constraints:

- `UpdateTeamState` atomicity as seen by concurrent callers.
- `AcquireRunLock` semantics (exclusive blocks exclusive; shared blocks nothing read-side).
- Spec-hash canonical form from §4.4.
- Archive semantics — `ArchiveRun` may map to "move dir" (FS), "set `archived_at`" (SQL), or "move key prefix" (KV); the engine's contract is "after `ArchiveRun(run_id)`, subsequent `LoadRunState` returns empty."

---

## 9. Observability & error handling

- All errors from Store methods propagate unwrapped. Impl-specific errors (e.g., `os.ErrNotExist`) are wrapped via `%w` so `errors.Is` works across the boundary.
- `AcquireRunLock` timeout errors name the holding PID and workspace path.
- `WithAgentLock` timeout errors name the holding PID and cache key.
- `slog` at `DEBUG` on every method entry with elapsed-ms on exit — cheap, useful for tracing cache behavior in the wild.
- No Store-level metrics in this feature. If orchestra-in-prod ever needs "cache hit rate" / "lock contention latency," we add them then.

---

## 10. Open questions

1. **SQL-impl shape.** Single-doc `map[string]TeamState` nudges a SQL impl toward either (a) a JSON blob column (re-serializing on every `UpdateTeamState`, giving up row-level updates) or (b) a normalized `teams` table (join on every `LoadRunState`). Neither is horrible at orchestra scale (≤ dozens of teams per run). Flagged for whoever writes a real SQL impl.
2. **`ArchiveRun` on the interface vs. off it.** It's filesystem-flavored, but every impl needs *some* way to retire a run so the next one starts clean. Permissive contract: impls without an archive concept return `nil`. Keeping it on the interface beats a separate `Archiver` fragment.
3. **Log/event read side.** Called out in §5.4. No hosted impl is on our roadmap; add `EventReader` when it becomes load-bearing, not before.
4. **Do we want `LastUsed` on `RunState.Teams` too?** Currently only on `AgentRecord` / `EnvRecord` to drive prune. Run-state analog (last-time-this-team-was-touched) might be useful for `orchestra status` ordering, but not necessary for correctness. Defer.

---

## 11. Acceptance criteria

- [ ] `pkg/store/`, `pkg/store/memstore/`, `pkg/store/filestore/`, `pkg/store/storetest/` exist and compile.
- [ ] `MemStore` and `FileStore` pass `storetest.RunConformance`.
- [ ] File-store-specific tests (§7.2) pass on Linux, macOS, Windows.
- [ ] Existing `smoke_test.go` / `e2e_test.go` pass unchanged.
- [ ] `cmd/` callers compile and behave as before (manual before/after run recorded in PR description).
- [ ] `/orchestra-monitor` skill updated to read the v2 schema; output matches before/after expectations on a canned run.
- [ ] `make test && make vet && make lint` green.
