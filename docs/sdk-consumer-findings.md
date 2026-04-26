# SDK consumer findings — pkg/orchestra

A research artifact from a build-and-discard experiment. Twelve ergonomic surprises hit while building a non-trivial Go consumer of `pkg/orchestra` (a one-shot PR-audit CLI: programmatic `*Config`, `Validate`, `Run` with `WithEventHandler`, `WithWorkspaceDir`, `Result`/`TeamResult` field access, Ctrl-C cancellation). The tool itself was not checked in — see [closed PR #22](https://github.com/itsHabib/orchestra/pull/22) for the full implementation if you want to revive it.

The two follow-up PRs that capture the most actionable subset:
- `pkg/orchestra: doc-pass on ergonomic gaps` — covers items 3, 5, 6, 8, 9, 10, 12 + a `.gitignore` polish.
- `pkg/orchestra: consumer helpers from pr-audit feedback` — covers items 1, 4, 5 (the exported half), and the `TeamStatus*` constants observation in item 3.

Findings are listed in the order encountered while building. Roughly: 1–4 are "had to debug or read source to figure this out," 5–8 are "missing helpers or godoc," 9–12 are polish.

---

## 1. No `orchestra.NewConfig(name)` constructor

**Where.** [pkg/orchestra/run.go](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/run.go) exposes `LoadConfig` (YAML path) and `Validate` (already-built `*Config`), but nothing for "give me a default-resolved `*Config` to fill in."

**What surprised.** Building a `*Config` programmatically required reading `internal/config/schema.go`'s `ResolveDefaults` cold to learn that `Defaults.PermissionMode` defaults to `acceptEdits` and `Defaults.TimeoutMinutes` to 30. Neither is documented on the public `orchestra.Defaults` alias and neither shows up in the godoc for `orchestra.Config`.

**Suggestion.** `orchestra.NewConfig(name string) *Config` returning a struct with defaults already resolved, with a doc comment naming each default value so the IDE tooltip teaches the user what they get.

## 2. `orchestra.Validate` mutates the caller's config via `ResolveDefaults`

**Where.** [pkg/orchestra/run.go:65](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/run.go).

**What surprised.** `Validate(cfg)` calls `cfg.ResolveDefaults()` before validating. This is documented in the godoc but only as "applies ResolveDefaults to cfg" — easy to miss, and surprising for a function named `Validate`. Saw `cfg.Defaults.Model` change from `""` to `"haiku"` after `Validate` and had to debug by reading the source.

**Suggestion.** Either rename to `ValidateAndResolve`, or expose `cfg.ResolveDefaults()` separately as a documented step in the SDK doc. If keeping the current name, lift the mutation note into a dedicated paragraph that cross-links to `CloneConfig` for shared-config-across-goroutines callers.

**Partially resolved.** `CloneConfig` already exists at [pkg/orchestra/run.go:69-83](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/run.go) with godoc that says "Use this when sharing a Config across goroutines that may invoke Run concurrently." The remaining gap is that `Validate`'s godoc doesn't cross-link to `CloneConfig` — a follow-up shouldn't re-add the helper.

## 3. `orchestra.TeamState` field godoc lives in `internal/store/run_state.go`

**Where.** [pkg/orchestra/types.go:124](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/types.go) aliases `store.TeamState`. The godoc on the alias says "After P2.0 it includes NumTurns alongside the existing token / cost counters" — but the actual fields (`Status`, `LastError`, `ResultSummary`, `NumTurns`, `CostUSD`, etc.) are documented in `internal/store/run_state.go` and don't show up in IDE tooltips that look only at the `pkg/orchestra` package.

**What surprised.** Had to open the internal file to learn that valid `Status` values are `"pending"`, `"running"`, `"done"`, `"failed"`. Magic strings everywhere with no compile-time check.

**Suggestion.** Lift field-level godoc onto the alias declarations (or onto the underlying struct fields in `internal/store/run_state.go` so it shows through). Enumerate the canonical Status values as exported `orchestra.TeamStatusPending` / `TeamStatusRunning` / `TeamStatusDone` / `TeamStatusFailed` constants — mirror the existing `orchestra.BackendLocal` / `BackendManagedAgents` precedent.

## 4. No `Result.AllTeamsDone() bool` or overall status field

**Where.** [pkg/orchestra/types.go](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/types.go) `Result` struct.

**What surprised.** The PR-audit consumer needed an "overall success/failed" status to compute its exit code AND to render its report. Had to walk `result.Teams` and check each `Status != "done"` in two places independently — once for the exit code, once for the renderer. Standard "two source-of-truth" smell. Also, the contract "`Result` is non-nil even on partial failure" is documented on `Wait()` but not on `Run()` — and `Run` is the SDK's documented hot path for one-shot consumers. Had to trace through `run.go` → `Start` → `Wait` to confirm `Run` inherits the contract.

**Suggestion.** Add `(r *Result) AllTeamsDone() bool` that returns true when every entry in `r.Teams` has `Status == "done"`. Document the empty-Teams case explicitly (lean: `false`, since no teams done means no teams completed). Lift the "Result non-nil even on partial failure" contract into `Run`'s godoc, not just `Wait`'s.

## 5. `orchestra.WithWorkspaceDir` doesn't expose its default

**Where.** [pkg/orchestra/option.go:26](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/option.go).

**What surprised.** The godoc says "overrides the default `.orchestra` workspace location" — fine — but consumers building paths around the workspace (e.g. `filepath.Join(".orchestra", "pr-audit-"+pr)`) hard-code the literal `.orchestra` rather than referencing a constant. Had to read `option.go:26` (`workspaceDir: ".orchestra"`) to confirm the default.

**Suggestion.** Export the default path as `orchestra.DefaultWorkspaceDir` so consumers compose without hard-coding a string.

## 6. `orchestra.Lead` / `Member` / `Task` / `Coordinator` field godoc

**Where.** [pkg/orchestra/types.go](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/types.go) aliases — `Lead`, `Member`, `Task`, `Coordinator`.

**What surprised.** When constructing a `*Config` programmatically and wanting to set per-team model overrides, the IDE tooltip for `Team.Lead.Model` says `type Lead = config.Lead` and stops there. No field-level documentation. Same for `Member.Role` / `Member.Focus`, `Task.Summary` / `Task.Details` / `Task.Deliverables` / `Task.Verify`, `Coordinator.Enabled` / `Model` / `MaxTurns`.

**Suggestion.** One-line field-level godoc on every aliased struct, even something minimal like `// Model overrides Defaults.Model when non-empty.`.

## 7. No `Config.String()` / `Config.MarshalYAML()` for debug dumps

**Where.** N/A — the helper doesn't exist.

**What surprised.** Wanted to dump the resolved config to stderr while debugging the PR-audit consumer to verify what the engine would see. There's no `cfg.String()` or `cfg.MarshalYAML()` exposed, and the consumer was constrained to stdlib + `pkg/orchestra` (no transitive yaml import allowed). Had to give up on the dump.

**Suggestion.** Either add `Config.String() string` (yaml.Marshal output) or document that `gopkg.in/yaml.v3` is a transitive dep callers can rely on for direct marshaling.

## 8. `orchestra.Run`'s godoc doesn't link to `WithEventHandler`

**Where.** [pkg/orchestra/handle.go:49-77](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/handle.go) — `Run` function godoc (the function lives in `handle.go`, not `run.go`).

**What surprised.** [pkg/orchestra/option.go:79](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/option.go) defines `WithEventHandler` for one-shot `Run` callers who want events without managing a `Handle` goroutine. The doc on `Run` mentions options but doesn't cross-link to the recommended one for synchronous progress reporting. Built the consumer initially without progress reporting because the option wasn't discoverable from `Run`'s godoc; found it later by reading `option.go` cold.

**Suggestion.** The `Run` godoc should explicitly recommend "for progress reporting in a one-shot run, see [WithEventHandler]."

## 9. `orchestra.Validate(nil)` `Errors[0].Field` is empty (matches "project-level" convention)

**Where.** [pkg/orchestra/run.go:60-64](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/run.go).

**What surprised.** P2.5 §5.3 "Field path conventions" says project-level issues get an empty `Field` slice. So `Validate(nil)` correctly returns `Field: nil`. But `Validate(nil)` is a degenerate case — there's no project at all. An explicit `Field: ["config"]` would let consumers distinguish "your config has no project name" (project-level issue inside the config) from "you passed nil" (no config at all). The behavior is documented now (`Validate`'s godoc explicitly says "A nil cfg is treated as a hard validation failure (one ConfigError entry, empty Field) rather than a panic"), so this is shape-only — the docs no longer require source-diving to understand it.

