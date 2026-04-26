# Feature: `pr-audit` — first SDK consumer

Status: **Proposed**
Owner: @itsHabib
Depends on: P2.0 ([07-p20-minimum-sdk-surface.md](./07-p20-minimum-sdk-surface.md)) (shipped), P2.4 ([10-p24-operational-sdk.md](./10-p24-operational-sdk.md)) (shipped), P2.5 ([11-p25-validation-result.md](./11-p25-validation-result.md)) (shipped)
Relates to: [DESIGN-v2.md](../DESIGN-v2.md) §13 Phase 2 — the "real consumer on the SDK" item from the v0.1.0 runway. This is the first non-test program that consumes `pkg/orchestra` end-to-end.

Target: a small CLI at `tools/pr-audit/` that takes a GitHub PR number, builds an `orchestra.Config` programmatically, runs a single audit team via `orchestra.Run`, and prints a structured report. Two deliverables of equal weight: the tool itself, and a written **SDK findings** report listing every rough edge encountered while building it.

---

## 1. Why this chapter exists (and why now)

The SDK has shipped its surface in three phases: P2.0 (minimum surface), P2.4 (operational primitives — `Start` / `Handle` / `Wait` / `Cancel` / `Status` / `Send` / `Interrupt` / events), P2.5 (`ValidationResult`). What it has **not** had is a consumer outside the orchestra repo's own CLI. Tests exercise the surface but tests can be written against any shape; a real program with a real job to do exposes ergonomic surprises tests do not.

PR audit is a good first consumer because:

- **It builds a config in memory, not from YAML.** The CLI always consumes YAML — a programmatic `*orchestra.Config` is something only the SDK's prospective callers will do. If `orchestra.Validate(cfg)` or `orchestra.Run(ctx, cfg)` has rough edges when given a hand-built config, this surfaces them.
- **It uses the synchronous `Run` shape.** `Start` + `Handle` is the hot path for long-running observable consumers; `Run` is the hot path for one-shot scripts. PR audit is one-shot — exercises the shorter path.
- **It needs a `*Result` and renders a structured report.** The SDK's `Result` shape (project, teams, tiers, durations) was designed for CLI summary rendering; if a downstream consumer wants a different shape (per-team summary text, JSON output), it surfaces what's missing.
- **It exercises cancellation.** `signal.NotifyContext(ctx, os.Interrupt)` + `orchestra.Run(ctx, cfg)` — Ctrl-C must propagate cleanly, and `*Result` must be non-nil even on partial completion.
- **It does not exercise managed-agents.** Local backend only. MA-specific surface (`Handle.Interrupt`, repository artifacts) is out of scope; that's a separate consumer chapter.

The output is value to a real workflow — auditing a PR for design-doc alignment, godoc completeness, and CHANGELOG hygiene. But the deeper value is the SDK feedback. Even an empty findings list is a finding ("the surface was usable cold without help" is the bar we want).

---

## 2. Requirements

**Functional.**

- F1. The tool is a `package main` Go program at `tools/pr-audit/`. Single positional argument: the PR number. One flag: `--json` for machine-readable output.
- F2. Fetches PR metadata and diff via shelling to `gh pr view <PR> --json title,body,files,additions,deletions,baseRefName,headRefName,url` and `gh pr diff <PR>`. Errors when `gh` is missing or returns non-zero with a clear "is `gh` installed and authenticated?" hint.
- F3. Builds an `orchestra.Config` in memory: single team named `auditor`, three tasks (design-doc alignment, godoc completeness, CHANGELOG hygiene). Diff inlined into the team's `Context` field, truncated to ~50 KB with a `[diff truncated]` footer if oversize.
- F4. Validates the config via `orchestra.Validate(cfg)` and refuses to run on validation failure. Soft warnings render to stderr.
- F5. Runs the audit synchronously via `orchestra.Run(ctx, cfg)`.
- F6. Renders a Markdown report by default; `--json` switches to a flat JSON object. Both shapes carry: PR meta (title, base, head, url, additions, deletions), run status (success / failed-team), workspace path, total cost, total turns, per-team status + summary + cost + turns.
- F7. `Result` from `orchestra.Run` is non-nil even on partial failure (P2.4 contract); the renderer handles whatever's there.
- F8. Env var overrides:
  - `PR_AUDIT_MODEL` overrides the auditor's model. Default `haiku`.
  - `PR_AUDIT_MAX_TURNS` overrides the per-team `max_turns`. Default `10`.
