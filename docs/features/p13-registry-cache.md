# Feature: P1.3 — Registry cache (`EnsureAgent` / `EnsureEnvironment`)

Status: **Proposed**
Owner: @itsHabib
Depends on: [00-store-interface.md](./00-store-interface.md)
Relates to: [DESIGN-v2.md](../DESIGN-v2.md) §9.1, §9.2, §13 phase P1.3.

---

## 1. Overview

P1.4 needs `agent_id` and `environment_id` to start an MA session. The naive approach — call `Beta.Agents.New` and `Beta.Environments.New` per team per run — fails four ways:

1. **Rate limits.** MA caps at 60 creates/min per org. A 20-team project burns a third of that every run; under tier-parallel load with retries, 429s become routine.
2. **Duplicate resources.** MA does not enforce name uniqueness. N runs → N duplicate agents cluttering the dashboard.
3. **Lost version lineage.** MA agents are versioned; `Update` bumps the version. Re-creating per run replaces that lineage with "how many times we ran."
4. **Cold-start cost.** Environments cache package installs across sessions sharing the same env resource. Re-creating per run defeats the cache (minutes of setup time per tier-start).

P1.3 caches agent and env resource IDs in the user-scoped Store, keyed by `<project>__<role>` (agents) and `<project>__<env-name>` (envs). On run start, orchestra reuses, updates on spec drift, adopts if MA already has a matching resource, and creates only as a last resort.

---

## 2. Requirements

**Functional.**

- F1. `ManagedAgentsSpawner.EnsureAgent(ctx, AgentSpec) (AgentHandle, error)` — returns a valid MA agent ID for the role, creating, updating, or adopting as needed.
- F2. `ManagedAgentsSpawner.EnsureEnvironment(ctx, EnvSpec) (EnvHandle, error)` — same shape for envs.
- F3. Read/write the cache via the `Store` interface; never reach the filesystem directly.
- F4. Deterministic `SpecHash` across platforms per the canonical-form rules in the Store doc §4.4.
- F5. List-and-adopt path handles "cache is empty but an agent already exists on MA" (fresh laptop, wiped `~/.config`, CI).
- F6. CLI: `orchestra agents ls` and `orchestra agents prune` for housekeeping.
- F7. `LastUsed` on cache records is touched on cache hit (so prune can act on actual usage, not last-write).

**Non-functional.**

