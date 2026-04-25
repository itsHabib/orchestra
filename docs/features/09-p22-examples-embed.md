# Feature: P2.2 — `examples/embed/` runnable SDK example

Status: **Proposed**
Owner: @itsHabib
Depends on: P2.0 ([07-p20-minimum-sdk-surface.md](./07-p20-minimum-sdk-surface.md))
Relates to: [DESIGN-v2.md](../DESIGN-v2.md) §13 Phase 2 — P2.2 follows P2.0 / P2.1.
Target: a small runnable Go program under `examples/embed/` that drives an orchestra run end-to-end through the public SDK, plus a richer README section pointing at it.

---

## Why this chapter exists

P2.0 shipped a minimum public surface in `pkg/orchestra` and a bare-bones snippet in [README.md](../../README.md). The snippet is enough to prove the surface is reachable; it isn't enough to show what a real consumer would build. A runnable example serves three purposes:

1. **Documentation that compiles.** A snippet in a README rots silently when the API changes; an `examples/embed/main.go` in `go build ./...` does not.
2. **Free design review.** The act of writing a real consumer surfaces awkwardness in the SDK before any external user hits it. (This is the same logic as the dogfood-app argument in P2.0 §1.)
3. **Onramp for the first dogfood app.** PR audit / sweeper / spike harness all need a working starting point. `examples/embed/` is that starting point.

This chapter is intentionally small. It is not a dogfood app — those get their own directories (e.g. `tools/pr-audit/`). This is a 60–120 line program that loads a YAML, runs it, prints the result table.

---

## Scope

### In-scope

- `examples/embed/main.go` — a single-file program that:
  - Takes a config path as argv[1] (default: `examples/embed/orchestra.yaml`).
  - Calls `orchestra.LoadConfig`, prints warnings, exits non-zero on err.
  - Calls `orchestra.Run` with `WithLogger(orchestra.NewCLILogger())`.
  - Iterates `Result.Tiers` for ordered rendering, prints a table mirroring what the CLI's `printSummary` produces (without depending on `cmd/`).
  - Treats `errors.Is(err, orchestra.ErrRunInProgress)` as a friendly "another run is in flight" message rather than a stack trace.
- `examples/embed/orchestra.yaml` — a tiny one- or two-team config that the example can run against (local backend; mock-claude friendly so it's runnable without an Anthropic key).
- README "Use as a Go library" section expanded to point at `examples/embed/` and walk through the program in a few sentences.
- Brief godoc-style comment in `examples/embed/main.go` explaining what the program does and how to run it.

### Out-of-scope

- Managed-agents backend example. Adds a hard dependency on `ANTHROPIC_API_KEY` and live MA. A separate example can come later if asked.
- Coordinator example. Same reasoning — needs claude installed.
- Streaming progress events / live UI. P2.0 §5.10 deferred this; the example uses `WithLogger` for live output.
- Tests for `examples/embed/`. The program is documentation; CI proves it compiles via `go build ./...`. If a regression breaks it, the build fails.

---

## Acceptance criteria

- [ ] `examples/embed/main.go` exists and compiles under `go build ./...`.
- [ ] `examples/embed/orchestra.yaml` exists with at least one team that the example can drive against a mock claude.
- [ ] `go run ./examples/embed examples/embed/orchestra.yaml` (with a mock-claude on PATH) runs the workflow and prints a per-team table.
- [ ] README "Use as a Go library" section links to `examples/embed/` and briefly describes what the example does.
- [ ] No new dependencies added to `go.mod`.
- [ ] No imports from `internal/...` — the example must use only `pkg/orchestra` and stdlib (this is the same enforcement check P2.0 §NF3 makes structurally).

---

## Notes for the implementer

- The whole point is that the example is **small**. Resist the urge to add flags, multiple subcommands, or fancy output. One file, one config, one table.
- Use `Result.Tiers` for rendering order — that's the SDK shape that proves the deferred-iteration design. Don't reach into `Result.Teams` and sort by name.
- If `Run` returns `(result, err)` with both non-nil, render the partial result and then surface the error. P2.0's contract guarantees `Result` reflects whatever state was reached.
- The CLI `printSummary` in `cmd/run_summary.go` is a reasonable reference for what columns to render, but do not import it. Re-implementing the few lines of formatting is the point.
- If the example accidentally exposes an awkward SDK shape (e.g. needing a helper that doesn't exist), that's a P2.1 finding, not an example-side workaround. Surface it and discuss before adding to `pkg/orchestra`.

---

## Open questions

1. **Do we want a "live" managed-agents variant later?** Lean: defer. The text-only example is more useful for first impressions; an MA variant adds setup friction without showing anything new about the SDK shape.
2. **Should the example be runnable directly via `go run` from the repo root, or via `make example-embed`?** Lean: `go run ./examples/embed` keeps the bar low; a make target is reasonable if we add MA later.