- F9. Ctrl-C cancels the run cooperatively via `signal.NotifyContext(ctx, os.Interrupt)`; the renderer still prints whatever partial result was reached.
- F10. Exit codes: 0 = success, 1 = audit ran but one or more teams failed, 2 = setup or input error (no PR number, `gh` failure, validation failure).

**Non-functional.**

- NF1. **Stdlib + `pkg/orchestra` only.** No new entries in `go.mod` (`require` block). The tool must compile with the existing dependency graph.
- NF2. **`flag` package, not cobra.** PR audit is a one-line CLI; cobra is overkill and pulls a transitive dep that is fine for the orchestra binary but unnecessary for a thin consumer.
- NF3. **No live Claude calls in any test.** Tests mock `gh` (via `execCommand` package-level seam) and mock `orchestra.Run` (via `runOrchestra` package-level seam). All paths are exercised against canned fixtures.
- NF4. **Env var defaults documented in `--help`.** `flag.Usage` is overridden to mention `PR_AUDIT_MODEL` and `PR_AUDIT_MAX_TURNS` so the user discovers them without reading the README.
- NF5. **Workspace under `.orchestra/pr-audit-<PR>/`.** Writes are scoped to the tool's own subdirectory of `.orchestra` so concurrent runs against different PRs don't collide and so the user can clean up trivially. Set via `orchestra.WithWorkspaceDir`.
- NF6. **Single binary, single package.** Source files: `main.go`, `gh.go`, `config.go`, `report.go`, with one `*_test.go` apiece plus `testdata/`. No subpackages.

---

## 3. Scope

### In-scope

- New directory `tools/pr-audit/` with:
  - `main.go` — `package main`. Flag parsing (`--json`), env var resolution, signal handling, orchestrates `gh.go` + `config.go` + `orchestra.Run` + `report.go`. Owns the exit code logic.
  - `gh.go` — `fetchPR(ctx, pr)` shells out to `gh pr view` and `gh pr diff`. Test seam: `execCommand = exec.CommandContext` package-level variable so tests can substitute a stub. Returns a typed error wrapping the `gh` exit status.
  - `config.go` — `buildConfig(pr PRData) *orchestra.Config`. Pure function — no I/O. Reads `PR_AUDIT_MODEL` / `PR_AUDIT_MAX_TURNS` from env via local helpers `modelFromEnv(default string) string` and `maxTurnsFromEnv(default int) int`. Diff is truncated here.
  - `report.go` — `renderMarkdown(*orchestra.Result, PRData) string` and `renderJSON(*orchestra.Result, PRData) ([]byte, error)`. Markdown shape per §4.4; JSON is a flat object.
  - `gh_test.go` — overrides `execCommand` with a stub returning canned bytes from `testdata/pr-21.json` and `testdata/pr-21.diff`. Asserts `fetchPR` parses the fixture into `PRData`.
  - `config_test.go` — builds a config from the canned fixture, asserts `orchestra.Validate(cfg).Valid() == true`, asserts team count, task count, model resolution from env-or-default, diff truncation behavior.
  - `report_test.go` — feeds a fabricated `*orchestra.Result` into both renderers; asserts substrings in markdown and schema in JSON.
  - `main_test.go` — uses `runOrchestra` package var to substitute a fabricated `*orchestra.Result`; asserts exit codes for success / failed-team / setup-error / cancelation. Skips the actual `orchestra.Run` call.
  - `testdata/pr-21.json` — captured `gh pr view` output for the just-merged P2.5 ValidationResult chapter. Real, recent, non-trivial PR.
  - `testdata/pr-21.diff` — captured `gh pr diff 21` output. ~100 KB — exercises the truncation path naturally.
  - `README.md` — installation (`go build ./tools/pr-audit/`), usage, env vars, sample report.
- Design doc updates:
  - This file (`docs/features/12-pr-audit-tool.md`).
- **No CHANGELOG entry.** This chapter touches no SDK surface — it consumes the existing one. Any changes flagged in the SDK findings list become their own follow-up chapters with their own CHANGELOG entries.

### Out-of-scope

