# Feature: P2.0 — Minimum public SDK surface

Status: **Proposed**
Owner: @itsHabib
Depends on: P1.6 ([06-p16-multi-team-text-only.md](./06-p16-multi-team-text-only.md)) (shipped)
Relates to: [02-pkg-to-internal.md](./02-pkg-to-internal.md) — this chapter triggers the "internal until SDK extraction is real work" condition; [DESIGN-v2.md](../DESIGN-v2.md) §13 Phase 2 (P2.1–P2.3 follow this chapter).
Target: a single `pkg/orchestra` package exposing exactly enough surface to drive an orchestra run from another Go program — no more.

---

## 1. Why this chapter exists (and why now)

The repo-wide policy ([02-pkg-to-internal.md](./02-pkg-to-internal.md)) is "everything stays under `internal/` until SDK extraction is real work, with a real consumer." Until P1.6 shipped, the only consumer was the in-tree CLI. The dogfooding intent — building tools (PR audit, self-maintaining sweeper, spike harness) on top of orchestra rather than alongside it — turns "potential SDK consumer" into "actual SDK consumer." The trigger condition is met.

This chapter exposes the **minimum** surface that lets a Go program do the equivalent of `orchestra run config.yaml` — load (or construct) a config, run it, read per-team results. Nothing more. It deliberately defers spawner / store / dag / injection / agents / run-service surfaces to later chapters (or never, if no consumer asks).

The strategic payoff: every dogfood app is a free design review. The first one (PR audit, planned next) will tell us within days whether the surface in this chapter is right; if it isn't, we change it before any external consumers exist. The cost of a wrong call here is one local refactor, not a public-API break.

---

## 2. Requirements

**Functional.**