**Suggestion.** Tweak `pkg/orchestra/run.go` to return `&ValidationResult{Errors: []ConfigError{{Field: []string{"config"}, Message: "nil config"}}}` for nil cfg.

## 10. `WithWorkspaceDir(t.TempDir())` test pattern undocumented

**Where.** [pkg/orchestra/option.go](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/option.go) — `WithWorkspaceDir` godoc.

**What surprised.** Writing tests that drive `orchestra.Run` against an ephemeral workspace requires `WithWorkspaceDir(t.TempDir())`. No godoc example shows this pattern; new SDK consumers who write tests for their tooling will reinvent the lookup.

**Suggestion.** Add a one-line example to the godoc on `WithWorkspaceDir`: `orchestra.WithWorkspaceDir(t.TempDir())` is the standard test pattern.

## 11. `pkg/orchestra` pulls full transitive dep graph

**Where.** [go.mod](https://github.com/itsHabib/orchestra/blob/main/go.mod) — `pkg/orchestra` transitively depends on yaml.v3, fatih/color, anthropic-sdk-go, gofrs/flock, etc.

**What surprised.** A sample consumer that uses none of those (no YAML parsing, no MA backend, no terminal coloring) still pulls them all transitively. A built consumer binary is around 16 MB.

**Suggestion (lower priority).** Consider a `pkg/orchestra/lite` subpackage that re-exports just `Config`, `Run`, `Result`, and the option/error sentinels — without dragging in the yaml/cobra/anthropic deps. Out of scope for any near-term release; flagged for the day a real polyglot service-mesh consumer cares.

## 12. `Result.Tiers` vs `Result.Teams` invariant unclear

**Where.** [pkg/orchestra/types.go](https://github.com/itsHabib/orchestra/blob/main/pkg/orchestra/types.go) `Result.Tiers` field godoc.

**What surprised.** `Tiers [][]string` is documented as "the tier-by-tier team-name layout, for ordered rendering" but doesn't say whether team names appearing in `Tiers` are guaranteed to be keys in `Teams`. The PR-audit renderer defensively skips entries missing from `Teams` — should renderers have to be defensive, or is the invariant strict?

**Suggestion.** Document the invariant explicitly. Lean: strict subset (`Tiers[i][j] is always a key in Teams` for completed runs; `Teams` may contain entries not yet in `Tiers` mid-run if `Status` is called before tier scheduling completes).

---

## Methodology

A small CLI consumer was built end-to-end (~1,800 LOC of Go + tests + testdata + design doc + README) that takes a GitHub PR number, builds an `orchestra.Config` programmatically, runs an audit team via `orchestra.Run`, and prints a structured report. The tool was reviewed and CI-verified before being thrown away based on the "doesn't belong in the tree" feedback. The implementation lives at [closed PR #22](https://github.com/itsHabib/orchestra/pull/22) for anyone wanting to revive it.

Findings 1–4 were debugging-level surprises (had to read source to make progress). 5–8 were "the API works but a helper would have made this trivial." 9–12 are polish.

Empty findings would have been a finding too. Twelve is a lot for a 1,800-line consumer that did nothing exotic — the SDK works, but its discoverability and ergonomics could be better.
