# pr-audit

A small CLI consumer of `pkg/orchestra` that audits a GitHub pull
request: fetches the PR via `gh`, builds an `orchestra.Config` in
memory, runs a single auditor team, and prints a structured findings
report.

> **Sample, not supported.** `pr-audit` is the orchestra repo's first
> SDK consumer outside its own CLI. Its purpose is two-fold: do a
> useful PR audit, and surface ergonomic gaps in `pkg/orchestra` that
> only show up when a real program consumes it. The Markdown and JSON
> output shapes can change without notice; consumers depending on the
> JSON format should pin a commit SHA.

## Install

```sh
go build ./tools/pr-audit/
```

The binary ends up at `./pr-audit` (or `./pr-audit.exe` on Windows).
Move it onto `$PATH` if you want shorter invocations.

## Usage

```sh
pr-audit <PR-number>          # Markdown report on stdout
pr-audit --json <PR-number>   # Flat JSON object on stdout
```

Diagnostics (validation warnings, gh failures, run failures) go to
stderr. Exit codes:

| Code | Meaning                                                      |
| ---: | :----------------------------------------------------------- |
|  `0` | Audit ran; every team finished                               |
|  `1` | Audit ran; at least one team failed (report still printed)   |
|  `2` | Setup error: missing arg, gh unavailable, validation failure |

## Requirements

- `gh` on `$PATH`, authenticated. The tool shells to `gh pr view ...`
  and `gh pr diff ...`; if either call fails the tool exits 2 with a
  hint pointing at `gh auth status`.
- A working `claude` binary on `$PATH` if you want the auditor team to
  actually run. The audit uses the local backend with a single team —
  no managed-agents access required.
- Network access to GitHub (transitively, via `gh`) and to Anthropic's
  API (transitively, via `claude`).

## Environment variables

| Variable             | Default | What it does                                                  |
| :------------------- | :------ | :------------------------------------------------------------ |
| `PR_AUDIT_MODEL`     | `haiku` | Overrides the auditor's model                                 |
| `PR_AUDIT_MAX_TURNS` |    `10` | Caps per-task turn budget; non-numeric / non-positive ignored |

```sh
PR_AUDIT_MODEL=sonnet PR_AUDIT_MAX_TURNS=20 pr-audit 21
```

## Workspace

Each invocation writes its run state under `.orchestra/pr-audit-<PR>/`
in the current directory. Concurrent runs against different PRs are
independent; concurrent runs against the same PR fail with
`orchestra: run already in progress for workspace`. Clean up with
`rm -rf .orchestra/pr-audit-*` when you're done.

## What gets audited

The tool builds a single-team config with three tasks:

1. **Design-doc alignment.** Identifies the design doc the PR claims to
   implement and checks whether the diff matches the spec (in-scope
   items implemented, out-of-scope items not snuck in).
2. **Godoc completeness.** Scans the diff for newly exported
   identifiers and verifies each has a meaningful godoc comment.
3. **CHANGELOG hygiene.** When the PR touches `pkg/orchestra`,
   verifies a `## Unreleased` entry exists and matches the change set.

The auditor model is `haiku` by default — read-only reasoning at
short turn budgets (`max_turns=10`), so a typical audit costs less
than a few cents.

## Sample report

The Markdown shape (representative, not from a live run):

```markdown
# PR audit: #21

- **Title.** P2.5 PR1: ValidationResult reshape
- **URL.** https://github.com/itsHabib/orchestra/pull/21
- **Branch.** main -> p2.5/validation-result
- **Diff.** +1438 / -243 lines

## Audit run

- **Status.** success
- **Workspace.** .orchestra/pr-audit-21/
- **Total cost.** $0.0123 USD
- **Total turns.** 7
- **Duration.** 124850ms

## auditor

- **Status.** done
- **Turns.** 7
- **Cost.** $0.0123 USD

> Findings:
>
> 1. Design doc alignment — matches §3 in-scope items: types,
>    function reshapes, validator changes, CLI migration. No
>    out-of-scope behavior detected.
> 2. Godoc — every newly exported identifier carries a doc comment.
>    `ValidationResult` (alias) leans on internal/config.Result's
>    godoc, which is consistent with the existing alias pattern.
> 3. CHANGELOG — `### Experimental: breaking — pkg/orchestra
>    validation result reshape (P2.5)` entry is present and bullets
>    match the diff (removed shapes + added types).

---

_Generated 2026-04-25 16:42:11 UTC by tools/pr-audit_
```

The `--json` shape (subject to change without notice):

```json
{
  "pr": {
    "number": 21,
    "title": "P2.5 PR1: ValidationResult reshape",
    "url": "https://github.com/itsHabib/orchestra/pull/21",
    "base_ref_name": "main",
    "head_ref_name": "p2.5/validation-result",
    "additions": 1438,
    "deletions": 243
  },
  "status": "success",
  "workspace": ".orchestra/pr-audit-21/",
  "cost_usd": 0.0123,
  "turns": 7,
  "duration_ms": 124850,
  "teams": [
    {
      "name": "auditor",
      "status": "done",
      "turns": 7,
      "cost_usd": 0.0123,
      "summary": "Findings: ..."
    }
  ],
  "generated_at": "2026-04-25T16:42:11Z"
}
```

## Limitations

- **Diff size.** The diff is truncated at 50 KB before being inlined
  into the auditor's context. PRs with patches larger than that get
  a `[diff truncated]` footer; the auditor sees only the first 50 KB.
- **Single team.** No fan-out. The audit runs as one auditor against
  three tasks; we don't split per-file or per-task into parallel
  teams.
- **Local backend only.** The tool does not configure managed-agents
  even if your shell's environment has MA credentials. Audits run as
  local `claude -p` subprocesses.
- **No `--keep-workspace` / `--clean-workspace` / `--config` flags.**
  Out of scope; users manage workspace cleanup themselves and don't
  override the auditor prompt.

## See also

- The chapter design doc:
  [`docs/features/12-pr-audit-tool.md`](../../docs/features/12-pr-audit-tool.md).
- The orchestra SDK package: `pkg/orchestra/`.
- The orchestra CLI for running YAML-based workflows: top-level
  `orchestra` binary.