- F1. A new package `pkg/orchestra` exists at the repo root. It compiles with `go build ./...` and is reachable via the module path `github.com/itsHabib/orchestra/pkg/orchestra`.
- F2. The package re-exports the YAML schema types via Go type aliases (not struct copies): `Config`, `Defaults`, `Backend`, `Coordinator`, `Team`, `Lead`, `Member`, `Task`. Aliases keep the source of truth in `internal/config` so that fields added there (e.g. P1.5's `ManagedAgentsBackend.Repository`) flow through automatically.
- F3. The package re-exports the run-state observation types via aliases: `RunState`, `TeamState`, `RepositoryArtifact` (when P1.5 lands). Source of truth stays in `internal/store`.
- F4. The package exposes a top-level `Run(ctx context.Context, cfg *Config, opts ...Option) (*Result, error)` function that performs the equivalent of `orchestra run`: validate config, build DAG, execute tiers, return results. `Run` takes ownership of `cfg` for the call duration: it may invoke `ResolveDefaults` and `Validate` on the pointer; concurrent caller mutation of the same `*Config` while `Run` is in flight is undefined behavior. Callers wanting to share a config across goroutines must clone it.
- F5. `Result` exposes per-team status, result summary, cost, duration, session ID, **per-team turn count and token counters**, and (when P1.5 lands) repository artifacts. It is read-only and self-contained — callers get the full picture without dipping into `internal/`. The CLI summary renderer (`printSummary`) is migrated to read every field from `Result`, not from disk.
- F6. The package exposes a `LoadConfig(path string) (*Config, []Warning, error)` helper so callers don't have to reimplement YAML loading. Warnings are returned even when `err != nil` (validation failure preserves the warning slice for context).
- F7. The CLI (`cmd/run.go`) is migrated to use `pkg/orchestra.Run` as its entry point. The CLI is the SDK's first consumer; if the surface is awkward there, it's awkward for everyone.
- F8. The package defines an `orchestra.Logger` interface capturing exactly the methods the orchestration loop calls today (`TeamMsg`, `TierStart`, `Info`, `Warn`, `Error`, `Success`). `internal/log.Logger` is made to satisfy it without behavior change. SDK callers either (a) pass the existing `internal/log.New()` (re-exported as `orchestra.NewCLILogger()` for the colored stdout default) or (b) supply their own implementation — typically a no-op for library callers, an `slog`-bridging adapter for callers that already have an slog tree, or a custom one for UI integrations.
- F9. `Run` guarantees that all subprocesses it spawned (coordinator, team agents) are stopped before it returns, even on early-tier failure or context cancellation. `defer` discipline in the extracted body, not best-effort.
- F10. Concurrent `Run` invocations against the same workspace from the same process return `ErrRunInProgress` (new exported sentinel) rather than racing for the workspace lock. Different `WithWorkspaceDir` values across goroutines are explicitly supported.

**Non-functional.**

- NF1. **Type aliases, not copies.** A `type Config = config.Config` declaration. Adding a field in `internal/config.Config` requires zero changes in `pkg/orchestra`. A struct copy here would force every internal-config change into a double-edit and slowly drift; aliases make the boundary explicit and free.
- NF2. **Stability tier: experimental.** The package godoc declares the surface is experimental — breaking changes allowed without a major-version bump until P2.1 stabilizes it. This chapter is *exposure*, not *commitment*.
- NF3. **No transitive `internal/` exposure.** A consumer importing only `pkg/orchestra` must never see, depend on, or need to reference `internal/...`. The Go compiler enforces this by default once `pkg/orchestra` is the only public package; this chapter just has to not violate it.
- NF4. **Behavior parity with the CLI.** A program calling `orchestra.Run(ctx, cfg)` produces the same run state, logs, results, and side effects as `orchestra run config.yaml` over the equivalent config. The CLI migration in F7 is the parity test.
- NF5. **No new dependencies.** This is a re-export + small entry-point package. It pulls in nothing the existing tree doesn't already use.

---

## 3. Scope

### In-scope

- New package `pkg/orchestra/` containing `orchestra.go` (types, Run, LoadConfig, Result, Option), `doc.go` (package godoc with the experimental marker), `option.go` (Option implementation if it grows beyond a couple of fields).
- Type-alias declarations re-exporting from `internal/config` and `internal/store`.
- A `Run(ctx, cfg, opts...) (*Result, error)` function implemented by extracting the body of `cmd/run.go:runOrchestration` into the new package (or by having `runOrchestration` delegate to it).
- An `Option` type for caller-side knobs — at minimum, a `WithLogger(*slog.Logger)` for callers that don't want orchestra's colored stdout, and a `WithWorkspaceDir(string)` for callers that don't want `.orchestra/` in `os.Getwd()`.
- Migration of `cmd/run.go` to call `pkg/orchestra.Run` instead of duplicating the orchestration loop.
- Godoc on every exported identifier, with a clear "experimental" banner on the package doc.
- A short test file `pkg/orchestra/orchestra_test.go` that imports the package and exercises `LoadConfig` + a tiny mock-backed `Run` against a one-team config — proving the surface is reachable from outside `internal/`.

### Out-of-scope

- **Spawner-as-public.** `internal/spawner` stays internal. No consumer has asked for "bring your own backend" yet; expose only when one does.
- **Store-as-public.** Same. The `Result` type gives callers everything the CLI gives a human; raw store access is implementation.
- **DAG-as-public.** Tier construction is internal. If a consumer wants tier visibility, expose it via `Result.Tiers []string[]` later — don't expose `dag.BuildTiers`.
- **Injection-as-public.** Prompt construction is internal. Custom prompts would be a separate feature with its own doc.
- **Run-service / messaging / workspace.** Implementation. The `Run` entry point hides these.
- **Stability commitment.** P2.1 marks the surface stable. This chapter explicitly does not.
- **`v0.1.0` tag.** P2.3.
- **The first dogfood app.** Lives in a follow-up chapter / its own directory (e.g. `tools/pr-audit/`). This chapter just makes building one possible.
- **An `Option` for swapping the CLI logger.** The dogfood apps will tell us what options matter; resist front-loading the option set.

---

## 4. Data model / API

### 4.1 Package skeleton

```go
// Package orchestra is the experimental Go SDK for the orchestra workflow
// engine. Surface is unstable; expect breaking changes without semver
// signaling until P2.1 stabilizes the surface. See CHANGELOG.md.
//
// Typical use:
//
//	cfg, warnings, err := orchestra.LoadConfig("orchestra.yaml")
//	for _, w := range warnings { fmt.Fprintln(os.Stderr, w) }
//	if err != nil { return err }
//	res, err := orchestra.Run(ctx, cfg)
//	if err != nil { return err }
//	for name, team := range res.Teams {
//	    fmt.Printf("%s: %s (%d turns, %.2f USD)\n",
//	        name, team.Status, team.NumTurns, team.CostUSD)
//	}
package orchestra

import (
    "context"
    "errors"

    "github.com/itsHabib/orchestra/internal/config"
    "github.com/itsHabib/orchestra/internal/log"
    "github.com/itsHabib/orchestra/internal/store"
)

// Config is the YAML schema. Aliased from internal/config; field additions
// there flow through transparently.
type (
    Config      = config.Config
    Defaults    = config.Defaults
    Backend     = config.Backend
    Coordinator = config.Coordinator
    Team        = config.Team
    Lead        = config.Lead
    Member      = config.Member
    Task        = config.Task
    Warning     = config.Warning
)

// RunState and friends are observation types describing what a run produced.
// Aliased from internal/store; same propagation rule.
type (
    RunState  = store.RunState
    TeamState = store.TeamState // includes NumTurns + token counters after this chapter
)

// Backend kind constants — typo-safety wrapper over the free-form
// Backend.Kind string.
const (
    BackendLocal         = "local"
    BackendManagedAgents = "managed_agents"
)

// Sentinel errors. Stable across breaking surface changes.
var (
    // ErrRunInProgress is returned by Run when another Run invocation against
    // the same workspace (within this process) is already in flight.
    ErrRunInProgress = errors.New("orchestra: run already in progress for workspace")
)

// Result is the SDK's view of a completed (or partially completed) run. All
// per-team data needed by the CLI summary renderer lives here — callers do
// not need to read .orchestra/results/ off disk.
type Result struct {
    Project    string
    Teams      map[string]TeamResult // richer than TeamState; see TeamResult below
    Tiers      [][]string            // tier-by-tier team-name layout, for ordered rendering
    DurationMs int64
}

// TeamResult is the SDK-shaped per-team view: a TeamState plus the fields the
// CLI's printSummary today reads from disk via workspace.TeamResult.
type TeamResult struct {
    TeamState           // embedded — Status, ResultSummary, CostUSD, DurationMs, SessionID, AgentID, etc.
    NumTurns      int
    InputTokens   int64
    OutputTokens  int64
    // Future P1.5 fields (RepositoryArtifacts) will be on TeamState.
}

// Logger is the orchestration loop's logging dependency. Library callers
// typically pass NewNoopLogger; CLI callers pass NewCLILogger.
type Logger interface {
    TeamMsg(team, format string, args ...any)
    TierStart(tierIdx int, teams []string)
    Info(format string, args ...any)
    Warn(format string, args ...any)
    Error(format string, args ...any)
    Success(format string, args ...any)
}

// NewCLILogger returns the colored, mutex-guarded stdout logger orchestra's
// CLI uses. internal/log.Logger satisfies orchestra.Logger.
func NewCLILogger() Logger { return log.New() }

// NewNoopLogger returns a Logger that discards all output. The default if no
// WithLogger option is supplied.
func NewNoopLogger() Logger { /* ... */ }

// Run executes the workflow described by cfg and returns its result. ctx is
// honored throughout; on cancellation, in-flight teams are cancelled and all
// spawned subprocesses (team agents, coordinator) are stopped before Run
// returns. The returned *Result reflects whatever state was reached, even on
// error.
//
// Run takes ownership of cfg for the call duration. It may call
// ResolveDefaults / Validate on the pointer; concurrent caller mutation is
// undefined behavior. Callers sharing a Config across goroutines must clone.
//
// Concurrent Run invocations from the same process targeting the same
// WithWorkspaceDir return ErrRunInProgress.
func Run(ctx context.Context, cfg *Config, opts ...Option) (*Result, error)

// LoadConfig parses a YAML config from path, applies defaults, and runs
// validation. Returns warnings even when err != nil (validation failure
// preserves them for context). Callers should print warnings before checking
// err — the CLI does this in cmd/run.go.
func LoadConfig(path string) (*Config, []Warning, error)

// Validate runs the config validator standalone. Useful for callers that
// build configs programmatically. Mirrors what Run does internally.
func Validate(cfg *Config) ([]Warning, error)

// Option configures a single Run invocation.
type Option func(*runOptions)

// WithLogger overrides the default no-op logger. CLI callers typically pass
// NewCLILogger(); library callers typically leave the default; UI callers
// supply a custom implementation.
func WithLogger(logger Logger) Option

// WithWorkspaceDir overrides the default .orchestra/ workspace location.
// Relative paths are resolved against os.Getwd() at Run call time, matching
// CLI behavior. Default: ".orchestra".
func WithWorkspaceDir(path string) Option
```

### 4.2 Implementation strategy

`Run` is implemented by extracting the body of `cmd/run.go:runOrchestration` into the new package. The CLI's `runCmd.Run` becomes a thin wrapper:

```go
// cmd/run.go (post-migration)
Run: func(_ *cobra.Command, args []string) {
    cfg, warnings, err := orchestra.LoadConfig(args[0])
    if err != nil { /* print + exit */ }
    for _, w := range warnings { /* print */ }

    res, err := orchestra.Run(context.Background(), cfg,
        orchestra.WithLogger(cliLogger),
        orchestra.WithWorkspaceDir(workspaceDir),
    )
    if err != nil { /* print + exit */ }
    printSummary(cliLogger, res, time.Since(wallStart))
},
```

`internal/run`, `internal/spawner`, etc. stay where they are. The new `pkg/orchestra.Run` is the only thing that knows about all of them.

---

## 5. Engineering decisions

### 5.1 Type aliases over struct copies

A `type Config = config.Config` is a Go type alias (not a definition). Adding a field to `config.Config` adds it to `orchestra.Config` automatically. A `type Config struct { ... }` declaration here would force every internal schema change to be applied twice — and the second application would silently drift.

P1.5 will add `ManagedAgentsBackend.Repository`, `EnvironmentOverride`, `RepositoryArtifact`, etc. to `internal/config` and `internal/store`. With aliases, P1.5 changes nothing in `pkg/orchestra` and the new fields are visible to SDK consumers immediately. With copies, P1.5 PRs would each need a parallel update here — and reviewers would have to police the parallelism.

The cost of aliases is that callers see `internal/config` types in their godoc / IDE tooltips. That's a minor cosmetic loss versus the major maintenance loss of copies. Aliases win.

### 5.2 `Run` returns `*Result`, not `(state, errs, fatal)` tuple

The CLI today distinguishes "team failed" (run continues partially, fatal at end) from "engine errored" (orchestration aborted). `Run` collapses these for the SDK: a team failure is a `team.Status == "failed"` entry in `Result.Teams` plus a non-nil `error` returned at the end. Callers who want to inspect partial state read `Result` even when `err != nil`.

Alternatives considered: (a) return `(*Result, []TeamFailure, error)` — three returns is awkward. (b) put errors inside `Result.Error` — confuses Go's idiomatic `if err != nil` pattern. (c) silent failure — never. The single-error single-result pair is the smallest surface that gives callers everything.

### 5.3 Experimental tier marker — godoc + per-identifier notes + CHANGELOG.md discipline

Common alternatives for "this is unstable":
- `pkg/orchestra/v0/` or `pkg/orchestra/v0alpha/` — semver `v0` explicitly disclaims stability and the path is unmistakable in import statements; the cost is one `git mv` plus consumer rewrites at stabilization
- `pkg/orchestra/experimental/` subpackage — same fork cost as `v0/`
- `Experimental_` prefix on every exported identifier — ugly but unmistakable in IDE tooltips
- Build tag — invisible to godoc / IDE
- godoc banner only — weakest signal; reviewers have noted (correctly) that godoc-only markers are widely ignored in practice

The choice for P2.0 is **godoc banner + per-identifier "Experimental:" note + CHANGELOG.md discipline** — i.e., every PR that changes the public surface adds a CHANGELOG entry, and we accept the up-front cost that the first dogfood app's import will need to be rewritten when P2.1 stabilizes. The cost of stronger signals (path-based versioning, identifier prefixes) was traded against the cost of one consumer migration; the consumer migration loses (it's one `sed` in a repo we own).

