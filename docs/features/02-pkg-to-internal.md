# Feature: Move `pkg/` into `internal/`

Status: **Proposed**
Owner: @itsHabib

---

## 1. Motivation

`pkg/store` and `pkg/spawner` were placed under `pkg/` aspirationally — to signal "external consumers welcome, shape this as a library surface." In practice there is no SDK consumer yet, no external users, and no stability promise to break. During P1.3 (registry cache) and P1.4 (MA session lifecycle) the `Store` and `Spawner` interfaces will churn; `internal/` better matches that reality and lets the Go compiler enforce "in-tree only."

When an SDK story is actually on the table (see DESIGN-v2 future phases), the packages can move back to `pkg/` — or to a dedicated `pkg/orchestra/` SDK surface — with a single rename. The move is cheap in either direction; the signal is not.

**Policy (2026-04-20):** this is the repo-wide direction going forward. Every new package lands under `internal/`; `pkg/` is off-limits until SDK extraction is a real, scheduled work item. This supersedes the placement guidance in [DESIGN-v2.md](../DESIGN-v2.md) §7 and D9 — those sections stage new packages under `pkg/` to make Phase 2 mechanical, but the cost of that staging (signaling a stable surface that does not exist) outweighs the saved refactor. DESIGN-v2 amendments to reflect this are pending.

## 2. Scope

Mechanical moves (shipped as a single PR):

- `pkg/store/` → `internal/store/` (including `memstore/`, `filestore/`, `storetest/` subpackages)
- `pkg/spawner/` → `internal/spawner/` (currently an empty leftover directory; remove before the move)
- Update imports across ~31 files: `cmd/*.go`, `internal/run/`, `internal/workspace/`, `internal/injection/`, tests, and the moved packages' own internal references.
- Update `.claude/CLAUDE.md` and `AGENTS.md` project-structure sections.
- Update `docs/DESIGN-v2.md` §7 and D9 to reflect the `internal/`-first policy.

Out of scope: any interface change, any behavior change.

Applies to new packages too. Any feature doc that proposes `pkg/<x>/` (e.g. an earlier draft of this document's reasoning, or DESIGN-v2 §7's `pkg/orchestra/`, `pkg/spawner/`, `pkg/state/`, `pkg/dag/`) is superseded — use `internal/<x>/`. Current design docs that already follow the policy: [04-agent-service.md](./04-agent-service.md), [05-p15-repo-artifact-flow.md](./05-p15-repo-artifact-flow.md).

## 3. Validation

- `go build ./...`, `go vet ./...`, `go test ./...` all green.
- `git grep "github.com/itsHabib/orchestra/pkg/"` returns nothing.
- CI passes.

## 4. Rollback

`git revert`. Self-contained mechanical move; no behavior change to preserve.

## 5. Follow-ups (not this PR)

- Revisit placement when the SDK question is revisited. Candidate shapes at that point: dedicated `pkg/orchestra/` library surface, or keep everything internal and expose via a separate module.