- **YAML config support** for the tool (`pr-audit -c custom.yaml`). The point is to exercise the programmatic config path. If a user wants YAML, the existing `orchestra` CLI runs YAML.
- **Concurrent audits** of multiple PRs from one invocation. Out of scope; users can run the tool once per PR.
- **Posting the report back to the PR** (`gh pr comment`). Out of scope; users redirect output if they want to.
- **GitHub HTTP API access** without `gh`. The `gh` shell-out is the simplest path; it inherits the user's auth and respects rate limits transparently.
- **`--keep-workspace` / `--clean-workspace`.** The workspace lives at `.orchestra/pr-audit-<PR>/`; users can delete it manually. A flag is premature.
- **Auditor team structure beyond the three tasks.** The tasks (design-doc alignment, godoc completeness, CHANGELOG hygiene) are this chapter's heuristic for "useful PR audit." Tunability is intentionally absent — the value of the tool is fixed; tunability is the user's problem to fork later.
- **Managed-agents backend.** Local backend only. The MA path needs repository configuration, GitHub PAT resolution, and session steering — none of which serve the audit use case.
- **Stability commitment for the `pr-audit` interface.** This is a sample consumer, not a supported tool. The Markdown / JSON shapes can change without notice; consumers depending on the JSON format pin a commit SHA. Documented in the README.

---

## 4. API surface

### 4.1 Tool layout

```
tools/pr-audit/
  main.go                  // package main
  gh.go                    // fetchPR + execCommand seam
  config.go                // buildConfig + env helpers
  report.go                // renderMarkdown + renderJSON
  gh_test.go
  config_test.go
  report_test.go
  main_test.go             // exit-code coverage via runOrchestra seam
  testdata/
    pr-21.json
    pr-21.diff
  README.md
```

### 4.2 `config.go` shape

```go
package main

import (
    "os"
    "strconv"

    "github.com/itsHabib/orchestra/pkg/orchestra"
)

const (
    defaultModel        = "haiku"
    defaultMaxTurns     = 10
    diffTruncationLimit = 50 * 1024 // ~50 KB
)

// buildConfig assembles the orchestra.Config used by the audit run.
// Pure function; reads PR_AUDIT_MODEL and PR_AUDIT_MAX_TURNS from env.
func buildConfig(pr PRData) *orchestra.Config {
    diff := pr.Diff
    if len(diff) > diffTruncationLimit {
        diff = diff[:diffTruncationLimit] + "\n\n[diff truncated]\n"
    }

    return &orchestra.Config{
        Name: "pr-audit-" + strconv.Itoa(pr.Number),
        Defaults: orchestra.Defaults{
            Model:    modelFromEnv(defaultModel),
            MaxTurns: maxTurnsFromEnv(defaultMaxTurns),
            // ... other defaults populated by ResolveDefaults via Validate
        },
        Teams: []orchestra.Team{{
            Name: "auditor",
            Lead: orchestra.Lead{Role: "Senior PR auditor"},
            Context: "## PR\n" + pr.Title + "\n\n" + pr.Body +
                "\n\n## Diff\n" + diff,
            Tasks: []orchestra.Task{
                {Summary: "Design-doc alignment: ..."},
                {Summary: "Godoc completeness: ..."},
                {Summary: "CHANGELOG hygiene: ..."},
            },
        }},
    }
}
```

### 4.3 `gh.go` shape

```go
package main

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "os/exec"
    "strconv"
)

// execCommand is the seam: tests override to a stub that returns canned
// bytes. Production callers leave it pointing at exec.CommandContext.
var execCommand = exec.CommandContext

// PRData is the parsed `gh pr view` payload plus the raw diff text.
type PRData struct {
    Number       int
    Title        string
    Body         string
    URL          string
    BaseRefName  string
    HeadRefName  string
    Additions    int
    Deletions    int
    Files        []ghFile
    Diff         string
}

type ghFile struct {
    Path      string `json:"path"`
    Additions int    `json:"additions"`
    Deletions int    `json:"deletions"`
}

// ErrGhUnavailable wraps a non-zero gh exit, missing-binary failure, or
// authentication failure. Surfaces from main as exit code 2 with a hint
// about installing or authenticating gh.
var ErrGhUnavailable = errors.New("gh CLI unavailable")

// fetchPR shells to `gh pr view ... --json ...` and `gh pr diff ...`,
// returning the merged PRData. Errors wrap ErrGhUnavailable with the
// captured stderr.
func fetchPR(ctx context.Context, pr int) (PRData, error)
```

### 4.4 Markdown report shape

```
# PR audit: orchestra#21

**Title.** P2.5 PR1: ValidationResult ...
**URL.** https://github.com/itsHabib/orchestra/pull/21
**Branch.** main ← p2.5/validation-result
**Diff.** +1438 / -243 lines across 27 files

## Audit run

- **Status.** success
- **Workspace.** .orchestra/pr-audit-21/
- **Total cost.** $0.0123 USD
- **Total turns.** 7

## auditor

- **Status.** complete
- **Turns.** 7
- **Cost.** $0.0123 USD

> The PR aligns with the design doc on three of the four key axes ...
> Godoc on every new exported identifier passes inspection ...
> CHANGELOG entry under "Experimental: breaking" matches the spec.

---

_Generated 2026-04-25 16:42:11 UTC by tools/pr-audit_
```