If a real external consumer materializes before P2.1, this trade re-opens and a `v0/` path move becomes the right call. The trigger is "external import," not "we feel unsure" — we're already unsure.

### 5.4 Run takes ownership of `*Config` for the call duration

`Run(ctx, cfg, ...)` accepts a pointer because (a) the underlying `internal/config.Validate()` and `ResolveDefaults()` are pointer-receiver methods, (b) the engine's `runOrchestration` extraction needs to mutate the pointer to apply defaults, and (c) by-value would copy ~few-KB structs per invocation when concurrent SDK callers exist.

The contract: `Run` may call `ResolveDefaults` and `Validate` on `cfg` (which mutates it), it does not retain the pointer beyond the call, and concurrent caller mutation while `Run` is in flight is undefined. SDK callers wanting to share a config across goroutines clone with a small helper:

```go
func CloneConfig(c *Config) *Config { /* trivial deep copy */ }
```

This helper ships in P2.0 because anyone running concurrent `Run`s will hit it.

### 5.5 Concurrent-Run protection via `ErrRunInProgress`

`runsvc.Begin` (`internal/run/service.go`) takes an exclusive workspace lock and `archivePrevious` archives whatever run is currently in `state.json`. Two SDK callers in the same process running concurrently against the same `WithWorkspaceDir` will today have the second `Begin` cannibalize the first's in-flight run. P2.0 adds an in-process workspace registry:

```go
// inside pkg/orchestra
var workspaceMu sync.Mutex
var activeWorkspaces = map[string]struct{}{}
```

`Run` enters by attempting to register the absolute workspace path; if already registered, returns `ErrRunInProgress`. Different workspaces are independent. The cross-process case is still handled by the existing `store.AcquireRunLock` exclusive lock (no change there).

### 5.6 `Run` guarantees subprocess cleanup before return

The current `runOrchestration` body calls `r.startCoordinator` and relies on `r.stopCoordinator` for cleanup. On early-tier failure, `runTiers` returns first and `stopCoordinator` is not called — a leak that is invisible because the CLI process exits and the OS reaps the child. In an SDK context (long-lived parent process), the leak is real.

P2.0 adds an explicit `defer` of all subprocess teardown in the extracted body and an acceptance-criterion test that asserts no orchestra-spawned processes survive `Run`'s return on the early-tier-failure path. This is a real bug fix bundled with the SDK extraction.

### 5.7 `Result` carries everything `printSummary` needs (no disk reads in the SDK path)

Today's `printSummary` (`cmd/run_summary.go`) reads per-team `NumTurns` from `workspace.TeamResult` on disk. The SDK extraction must not require disk reads to render a summary, so `Result.Teams` exposes a richer `TeamResult` struct that embeds `TeamState` and adds `NumTurns`, `InputTokens`, `OutputTokens`. The CLI's migrated `printSummary` reads only from `Result`.

