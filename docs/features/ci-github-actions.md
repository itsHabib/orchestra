# Feature: CI via GitHub Actions (lint / vet / test)

Status: **Proposed**
Owner: @itsHabib
Depends on: nothing — net-new. No prior CI exists.
Relates to: [Makefile](../../Makefile) (`make lint test vet`), [.golangci.yml](../../.golangci.yml), [go.mod](../../go.mod) `tool` directive.

---

## 1. Overview

Orchestra has no CI. Every check — `go vet`, `go test ./...`, `go tool golangci-lint run ./...` — runs only on a contributor's laptop, on demand, via `make check`. That means:

1. **PRs can merge red.** Nothing stops a "green on my machine, broken on CI" change because there is no CI. The only gate is reviewer memory.
2. **Platform coverage is whatever the author happens to use.** Orchestra does real work across Linux/macOS/Windows (subprocess lifecycle, path handling, atomic rename in `fsutil`, future cross-process flock in the Store work). Author-only test runs catch Linux regressions well, Windows regressions poorly.
3. **The lint config is invisible to reviewers.** `.golangci.yml` is elaborate; a reviewer cannot tell, from a PR alone, whether the author ran it. Drift accumulates silently until the next `make lint` breaks on unrelated lines.

We add a GitHub Actions workflow that runs `vet`, `test`, and `lint` on every push to `main` and every PR. No Anthropic-backed integration is run in CI for this feature — that is explicitly out of scope (see §3, §5.4). The goal is a fast, deterministic gate that matches what `make check` does locally, minus the paths that require an API key or a real `claude` CLI on the runner.

---

## 2. Requirements

**Functional.**