- NF1. Happy path is zero new-resource API calls — `EnsureAgent` on a warm cache makes exactly one `Beta.Agents.Get` (to verify the cached agent isn't archived).
- NF2. Cold `EnsureAgent` on a cache miss does at most `max_list_pages` (default 10) of `Beta.Agents.List` before falling back to create. No full org scans.
- NF3. Per-key lock on cache writes is cross-process safe (see Store doc §5.2). Timeout is bounded (default 90s).
- NF4. No orphan MA resources created under concurrent `EnsureAgent` on the same key from two processes.
- NF5. Spec-hash computation does not panic on valid input; hash stability is platform-independent (covered by tests).

---

## 3. Scope

### In-scope

- `ManagedAgentsSpawner.EnsureAgent` + `EnsureEnvironment`.
- Spec-hash implementation per Store-doc §4.4.
- `<project>__<role>` / `<project>__<env-name>` naming.
- List-and-adopt path using MA metadata tagging.
- `orchestra agents ls` command.
- `orchestra agents prune` command (dry-run by default; `--apply` writes).
- Unit tests, concurrency tests, opt-in integration tests.

### Out-of-scope

- **`StartSession`, `ResumeSession`.** P1.4 / P1.8.
- **Retry layer for 429 / 5xx.** P1.4 owns the engine-level retry layer (DESIGN-v2.md §9). P1.3 errors bubble up; callers (P1.4+) catch them.
- **`orchestra envs ls`.** Agents get CLI surface because they clutter (many per project); envs do not (one per project). Add if demand surfaces.
- **In-place env update.** MA does not support env config update. Drift triggers archive-old-create-new (see §5.2).
- **Orchestra-wide dashboard / metrics.** Tracking "cache hit rate across all users" is a hosted-orchestra feature, not P1.3.

---

## 4. Data model

### 4.1 Records

Defined in the Store doc §4.2. Relevant fields for this feature:

```go
type AgentRecord struct {
    Key       string    // "<project>__<role>"
    Project   string
    Role      string
    AgentID   string    // MA resource ID
    Version   int       // MA-side version (for Beta.Agents.Update)
    SpecHash  string    // canonical sha256 per Store-doc §4.4
    UpdatedAt time.Time // last write by orchestra
    LastUsed  time.Time // touched on cache hit; drives prune semantics
}

type EnvRecord struct { ... /* parallel shape */ }
```

### 4.2 Agent naming on MA

Orchestra-created MA agents have:

- `name: "<project>__<role>"` (double underscore namespacing matches DESIGN-v2.md §9.1).
- `metadata: {"orchestra_project": <name>, "orchestra_role": <role>, "orchestra_version": "v2"}`.

The metadata tag is load-bearing for §5.3 list-and-adopt scoping. An agent without the tag is treated as "not ours" during adopt — we never adopt an agent a human created by hand via the dashboard, because we cannot trust its spec matches.

### 4.3 Spec-hash canonical form

Per Store doc §4.4 — enforced here. Summary:

1. Hashable: `Model`, `SystemPrompt`, `Tools`, `MCPServers`, `Skills`. Exclude `Metadata`.
2. Normalize `SystemPrompt`: Unicode NFC; CRLF → LF. No trim (trailing whitespace is semantically meaningful).
3. Slices in declared order (reorder bumps the hash).
4. Maps key-sorted recursively.
5. Output: `sha256:<hex>`.

Reference implementation sketch:

```go
func specHash(s AgentSpec) string {
    canon := canonicalForm{
        Model:        s.Model,
        SystemPrompt: normalizePrompt(s.SystemPrompt),
        Tools:        s.Tools,
        MCPServers:   s.MCPServers,
        Skills:       s.Skills,
    }
    b, err := canonicaljson.Marshal(canon)
    if err != nil { panic(err) } // programming error on our own structs
    sum := sha256.Sum256(b)
    return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizePrompt(s string) string {
    return strings.ReplaceAll(norm.NFC.String(s), "\r\n", "\n")
}
```

Library: `github.com/gibson042/canonicaljson-go` preferred (RFC 8785 compliant, small, well-tested). Alternative is a hand-rolled 50-line encoder; either is fine.

---

## 5. Engineering decisions

### 5.1 `EnsureAgent` control flow

```
key  := "<project>__<role>"
hash := specHash(spec)

store.WithAgentLock(ctx, key, func(ctx) error {
    rec, found, err := store.GetAgent(ctx, key)
    if err != nil { return err }

    if found {
        agent, err := ma.Agents.Get(ctx, rec.AgentID)
        switch {
        case isNotFound(err):
            // Dashboard delete or eventual-consistency window.
            // See §5.4 — don't delete cache yet; fall through to adopt.
            goto adopt
        case err != nil:
            return err
        case agent.ArchivedAt != nil:
            // Cache points at archived agent; adopt from fresh list.
            goto adopt
        case rec.SpecHash == hash && rec.Version == agent.Version:
            // Fast path. Touch LastUsed; return.
            return store.PutAgent(ctx, key, rec.withLastUsedNow())
        case rec.SpecHash != hash:
            // Spec drift: Update bumps MA version.
            updated, err := ma.Agents.Update(ctx, agent.ID, params(spec), agent.Version)
            if err != nil { return err }
            slog.Info("agent updated due to spec drift", "key", key, "new_version", updated.Version)
            return store.PutAgent(ctx, key, AgentRecord{...Version: updated.Version, SpecHash: hash, ...})
        default:
            // Same hash, version drifted (someone touched the agent in the MA dashboard).
            // Refresh our cache to match; log a warning so this is visible.
            slog.Warn("agent version drifted outside orchestra; adopting new version", "key", key, "prev", rec.Version, "current", agent.Version)
            return store.PutAgent(ctx, key, rec.withVersion(agent.Version).withLastUsedNow())
        }
    }

adopt:
    matches, err := listScoped(ctx, ma, spec.Project, spec.Role) // see §5.3
    if err != nil { return err }

    switch len(matches) {
    case 0:
        created, err := ma.Agents.New(ctx, params(spec, key, orchestraMetadata(spec)))
        if err != nil { return err }
        return store.PutAgent(ctx, key, AgentRecord{...AgentID: created.ID, ...})
    case 1:
        adopted := matches[0]
        // Hash-match check: if the adopted agent's recomputed hash matches ours, reuse.
        // Otherwise Update to align.
        // (We can recompute from the returned spec fields.)
        if recomputeSpecHash(adopted) == hash {
            return store.PutAgent(ctx, key, AgentRecord{...AgentID: adopted.ID, Version: adopted.Version, ...})
        }
        updated, err := ma.Agents.Update(ctx, adopted.ID, params(spec), adopted.Version)
        if err != nil { return err }
        return store.PutAgent(ctx, key, AgentRecord{...AgentID: updated.ID, Version: updated.Version, ...})
    default:
        // Multiple matches with our metadata tag — shouldn't happen with the per-key lock,
        // but possible if someone bypassed orchestra. Adopt most recently updated; warn.
        slog.Warn("multiple orchestra-tagged MA agents match key; adopting most recent", "key", key, "matches", ids(matches))
        sort.Slice(matches, byUpdatedAtDesc)
        return adoptSingle(matches[0])
    }
})
```

**The flow runs under `WithAgentLock(key)`** — a per-key cross-process flock from the Store doc §5.2. Two `EnsureAgent` calls with the same key serialize; the second enters the callback and finds the first's cache write. No orphan creates.

**On `WithAgentLock` timeout:** error with the holding PID and the key. Users can `orchestra agents unlock <key>` (stretch; see §10) if something is genuinely wedged.

### 5.2 `EnsureEnvironment` control flow

Same shape as `EnsureAgent`, with two differences:

1. **No Update path.** MA environments are not versioned and the API does not expose an update endpoint. On spec drift, archive the old env and create a new one. The new `EnvID` replaces the cached one. Old env is left archived (not deleted) so any still-running sessions that referenced it continue to work.
2. **Archived-env reuse during drift window.** Between the archive call and the new create, a concurrent session creation could race. The per-key flock serializes `EnsureEnvironment` itself; cross-`Ensure` races don't happen.

Claim that "archived envs continue to serve existing sessions" is from DESIGN-v2.md §9.3 summarizing MA behavior. Confirmed against [environments docs](https://platform.claude.com/docs/en/managed-agents/environments): "Archive an environment (read-only, existing sessions continue)." Cite this in the implementation code comment.

### 5.3 List scope: avoiding full org scans

Naive `Beta.Agents.List` is O(total-agents-in-org) per cache-miss `EnsureAgent`. Unacceptable once a user has multiple orchestra projects (or other MA consumers) on the same account.

Tiered mitigation:

1. **Metadata filter (preferred).** Orchestra tags every agent it creates with `metadata.orchestra_project` + `orchestra_role`. If the SDK supports server-side metadata filtering — to confirm against the SDK version pinned in `go.mod`; docs describe `metadata` as free-form but do not document query — use it. Worst-case narrows to one project's agents.
2. **Client-side short-circuit.** Regardless of server filtering, paginate client-side and stop on first match. For the zero-match case (cold cache, first-run of a new role), we still have to exhaust — but that's a new-role one-time cost, not a steady-state cost.
3. **Bounded scan ceiling.** Cap at `max_list_pages` (default 10) when searching for the zero-match case. If no match is found within that ceiling, treat as "no match, proceed to create." Logged at INFO with page count scanned. A user with more than ~1000 agents can raise the cap via config.

P1.3 ships (2) and (3) unconditionally; (1) is implemented if the SDK supports it at the pinned version.

### 5.4 Handling transient MA 404s

`Beta.Agents.Get` can 404 for reasons other than "agent deleted":

- Eventual-consistency window immediately after create.
- Transient MA outage returning 404 on existing resources.
- Network blip surfacing as 404 at some gateway.

Silently deleting the cache entry on first 404 triggers list-and-adopt, which on a large org blows NF2.

**P1.3 behavior:** on 404 from `Agents.Get`, do **not** delete the cache entry. Fall through to list-and-adopt; if that finds the agent (eventual-consistency window passed, or it's just a gateway blip), proceed with the adopted record. If list-and-adopt returns zero matches, create a new agent — and only then delete the stale cache entry.

### 5.5 Version-drift handling (same hash, different version)

If `rec.SpecHash == hash` but `rec.Version != agent.Version`, someone has touched the agent outside orchestra (manual update via the MA dashboard, for instance). Two options: (a) treat our cache as authoritative and re-Update to revert, (b) adopt the new version as-is.

(a) overwrites external changes without warning. (b) silently accepts external changes. **We pick (b) with a `WARN` log** — the out-of-band change is visible in logs, so an operator can see it; but we don't fight it. Users who want strict orchestra-as-source-of-truth can layer that on top later by treating WARN-level drift events as a signal to re-apply.

### 5.6 CLI surface

**`orchestra agents ls`**

```
KEY                     AGENT ID                  VERSION  LAST USED           MA STATUS
chatbot__backend        agent_01HqR2k7...         3        2026-04-17 09:22    active
chatbot__frontend       agent_01HqR4m9...         2        2026-04-17 09:22    active
oldproject__whatever    agent_01Gz...             1        2025-11-03 14:00    archived
```

Reads the cache, calls `Beta.Agents.Get` concurrently (bounded worker pool, default 5) to annotate with MA status. Get failures show as `unreachable` per-row; the command exits 0 so it's usable in scripts.

**`orchestra agents prune`**

Semantics of "stale" — all of the following qualify:

- MA returns 404 on the cached ID (deleted via dashboard, not via orchestra).
- MA returns `archived_at != nil` (archived outside orchestra).
- `LastUsed` older than `--older-than` (default 30d). `LastUsed` is touched on every cache hit, so an agent used daily by `orchestra run` stays un-stale regardless of spec drift.

Default is **dry-run**. `--apply` performs the deletes. `--reconcile` also lists orchestra-tagged MA agents that exist on MA but are *not* in the cache (orphans), for manual cleanup — does not auto-delete those.

**`orchestra agents unlock <key>`** (stretch; see §10)

Removes a wedged per-key lockfile if a prior process died without releasing.

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Per-key flock, bounded timeout | (a) Global flock on `agents.json` for the whole EnsureAgent. (b) No flock. | (a) wedges cross-project usage on the same laptop (one slow `run` on Project A blocks `agents ls` on Project B). (b) allows duplicate MA creates on concurrent EnsureAgent. Per-key with a bounded timeout limits blast radius. |
| List-and-adopt with metadata filter + bounded scan | (a) Full `ListAutoPaging` every time. (b) No list-and-adopt, always create on cache miss. | (a) is O(org-wide-agents) and unacceptable at scale. (b) leaves the "fresh laptop against an existing MA" case broken and keeps creating duplicates. Metadata + bounded scan bounds the cost. |
| Adopt only orchestra-tagged agents | (a) Adopt any name-match. | (a) silently adopts human-created test agents whose spec we cannot verify. Tag-gated adoption makes adopted agents opt-in. |
| `normalizePrompt` = NFC + CRLF→LF, no trim | (a) Also trim trailing whitespace. (b) Hash raw. | (a) loses meaning — trailing whitespace on a prompt can affect completion behavior. (b) causes Windows/Linux hash mismatches on the same logical prompt. NFC + CRLF hits the 95% case without losing semantics. |
| Warn-on-drift (same hash, different version) | (a) Re-Update to revert. (b) Accept silently. | (a) fights the user's intent. (b) hides the out-of-band change. WARN log makes it visible without clobbering. |
| Env drift: archive-old, create-new | (a) Delete old immediately. | (a) risks breaking a still-running session holding the old env ID. Archive is reversible; users can clean up later via prune. |
| Don't delete cache on single 404 | (a) Delete on first 404. | (a) interprets transient MA issues (eventual consistency, gateway blips) as "agent is gone," triggering full list scans. Fall-through-to-adopt is cheap when the resource does exist, and correct when it doesn't. |
| No retry layer in P1.3 | (a) Retry inside EnsureAgent. | P1.4 owns the engine retry layer. Duplicating retry logic at the Ensure level fragments the retry story. Bubble errors; let P1.4 handle them. |

---

## 7. Testing

### 7.1 Unit (no network)

- `TestSpecHash_Deterministic` — identical input → identical hash over 1000 iterations.
- `TestSpecHash_ChangesOnEachField` — parameterized over `Model`, `SystemPrompt`, `Tools[0].Name`, `MCPServers[0].URL`, `Skills[0].Name`; assert each mutation changes the hash.
- `TestSpecHash_MetadataIgnored` — changing `Metadata` does not change hash.
- `TestSpecHash_SliceOrderMatters` — reordering `Tools` changes the hash.
- `TestSpecHash_MapOrderIgnored` — equivalent maps (different insertion order) hash identically.
- `TestSpecHash_NFCNormalization` — Unicode NFC vs. NFD on `SystemPrompt` hash identically.
- `TestSpecHash_CRLFNormalization` — CRLF vs. LF hash identically.
- `TestSpecHash_GoldenCases` — two or three fixed specs pinned to expected hash values checked into the test file. Cross-platform CI catches drift.
- `TestEnsureAgent_FastPath_CacheHit` — fake MA returns matching agent; assert one `Get` call, `PutAgent` updates `LastUsed` only.
- `TestEnsureAgent_SpecDrift_Updates` — fake MA `Update` returns bumped version; assert cache updated.
- `TestEnsureAgent_404_FallsThroughToAdoptAndFinds` — fake `Get` returns 404, list finds match; assert adopted, no `New`.
- `TestEnsureAgent_404_FallsThroughToCreate` — fake `Get` returns 404, list empty; assert `New` called, stale entry replaced.
- `TestEnsureAgent_ArchivedCache_Adopts` — fake `Get` returns archived; assert fresh list scanned.
- `TestEnsureAgent_ZeroMatches_Creates` — empty cache + empty list; assert `New`.
- `TestEnsureAgent_MultipleMatches_AdoptsMostRecent` — three list matches, warns, adopts.
- `TestEnsureAgent_VersionDriftSameHash_WarnsAndRefreshes` — cache version < MA version, hashes match; assert WARN logged + cache updated.
- `TestEnsureAgent_OnlyAdoptsOrchestraTagged` — list returns one untagged + one tagged match; adopts only the tagged one.
- `TestEnsureEnvironment_Drift_ArchivesOldCreatesNew` — spec hash mismatch; assert `Archive` + `New` + cache updated.
- `TestListBoundedByMaxPages` — fake MA returns 20 pages; list stops at 10 with INFO log.

### 7.2 Concurrency (real Store, fake MA)

- `TestEnsureAgent_SameKeyTwoGoroutines_SingleCreate` — two goroutines + shared `FileStore`; assert exactly one `New` call, one cache entry.
- `TestEnsureAgent_DifferentKeys_ConcurrentProceed` — two goroutines with different keys; assert both complete without serialization (per-key flock, not whole-file).
- `TestWithAgentLock_SlowMA_TimesOut` — fake MA `Get` hangs past the flock timeout; assert EnsureAgent errors with PID + key in the message.

### 7.3 Cross-platform flock (subprocess)

- Two `go run` subprocesses, same key, against a shared user-config dir. Assert one `New`, no JSON corruption. Runs on Linux, macOS, Windows CI.
- One subprocess holds the lock; kill it with SIGKILL. Next acquirer sees stale pid and claims the lock with a warning.

### 7.4 Integration (live MA, opt-in)

Gated behind `ANTHROPIC_API_KEY` + `ORCHESTRA_LIVE_TESTS=1`. `go test -tags=integration ./...` path:

- Create a temporary role name (timestamped). Run `EnsureAgent` twice. Assert second call is fast path, no new agent on MA. Clean up via `Beta.Agents.Archive`.
- Create + mutate spec + re-`EnsureAgent`. Assert MA-side version bumped by one.
- Wipe the cache dir, re-`EnsureAgent`. Assert list-and-adopt found the existing agent, no new create.

### 7.5 Local-backend regression

The PR must run `smoke_test.go` / `e2e_test.go` (local backend) before and after; assert `go test` output is unchanged. Local-backend codepath does not touch `ManagedAgentsSpawner`; regression would indicate accidental coupling.

---

## 8. Rollout / Migration

Single PR building on the Store interface from 00-store-interface.md (which must land first).

1. `pkg/spawner/managed_agents.go` — struct `ManagedAgentsSpawner{store, ma, logger}` wiring.
2. `pkg/spawner/managed_agents_ensure.go` — `EnsureAgent` + `EnsureEnvironment` + spec-hash.
3. `pkg/spawner/managed_agents_test.go` — §7.1 + §7.2 tests.
4. `cmd/agents.go` — `agents ls` / `agents prune` [/ `agents unlock`] subcommands; registered on root command.
5. `cmd/root.go` — register the agents subcommand group.

No changes to `internal/workspace/` (the Store doc's PR did those). No changes to `pkg/spawner/local.go`. No changes to `orchestra.yaml` schema.

**Rollback.** `git revert`. Self-contained.

---

## 9. Observability & error handling

**Logging.**

- `slog.Debug` on every branch: cache hit, hash match, drift update, 404-fallthrough, archived-fallthrough, adopt, create, multiple-match, version-drift.
- `slog.Info` on create, Update, archive-then-recreate — these cost API budget; visibility matters.
- `slog.Warn` on multiple-match adoption, version drift (same hash, different version), bounded-list ceiling hit, stale-pid lock recovery.
- `slog.Error` on unrecoverable paths (MA 5xx, JSON marshal bug).

**Error handling.**

| Error | Behavior |
|---|---|
| `Store.GetAgent` error (disk I/O, corrupt JSON) | Propagate. User inspects / deletes cache. |
| MA 429 / 5xx | Propagate. P1.4's retry layer handles these. P1.3 does not retry. |
| MA 401/403 | Propagate with a hint about `ANTHROPIC_API_KEY`. |
| MA 404 on `Get` | Do not delete cache; fall through to adopt. |
| Canonical-JSON marshal error | Panic. Programming bug on our own structs. |
| `Store.PutAgent` write failure after MA create | Log the orphan's MA ID prominently. Next `EnsureAgent` re-adopts via list. |
| `WithAgentLock` timeout | Error with holding PID and key. User can `orchestra agents unlock <key>` (stretch) or kill the holding process. |

**Metrics.** Not in P1.3. If cache hit rate ever becomes interesting (during P1.6 load measurement), add then.

---

## 10. Open questions

1. **Canonical-JSON library vs. hand-rolled.** `github.com/gibson042/canonicaljson-go` is RFC 8785 compliant, single-purpose. Hand-rolled is ~50 lines. Library is preferred unless transitive-dep weight is a concern; either passes the tests.
2. **SDK metadata filter.** Unknown whether `Beta.Agents.List` supports `metadata.<key>` server-side filtering at the pinned SDK version. To confirm during implementation. If not supported, we rely on client-side short-circuit + bounded scan; no design impact.
3. **`orchestra agents unlock`.** Ship in P1.3 or defer to P1.10 polish? Lean: defer. The bounded timeout + stale-pid detection together cover the common failure modes; a manual unlock is a safety net for the long tail.
4. **Metadata hashing escape hatch.** Some users will want `metadata.owner` changes to version-bump. Add a per-project `spec_hash_includes_metadata: true` flag later if demand surfaces; excluded for P1.3. Migration note: hashes are not comparable across the flag flip.
5. **Adopt-with-hash-mismatch auto-Update.** §5.1's "adopt single match + Update to align" is somewhat aggressive (we assume our local spec is authoritative). If a user wants orchestra to never auto-modify an existing MA agent, they need a `--no-update-adopted` flag. Defer; add on demand.

---

## 11. Acceptance criteria

- [ ] `pkg/spawner/managed_agents.go` and `managed_agents_ensure.go` exist; `ManagedAgentsSpawner` implements `Spawner`'s `EnsureAgent` / `EnsureEnvironment`. (Other methods return `ErrUnsupported` until P1.4.)
- [ ] `cmd/agents.go` implements `agents ls` and `agents prune --dry-run` against a seeded cache.
- [ ] §7.1 unit tests pass.
- [ ] §7.2 concurrency tests pass.
- [ ] §7.3 cross-platform flock tests pass on Linux, macOS, Windows CI.
- [ ] §7.4 integration tests pass when run with `ORCHESTRA_LIVE_TESTS=1` against a real MA account.
- [ ] §7.5 — `smoke_test.go` / `e2e_test.go` output byte-identical before/after (local-backend regression guard).
- [ ] `make test && make vet && make lint` green.
- [ ] PR description demonstrates three scenarios end-to-end: fresh cache → create; stale-hash cache → Update; cache-miss-with-existing-MA-agent → adopt.
