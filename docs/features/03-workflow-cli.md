# Feature: Workflow-first CLI surface

Status: **Implemented**
Owner: @itsHabib

---

## 1. Motivation

P1.3 shipped `orchestra agents ls` / `orchestra agents prune` as the first
user-visible entry points into the Managed Agents cache. In review the
framing was flagged: those commands expose MA nouns (agents, environments)
directly, which duplicates the Claude API rather than delivering
workflow-level value. Orchestra's goal is to abstract DAG workflows — the
primary CLI nouns should be `runs`, `teams`, `projects`, not `agents`.

The underlying registry cache (spec hashing, adopt-or-create, per-key locks,
archive-and-recreate on env drift) has real value and stays. What changes is
only the CLI surface users see.

## 2. Scope

- Introduce a workflow-oriented top-level group. Candidate shape:
  - `orchestra runs ls` — list recent runs with status, cost, duration.
  - `orchestra runs show <run-id>` — drill into a run to see teams, DAG tier,
    cached MA agent/env IDs, session IDs.
  - `orchestra runs prune` — reclaim resources (cache records + optionally
    MA agents/envs) tied to runs matching a predicate (older than, terminal
    state, etc.).
- Retire or demote the `agents` group:
  - Delete `cmd/agents.go` as a user-facing command, or
  - Move it behind `orchestra debug agents …` / gate with a hidden flag.
- Orphan reconciliation (`--reconcile`) becomes a `runs`-scoped concept:
  MA agents tagged by Orchestra but not referenced by any known run.

## 3. Design questions to settle before implementing

- Where do "runs" live canonically? `.orchestra/runs/<run-id>/` already
  exists for per-run state — the CLI should read from there rather than
  build a parallel index.
- What does `prune` mean for a completed vs. still-cached-for-reuse run?
  The cache deliberately outlives a single run to dedupe subsequent ones;
  prune should target *stale* or *orphaned*, not *old*.
- Do we still need a low-level escape hatch for "show me every MA resource
  Orchestra touches"? Useful for debugging, but must not be the default UX.

## 3.1 Implementation decisions

- The canonical on-disk layout in this repo is flat active state plus
  `.orchestra/archive/<run-id>/`, not `.orchestra/runs/<run-id>/`. The `runs`
  CLI reads active `.orchestra/state.json` and archived `state.json` files
  from that layout.
- New run state persists each team's DAG tier so `orchestra runs show <run-id>`
  can display tier information without reverse-engineering the original config.
  Older archived states that predate this field show `-` for tier.
- The low-level Managed Agents cache command is still available as
  `orchestra debug agents ...` for support/debugging, but it is no longer a
  top-level user-facing noun.
- `runs prune` deletes only local stale cache records. It does not archive or
  delete MA-side resources; MA-side cleanup remains an explicit future step
  because the current cache records are reusable across runs and do not carry a
  run ownership boundary.

## 4. Validation

- `orchestra runs ls` surfaces the same information a user would previously
  have reconstructed by cross-referencing `agents ls` + run state on disk.
- No CLI command takes an MA agent ID or env ID as its primary argument.
- CLAUDE.md / AGENTS.md updated to reflect the new CLI shape.

## 5. Out of scope

- Changing the cache semantics themselves (keys, hashing, adoption).
- Changing the `Spawner` interface.
- Adding new MA features.

## 6. Follow-ups deferred from P1.3

- Environment prune (P1.3 only prunes agent records).
- Bounded-scan documentation noting duplicates on later pages are visible
  now that the scan collects across pages (already addressed in PR #6 follow-up).