- F1. A GitHub Actions workflow runs on `push` to `main` and on every `pull_request` targeting `main`.
- F2. The workflow runs, at minimum: `go vet ./...`, `go test ./...` (default build tags), and `go tool golangci-lint run ./...`.
- F3. Go toolchain version is sourced from `go.mod` (`go-version-file: go.mod`). No hard-coded duplicate of the toolchain version in the workflow file.
- F4. The workflow must fail (non-zero exit) if any of vet / test / lint fail. PRs with failing CI must be blockable via branch protection (branch-protection config itself is outside this PR's diff, but the workflow must produce the required check name).
- F5. The workflow must not require, read, or reference `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, or any other live-provider secret.
- F6. The workflow must not invoke or require the real `claude` CLI on the runner.

**Non-functional.**

- NF1. **Fast.** End-to-end wall clock for a warm cache on `ubuntu-latest`: under 3 minutes for the common path (vet + test + lint). Cold (no cache): under 6 minutes. See §5.3 on caching.
- NF2. **Deterministic.** No flakes from network, missing binaries, or unpinned tool versions. Runs offline after `go mod download` (caveat: `actions/*` steps fetch from GitHub).
- NF3. **Cheap.** Single OS (Linux) for the initial landing. Matrix expansion (macOS/Windows) is a forward flag in §5.5, not a launch requirement, because orchestra has no platform-specific code *today* that a Linux run won't exercise.
- NF4. **Readable failure output.** On lint failure, the annotated diff appears inline on the PR (GitHub Actions annotations). On test failure, the failing package and test name are in the job summary, not buried in a 5000-line log.
- NF5. **Least privilege.** Workflow `permissions:` block grants only `contents: read`. No `write` scopes. No reusable-workflow side channels.
- NF6. **Pinned third-party actions.** All non-`actions/*`-org steps pinned by commit SHA, not tag. `actions/checkout` and `actions/setup-go` pinned by major version (`@v4`, `@v5`) per GitHub's own recommendation for first-party actions.

---

## 3. Scope

### In-scope

- One workflow file: `.github/workflows/ci.yml`.
- Three jobs (or one job with three steps — see §5.1): `lint`, `vet`, `test`.
- Go module + build cache via `actions/setup-go@v5` (built-in cache, not a separate `actions/cache` invocation).
- Concurrency group so a fresh push to a PR cancels the prior run.
- `//go:build smoke` stays excluded (default build tags skip it). No new tag gate added in this feature.

### Out-of-scope

Stated explicitly because each is a tempting adjacent scope:

- **No Anthropic-backed E2E.** Nothing in this workflow talks to `api.anthropic.com` or runs a real `claude -p` subprocess. That's a separate feature once we decide what credential surface we accept (OIDC-federated token? per-repo secret?) and what budget a provider-hitting run is allowed to spend.
- **No `go test -race` on the default path.** Race detector doubles test wall time and surfaces real bugs in concurrent code. We want it, but not in the PR-gating job on day one. Flagged in §5.6 for a follow-up.
- **No coverage upload.** `go test -cover` is easy; a Codecov/Coveralls integration is a policy decision (trust model, free-tier limits, PR comment spam). Defer.
- **No release workflow.** `goreleaser`, tag-triggered builds, signed artifacts — all out. This feature is about the merge gate only.
- **No matrix across OSes.** macOS and Windows runners cost more and orchestra has no OS-specific code paths today that Linux won't exercise. Forward flag in §5.5.
- **No branch protection configuration.** The workflow produces check names that branch protection *can* require. Turning the requirement on is a one-off repo setting, not a file in this PR.
- **No `e2e_test.go` rewrite.** The root-package `TestE2E_OrchestraRun` uses a mock claude binary built on-the-fly; it runs inside `go test ./...` without any live credential. It stays in the default test run. If it ever grows a real-provider path, that code gets gated behind a new build tag (not this feature).

"We have CI" is not "we have quality gates." Style-linting, static analysis, and unit tests catch a meaningful slice of regressions — they do not catch semantic ones. Acceptance tests against a real `claude` subprocess (the smoke suite) stay a local/manual concern for now.

---

## 4. Workflow layout

File: `.github/workflows/ci.yml`.

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

concurrency:
  group: ci-${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

permissions:
  contents: read

jobs:
  check:
    name: vet / test / lint
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Download modules
        run: go mod download

      - name: Vet
        run: go vet ./...

      - name: Test
        run: go test ./...

      - name: Lint
        run: go tool golangci-lint run ./...
```

**Notes on the shape.**

- Single job, three steps. Rationale in §5.1.
- `cancel-in-progress` is `true` only for PRs. `main` pushes always finish — we want a clean history of `main` results.
- `timeout-minutes: 10` is a safety net, not a target (target is NF1's 3 min warm / 6 min cold).
- `go mod download` is its own step so a dependency-fetch failure is named clearly in the UI instead of blamed on the first `go vet`.
- `cache: true` in `setup-go@v5` caches both `$GOMODCACHE` and `$GOCACHE` keyed on `go.sum` + OS + go version. Empirically better than hand-rolling `actions/cache` for Go.

---

## 5. Engineering decisions

### 5.1 One job with three steps vs. three parallel jobs

Going with **one job, three steps** for the initial landing.

- Three parallel jobs (fan-out) would overlap vet/test/lint, giving faster wall clock on the happy path. Cost: three repo checkouts, three `setup-go` runs, three cache restores. On a project this size, each of those is ~15–30s — fanning out pays off only when per-job work is much larger than per-job fixed cost.
- Today's vet/test/lint on orchestra runs in maybe 60–120s total on a warm laptop. Fan-out saves perhaps 20–30s of wall clock at the cost of tripling fixed-cost seconds and tripling the "which job failed, let me open three tabs" overhead.
- One sequential job keeps the CI UI clean: one green checkmark or one red one, with the failing step named inline.

If the `test` step ever gets long enough (say, adding `-race` pushes it past 2 min), we revisit and split `test` into its own job. Flagged in §5.6.

### 5.2 Toolchain resolution

`actions/setup-go@v5` with `go-version-file: go.mod` reads the `go` directive from `go.mod` (currently `1.26.2`) and resolves the matching toolchain. This is the single source of truth. Hard-coding `go-version: "1.26"` in the workflow is rejected because it drifts from `go.mod` — which is what `go build` / `go test` actually honor — and the drift shows up as "CI passes, local fails" or vice versa.

`go.mod` carries a `tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint` directive (Go 1.24+ tool directive), so `go tool golangci-lint run ./...` is the invocation. Do **not** use the `golangci/golangci-lint-action` GitHub Action — it installs its own golangci-lint version, which will silently diverge from the one pinned in `go.mod`. The invocation above is the same path `make lint` takes, which is the whole point.

### 5.3 Caching strategy

`setup-go@v5`'s built-in cache is keyed on `go.sum` hash + OS + Go version. On cache hit, `go mod download` is a no-op; `go test`/`go vet`/`go tool ...` benefit from `$GOCACHE` as well.

- Cache miss (first run after a `go.sum` change): ~2–3 minutes of module download + tool compilation.
- Cache hit: ~10–20 seconds of cache restore.

No separate `actions/cache` step is added. The `tool` directive's compiled golangci-lint binary lives under `$GOCACHE` and is cached with everything else.

### 5.4 Why no Anthropic-backed E2E in CI

Two reasons, one practical one principled.

**Practical.** `internal/machost/client.go` hits `api.anthropic.com`. Running its E2E surface from CI requires a real API key, which requires a repo secret, which requires an answer to: who can run CI on a fork (and therefore exfiltrate the secret)? The default GitHub Actions posture (secrets denied to fork PRs) is correct but makes fork PRs untestable on the provider-hitting paths — confusing for contributors and for us.

**Principled.** A provider-hitting test has non-zero cost (both dollar and rate-limit) per run. CI runs on every push. Multiplying those produces either budget pain or a quota that throttles CI itself. Neither is hard to solve, but both are *policy* problems, not *engineering* problems, and they do not belong in the same PR as "we have lint in CI now."

The `internal/machost/client_test.go` cases that `t.Setenv("ANTHROPIC_API_KEY", "sk-...")` are unit tests against the constructor's env-parsing behavior; they do not make network calls and run fine in CI with no secret.

### 5.5 Single-OS for launch; matrix as forward flag

Orchestra runs on Linux/macOS/Windows in production. The workflow launches on `ubuntu-latest` only. Justification:

- Orchestra's Go code today has very little OS-specific branching. `internal/fsutil/` uses `os.Rename` which is atomic on all three. `e2e_test.go` has platform-aware helpers (`goCommand`, `testExecutableName`) but they're already exercised on Linux.
- macOS and Windows runners are **10×** and **2×** the cost of Linux minutes respectively (GitHub's billing docs). Paying that on every PR to catch ~zero OS-specific bugs is a bad trade today.
- Upcoming Store work (`AcquireRunLock`, per-key `flock`) *will* introduce OS-specific code and needs matrix coverage. The Store doc (00-store-interface.md §7.2) already calls out "Runs on Linux, macOS, Windows in CI" as part of its acceptance criteria. When that feature lands, the matrix expands — and that's the right trigger, not this one.

Expansion sketch (for when we're ready): replace `runs-on: ubuntu-latest` with a matrix on `[ubuntu-latest, macos-latest, windows-latest]`. The steps themselves don't change. Shell syntax in the steps is already POSIX-compatible (no bashisms).

### 5.6 What's deliberately deferred

- **`go test -race ./...`.** Doubles test wall time and allocates more; surfaces real concurrency bugs. Add after first mover is comfortable with the CI baseline. Probably as its own job so warm-path PR gating stays fast.
- **Coverage.** `go test -coverprofile=cover.out ./...` is one flag away. Uploading to a third party (Codecov, Coveralls) is a trust/policy call. Local `go tool cover` is fine for now.
- **Dependency-review action** / **govulncheck**. Both valuable. Both out of scope for "we don't have CI yet" → "we have CI now."
- **`go mod verify`.** One-liner, catches tampered modules. Can add trivially in a follow-up.

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Single job, sequential steps | Three parallel jobs fanned out | Fan-out pays when per-job work ≫ per-job fixed cost. Orchestra is too small today; we'd pay ~45s of fixed cost to save ~30s of wall clock. Revisit when `test` grows past 2 min. |
| `go tool golangci-lint run` via `go.mod` tool directive | `golangci/golangci-lint-action@vX` | The action installs its own binary, which can and will drift from `go.mod`. We want "same command as `make lint`" — period. |
| `go-version-file: go.mod` | Hard-coded `go-version: "1.26"` | `go.mod` is already the single source of truth for the toolchain. Don't duplicate it in YAML. |
| Linux-only at launch | Linux/macOS/Windows matrix | 3× minutes cost for near-zero real OS-specific coverage today. Matrix lands with the Store feature, which has genuine cross-OS surface. |
| No Anthropic-backed E2E in CI | Run E2E with a repo secret | Secrets + fork-PR model + per-run cost is a policy problem we are not solving in this PR. The existing `TestE2E_OrchestraRun` (mock claude) still runs. |
| `cancel-in-progress: true` for PRs only | Always / never | Cancelling mid-PR saves runner minutes on force-pushes. Not cancelling `main` means a rushed merge never loses the prior run's result. |
| `permissions: { contents: read }` at workflow level | Inherit repo default (write) | OIDC-era hygiene. Future steps (release, PR comments) opt into scopes in their own job blocks. |
| Don't add a new build tag for `e2e_test.go` | Gate root-package E2E behind `//go:build e2e` | The existing root E2E uses a mock; it's fast (seconds) and a genuine integration proof of the spawner + DAG + workspace. Gating it would be a regression. If it ever grows a real-provider path, *that* code gets the tag. |

---

## 7. Testing

Testing the CI itself is partly recursive ("CI tests the repo; what tests the CI?"). The practical approach:

- **Dry-run locally.** Before opening the PR, run `act` (or just: the three step commands) on the same Linux image (`ubuntu-latest` ≈ `ghcr.io/catthehacker/ubuntu:act-latest`). Confirm exit codes match intended semantics.
- **Break-it test.** Introduce a deliberate lint violation on a throwaway branch, push, confirm the job fails with the expected annotation. Revert. Do the same for a failing test (`t.Fatal`) and a vet violation (shadowed variable). Record the three failing-PR URLs in the merge PR's description as proof.
- **Cache correctness.** Push twice: first with a `go.sum` edit, second with only a `.go` source edit. Confirm run 2 uses the cache (visible in `setup-go` step log) and run 1 does not.
- **Concurrency cancel.** Push two commits back-to-back to a PR branch. Confirm the first run is cancelled automatically.
- **Fork-PR dry-run.** Fork the repo from a throwaway account, open a PR. Confirm the workflow runs (secrets-free) and does not fail for lack of credentials.

No automated test for the workflow YAML itself. `actionlint` as a pre-commit or follow-up step would be cheap insurance; flagged in §5.6's "deferred" neighborhood but not blocking.

---

## 8. Rollout / Migration

Single PR. Self-contained.

Order of operations inside the PR:

1. **Add `.github/workflows/ci.yml`** exactly as in §4.
2. **Open PR from a branch that deliberately breaks one check** (one of the three) to prove the gate works. Once demonstrated, push a commit that fixes it; PR goes green.
3. **Land.**
4. **Post-land: repo settings.** Enable branch protection on `main` requiring the `check / vet / test / lint` status (the exact display name is `<workflow>` / `<job>`; confirm against the first successful run). Not in the PR diff — it's a settings-page click.

**Rollback.** `git revert` the workflow file. No code paths depend on CI existing.

**Forward constraints** the workflow imposes on future PRs:

- Any change that adds network-hitting tests must gate them behind a build tag, or they'll break CI offline-after-module-download.
- Any new linter added to `.golangci.yml` runs in CI on the next PR. If the linter has a one-time codebase-wide cleanup cost, that cleanup ships in the same PR as the linter addition, not after.
- Dependencies added via `go get` update `go.sum`; next CI run is a cache miss. Expected and fine.

---

## 9. Observability & error handling

- **Failure surface.** GitHub Actions renders failed-step output inline on the PR. For lint, golangci-lint's default output is line-level and GitHub picks up the annotations via the `::error file=...` format automatically (golangci-lint v2 emits these).
- **Timeout.** `timeout-minutes: 10` hard-kills a stuck job. That's a circuit breaker, not a performance target; a job taking more than 3–4 minutes is a signal, not a feature.
- **Logs.** Retained per GitHub defaults (90 days). Not uploading artifacts — no test output file, no coverage profile — because none of the current jobs produce one worth keeping and the UI renders `go test` output fine.
- **Metrics.** No CI-health dashboard. If flakes become a pattern, the fix is to find and fix the flaky test, not to build out observability for the flake.

---

## 10. Open questions

1. **`actionlint` in the same PR or follow-up?** One-liner (`rhysd/actionlint` as a composite action or a pre-commit hook) that validates `ci.yml` syntax. Cheap. Defer unless someone pushes back on review.
2. **Go toolchain auto-update cadence.** `go.mod` pins `go 1.26.2`. When 1.26.3 ships, do we auto-bump via Dependabot? Leaning yes (Go patch releases are safe; `setup-go@v5` handles the install). Open question: does Dependabot understand `go.mod`'s `go` directive for bumps, or only `require` entries? Verify before enabling.
3. **Required status check name.** GitHub's branch protection matches on the exact job name `vet / test / lint`. That name is proposed in §4 but finalized only after the first successful run shows it rendered. If it looks ugly in the UI we can rename; it's cheap.
4. **Do we want a "PR body must reference issue" lint?** Out of scope for this feature. Flagged because it often comes up the moment CI exists — and the answer is "no, not here."

---

## 11. Acceptance criteria

- [ ] `.github/workflows/ci.yml` exists, matches §4's shape.
- [ ] On push to `main`: workflow runs, all three steps pass on a clean `main`.
- [ ] On PR to `main`: workflow runs and is required (after branch-protection enablement, post-land).
- [ ] Deliberate lint violation in a proof-branch: CI fails with lint annotation on the PR diff.
- [ ] Deliberate failing test in a proof-branch: CI fails with the failing test named in the job summary.
- [ ] Deliberate vet violation in a proof-branch: CI fails at the `Vet` step.
- [ ] Fork PR (from a throwaway account): workflow runs to completion without requiring any secret.
- [ ] Two back-to-back PR pushes: the first run is cancelled by the second (concurrency group works).
- [ ] Warm-cache end-to-end wall clock under 3 minutes on `ubuntu-latest`. Cold-cache under 6 minutes.
- [ ] `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, or any provider credential does **not** appear in the workflow file, runner environment, or any secret referenced by the workflow.
- [ ] `make check` locally and the CI run exercise the same commands (`go vet ./...`, `go test ./...`, `go tool golangci-lint run ./...`) — verified by inspection, no silent divergence.