Schema-side, `internal/store.TeamState` is extended to record `NumTurns` (it already records token counters). One small additive store change, justified as part of this chapter rather than as a "while we're here" drive-by.

### 5.8 Migrate the CLI as part of this chapter

The CLI is the SDK's first consumer. If we ship `pkg/orchestra` without migrating the CLI, the package gets no exercise until the first dogfood app — and any surface mistakes ship with it. Migrating the CLI here costs ~50 lines of re-plumbing in `cmd/run.go` and gives us free regression coverage: every existing CLI test exercises the SDK.

### 5.9 No `WithStore` / `WithSpawner` options

Tempting to expose pluggable backends ("bring your own session API for testing"). Resist. The CLI doesn't need it; the dogfood apps don't need it; and exposing `Spawner` makes `internal/spawner` effectively public. Tests inside the orchestra repo continue to use the internal test substitutes; SDK consumers test against real or recorded MA — not a fake spawner.

If a real consumer asks, add it then.

### 5.10 `Result` is a snapshot, not a stream

`Run` blocks until the run completes (or ctx cancels) and returns a single `Result`. Callers who want progress events can pass `WithLogger(myLogger)` and observe per-team / per-tier events as they arrive. A streaming events channel would force every consumer to handle backpressure, ordering, and partial-state semantics that the CLI hides today. Snapshot-on-completion plus a logger for live events covers both UX modes.


