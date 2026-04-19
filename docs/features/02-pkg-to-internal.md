# Feature: Move `pkg/` into `internal/`

Status: **Proposed**
Owner: @itsHabib

---

## 1. Motivation

`pkg/store` and `pkg/spawner` were placed under `pkg/` aspirationally — to signal "external consumers welcome, shape this as a library surface." In practice there is no SDK consumer yet, no external users, and no stability promise to break. During P1.3 (registry cache) and P1.4 (MA session lifecycle) the `Store` and `Spawner` interfaces will churn; `internal/` better matches that reality and lets the Go compiler enforce "in-tree only."

When an SDK story is actually on the table (see DESIGN-v2 future phases), the packages can move back to `pkg/` — or to a dedicated `pkg/orchestra/` SDK surface — with a single rename. The move is cheap in either direction; the signal is not.

## 2. Scope

- `pkg/store/` → `internal/store/` (including `memstore/` and `filestore/` subpackages)
- `pkg/spawner/` → `internal/spawner/`
- Update imports across `cmd/`, `internal/run/`, `internal/workspace/`, `internal/injection/`, and any tests that reference the moved packages.
- Update `.claude/CLAUDE.md` and `AGENTS.md` project-structure sections.

Out of scope: any interface change, any behavior change.

## 3. Validation

- `go build ./...`, `go vet ./...`, `go test ./...` all green.
- `git grep "github.com/itsHabib/orchestra/pkg/"` returns nothing.
- CI passes.

## 4. Rollback

`git revert`. Self-contained mechanical move; no behavior change to preserve.

## 5. Follow-ups (not this PR)

- Revisit placement when the SDK question is revisited. Candidate shapes at that point: dedicated `pkg/orchestra/` library surface, or keep everything internal and expose via a separate module.