### 4.5 Test seam pattern

Two package-level vars are the test seams:

```go
// in gh.go
var execCommand = exec.CommandContext

// in main.go
var runOrchestra = orchestra.Run
```

Tests stub via `t.Cleanup` to restore. Example (`main_test.go`):

```go
func TestMain_SuccessReturnsZero(t *testing.T) {
    orig := runOrchestra
    runOrchestra = func(_ context.Context, _ *orchestra.Config, _ ...orchestra.Option) (*orchestra.Result, error) {
        return &orchestra.Result{
            Project: "pr-audit-21",
            Teams: map[string]orchestra.TeamResult{
                "auditor": {TeamState: store.TeamState{Status: "complete", NumTurns: 5}},
            },
        }, nil
    }
    t.Cleanup(func() { runOrchestra = orig })
    ...
}
```

This pattern follows P2.4's `steeringSendUserMessage` indirection — package-level var, doc comment naming the test seam, restored via `t.Cleanup`.

---

## 5. Implementation order

Suggested commit order. Each commit compiles + passes existing tests.

1. **`tools/pr-audit: scaffold + gh fetcher`** — `main.go` skeleton (just `flag.Parse` + a placeholder), `gh.go` with `fetchPR` and `execCommand` seam, `testdata/pr-21.json` + `pr-21.diff`, `gh_test.go` driving the fixture through the seam.
2. **`tools/pr-audit: config builder`** — `config.go` with `buildConfig`, env helpers, `config_test.go` asserting validation passes and truncation works on the oversize fixture.
3. **`tools/pr-audit: report rendering + JSON output`** — `report.go` with `renderMarkdown` + `renderJSON`, `report_test.go` with fabricated `*orchestra.Result`.
4. **`tools/pr-audit: wire main + signal handling + main_test`** — `main.go` complete: `signal.NotifyContext`, exit codes, env vars, `--json` flag, `runOrchestra` seam, `main_test.go` driving the seam through success / failed-team / setup-error scenarios.
5. **`docs: pr-audit design doc + tools/pr-audit/README.md`** — this design doc and the user-facing README.

---

## 6. Acceptance criteria (chapter-level)

- [ ] `tools/pr-audit/` builds: `go build ./tools/pr-audit/`.
- [ ] All tests pass: `go test ./tools/pr-audit/...`.
- [ ] No new entries in `go.mod`'s direct require block.
- [ ] `go vet ./...`, `go test ./...`, `go test -race ./...` (Linux CI), `go tool golangci-lint run ./...` all green.
- [ ] `pr-audit --help` shows the `--json` flag and documents `PR_AUDIT_MODEL` / `PR_AUDIT_MAX_TURNS`.
- [ ] Markdown and JSON outputs both carry the §4.4 fields.
- [ ] Ctrl-C during a run produces a partial report (the `*Result` from a canceled run is non-nil).
- [ ] Exit codes match §F10: 0 / 1 / 2.
- [ ] **SDK findings** section in the implementer's PR description listing every rough edge (or "no surprises encountered" if none).

---

## 7. Open questions

1. **Diff size limit.** 50 KB is roughly the size of a 1500-line patch — about the largest a model can usefully read in one pass at haiku context budgets. Resolved here: truncate at 50 KB with a `[diff truncated]` footer; do not split into chunks. A future "deep audit" mode could chunk + summarize per-file, but that's a different tool.

2. **Workspace location.** `.orchestra/pr-audit-<PR>/` keeps audits per-PR isolated. Using `.orchestra/` collides with the parent project's own `orchestra` runs. Resolved: scoped subdirectory per PR; user cleans up with `rm -rf .orchestra/pr-audit-*`.

3. **Audit task wording.** The three tasks are this chapter's heuristic for a useful PR audit. Resolved: hard-coded for now. Tunability is left to forks; the chapter's value is the SDK feedback, not the audit prompt engineering.

4. **`--json` schema stability.** Resolved: not stable. The README explicitly says "consumers depending on this JSON shape should pin a commit SHA." If the JSON shape becomes load-bearing for an external consumer, a versioned schema is the next chapter — but the current shape is intentionally minimal and unversioned.