---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Type aliases re-exporting from `internal/` | Struct copies, code generation, or building the SDK as the source of truth and re-exporting back to `internal/` | Aliases are zero-maintenance and free; copies double-edit; codegen is overkill; flipping the source of truth to `pkg/` upends the current tree for no gain at this stage |
| Single-package `pkg/orchestra` | Multi-package layout (`pkg/orchestra/config`, `pkg/orchestra/run`, etc.) | One package matches the consumer's mental model — "I want to run an orchestra"; multiple packages are premature partitioning |
| Migrate CLI here | Migrate CLI in a follow-up | Free regression coverage and immediate exercise of the surface |
| `Run` blocks, returns snapshot; live events via `Logger` | Streaming events channel | No consumer demands streams; snapshot matches CLI; the Logger interface gives live events without backpressure plumbing |
| godoc + per-identifier marker + CHANGELOG | `v0/` path, `experimental/` subpackage, `Experimental_` prefix on every export, build tag | Reviewers correctly noted godoc-only is weak in practice; we accept that and trade against the cost of moving the path. Trigger to escalate: any external import. |
| `Result` carries TeamResult (TeamState + NumTurns + tokens) | Expose internal `workspace.TeamResult` directly via alias; or extend `TeamState` with all fields | Aliasing internal `workspace` types couples SDK to a layout that may change; extending `TeamState` with NumTurns is a small additive change worth doing here, and the SDK-shaped `TeamResult` keeps token/turn counters in one obvious place |
| `Run` takes ownership of `*Config` | By-value `Config` to be safe; or document immutability and clone internally | By-value copies on every call when concurrent SDK use is the explicit goal; explicit ownership contract + a `CloneConfig` helper for shared-config cases is cheaper |
| Process-local concurrent-Run protection | Cross-process protection only via the existing exclusive workspace lock | The exclusive lock catches it across processes; in-process callers want a typed sentinel (`ErrRunInProgress`) instead of an opaque file-lock-contention error |
| `Run` guarantees subprocess cleanup | Best-effort cleanup that works for the CLI (process exits anyway) | SDK callers don't exit; the bug is invisible today and ships under the SDK without the fix |
| No `WithSpawner` / `WithStore` | Pluggable backends from day one | Exposes internals; no consumer asks; testing inside the repo continues to use internal substitutes |
| `LoadConfig` returns `(cfg, warnings, err)` | `(cfg, err)` only, hide warnings | Warnings are part of orchestra's UX (printed by CLI); SDK callers should see them too — preserved even on err |
| `Logger` interface (TeamMsg/TierStart/...) | Pass `*slog.Logger`; bridge inside the engine | The engine uses the colored `internal/log.Logger` everywhere — slog can't replace it without a separate refactor; an SDK interface gives callers a clean integration point |

---

## 7. Testing

### 7.1 Unit tests inside `pkg/orchestra/`

- `orchestra_test.go` imports the package as an external consumer would (`package orchestra_test`) and exercises `LoadConfig` against a fixture YAML, asserting the returned `*Config` has the expected fields (proves the alias works for nested types like `Backend`'s custom `UnmarshalYAML`).
- Run a one-team orchestra against a mock-claude-script fixture (reuse the helper from `e2e_test.go`) using the local backend. Assert `Result.Teams[name].Status == "done"`, `NumTurns > 0`, `InputTokens > 0`, `CostUSD > 0`. Proves `Result` carries everything `printSummary` needs without disk reads.
- Concurrent-`Run` test: kick two `Run` calls against the same workspace from two goroutines; assert one wins and the other returns `ErrRunInProgress` (not a corrupted state.json). Pair with a third goroutine using a different workspace to assert different workspaces are independent.
- Subprocess-cleanup test: build a config whose tier-0 team always fails (mock-claude exits non-zero); assert that after `Run` returns the error, no orchestra-spawned processes remain. Use `runtime.Goexit`/`os.Process` enumeration; if too platform-specific, assert via the absence of a sentinel marker file the coordinator would write.
- Config-mutation test: pass a `*Config` with `Defaults.Model == ""`, assert that after `Run` returns, `cfg.Defaults.Model == "sonnet"` (i.e., `ResolveDefaults` ran on the caller's pointer). Documents the ownership contract explicitly.

### 7.2 CLI parity

The existing `e2e_test.go` already builds the binary and runs end-to-end. After CLI migration to `pkg/orchestra.Run`, this test exercises the SDK transitively.

**Coverage baseline.** Before the migration, audit `e2e_test.go` and document in this chapter what it actually exercises (local backend happy path? failure paths? coordinator? MA?). If the coverage is local-backend-only (likely), call out explicitly that the SDK migration ships with no regression coverage on the MA path beyond the unit tests; rely on the `make e2e-ma-*` fixtures (which now also flow through the SDK) to catch MA-specific regressions.

### 7.3 What we explicitly do not test

- SDK-level live MA via `pkg/orchestra/orchestra_test.go` directly. Live MA continues to run only via the existing `e2e-ma-*` make targets through the CLI; once the CLI delegates to the SDK, that coverage flows through automatically.
- Pluggable backend scenarios. Out of scope by §5.9.
- Streaming events channel. Out of scope by §5.10 — Logger covers the use case.

---

## 8. Rollout

Single PR. The work is mostly file moves and a small entry-point function:

1. Create `pkg/orchestra/{orchestra.go,doc.go,option.go,orchestra_test.go}`.
2. Add type aliases for the schema and observation types.
3. Extract `cmd/run.go:runOrchestration` body into `pkg/orchestra.Run`. The internal services (`runsvc`, `spawner`, `dag`, etc.) stay where they are; only the orchestration entry point moves.
4. Migrate `cmd/run.go` to call `orchestra.Run`. Keep CLI-only concerns (cobra wiring, stdout summary, exit codes) in `cmd/`.
5. Add `WithLogger` and `WithWorkspaceDir` options. No others.
6. Godoc + experimental banner on the package.
7. Update README to add a one-paragraph "Use as a Go library" section pointing at the package.

**Rollback.** Pure revert. No schema changes, no migration. The CLI continues to work either way.

**Migration policy for downstream chapters.** P1.5 and P1.9-A target `internal/` types as today; the type aliases in `pkg/orchestra` pick up additions automatically. No coordination required.

---

## 9. Observability & error handling

- The default logger writes nothing (a `slog.New(slog.NewTextHandler(io.Discard, nil))`) — library callers who want output supply `WithLogger`. The CLI keeps the colored stdout logger by passing it via `WithLogger`.
- `Run` returns wrapped errors using `fmt.Errorf("%w", err)` so callers can `errors.Is` against the underlying cause (e.g. `context.Canceled`).
- `LoadConfig` distinguishes parse errors (return immediately) from validation warnings (return alongside `nil` error). Hard validation errors come back as `err` with the warnings slice populated for context.

---

## 10. Open questions

Genuinely open — not yet decided:

1. **Should `Run` accept a `context.Context` per team or a single context?** Today it's one. Per-team cancellation has no current consumer and complicates the cancellation story. Lean: defer until a dogfood app asks. Marker: a fan-out app that wants to time-bound individual teams differently.
2. **CLI surface coverage in `e2e_test.go`** (paired with §7.2 coverage baseline). If the audit shows the existing e2e covers only local-backend happy path, do we widen it to local-backend failure + cancellation paths as part of this chapter, or accept the gap and rely on `make e2e-ma-*` for the rest? Decide during implementation after running the audit.
3. **Plan / Status SDK entry points** (`Plan(*Config) *Plan`, `Status(workspaceDir) *Status`). Not in this chapter. Add when a dogfood app needs them.
4. **Module path stabilization at P2.1.** When P2.1 marks the surface stable: keep aliases as-is, flip source of truth from `internal/config` to `pkg/orchestra`, or split the surface into multiple packages? Decide with the first dogfood app's feedback. Until P2.1, breakage is accepted and not semver-bounded — documented in the package godoc.
5. **`pkg/orchestra` vs bare-root package path.** The bare-root option (consumers import `github.com/itsHabib/orchestra` directly) is more idiomatic for single-package libraries but conflicts with the existing top-level `main` package. `pkg/orchestra` keeps the CLI binary and the library cleanly separated. Lean: stick with `pkg/orchestra`. Revisit only if module restructuring lands for unrelated reasons.

Decided in this chapter (no longer open):

- **`Result.Tiers [][]string`** — included from day one. Cheap, the CLI already has tier ordering during execution, and dogfood apps that render progress per tier (PR audit) want it.
- **`BackendLocal` / `BackendManagedAgents` const set** — exported. Three lines, prevents typo bugs.
- **Standalone `Validate(*Config) ([]Warning, error)`** — exported. Programmatic-config consumers need it before calling `Run`.
- **Relative-path resolution for `WithWorkspaceDir`** — relative to `os.Getwd()` at `Run` call time, matching CLI behavior. Documented in godoc.

---

## 11. Acceptance criteria

- [ ] `pkg/orchestra/` exists with `orchestra.go`, `doc.go`, `option.go`, `logger.go`, `orchestra_test.go`.
- [ ] `go doc github.com/itsHabib/orchestra/pkg/orchestra` prints package godoc with the experimental banner and the exported identifiers (`Config`, `Run`, `LoadConfig`, `Validate`, `Result`, `TeamResult`, `Logger`, `Option`, `WithLogger`, `WithWorkspaceDir`, `NewCLILogger`, `NewNoopLogger`, `BackendLocal`, `BackendManagedAgents`, `ErrRunInProgress`, `CloneConfig`).
- [ ] `cmd/run.go` calls `orchestra.Run`; the orchestration loop body lives in `pkg/orchestra` only. `printSummary` reads only from `Result` (no `workspace.ReadResult` calls).
- [ ] `internal/store.TeamState` gains `NumTurns` field, populated during runs.
- [ ] `e2e_test.go` (existing) passes unchanged — proves CLI parity.
- [ ] `pkg/orchestra/orchestra_test.go` passes — proves the package is reachable as an external consumer and exercises: alias correctness on `Backend.UnmarshalYAML`, one-team mock-claude run with full `Result` fields populated, concurrent-`Run` returns `ErrRunInProgress`, subprocess cleanup on tier-0 failure, `ResolveDefaults` mutation of caller's `*Config` is observable.
- [ ] `make test && make vet && make lint` green.
- [ ] README has a "Use as a Go library" section pointing at `pkg/orchestra`.
- [ ] CHANGELOG.md exists at repo root with a P2.0 entry — establishes the discipline §5.3 commits to.
- [ ] Follow-ups tracked (not shipped here): the first dogfood app, P2.1 stabilization, P2.2 `examples/embed/`, P2.3 `v0.1.0`, widening of `e2e_test.go` coverage if the audit (§7.2 baseline) shows gaps.
