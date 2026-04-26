# Feature: P2.5 — `ValidationResult` for `LoadConfig` / `Validate`

Status: **Proposed**
Owner: @itsHabib
Depends on: P2.0 ([07-p20-minimum-sdk-surface.md](./07-p20-minimum-sdk-surface.md)) (shipped), P2.4 ([10-p24-operational-sdk.md](./10-p24-operational-sdk.md)) (shipped)
Relates to: [DESIGN-v2.md](../DESIGN-v2.md) §13 Phase 2 — the second of two slim follow-ups deferred from P2.4 (see P2.4 §1, §3 out-of-scope, §8 Q6). Strongly recommended to land before tagging v0.1.0 so the first tagged release has the validation surface settled.
Target: replace the `(*Config, []Warning, error)` and `([]Warning, error)` tuples returned by `pkg/orchestra.LoadConfig` / `pkg/orchestra.Validate` with a single `*ValidationResult` value that carries the parsed `*Config`, structured `Warnings`, structured `Errors` (each with a field path), and convenience methods (`Valid() bool`, `Err() error`).

---

## 1. Why this chapter exists (and why now)

P2.4 named this reshape but explicitly deferred it ("the decide-loadconfig dogfood synthesis is **not** part of this chapter — it covers a different surface and lands as its own slim follow-up", P2.4 §1). The reasons to do it now, before v0.1.0:

- **The current tuple forces every caller to demux `warnings` / `error` and gives up structure on the way.** Hard validation errors are joined into one opaque string by `Config.Validate` ([internal/config/schema.go:218-220](../../internal/config/schema.go)):

  ```go
  return warnings, fmt.Errorf("validation errors:\n  - %s", strings.Join(errs, "\n  - "))
  ```

  An SDK consumer that wants to render a structured report (a TUI, a dashboard, an IDE squiggle, an audit tool) has to parse strings out of `err.Error()`. Field-level context (which team, which task, which yaml field) has already been thrown away by the time the tuple returns.

- **The asymmetry between warnings and errors is artificial.** Today warnings are typed (`Warning{Team, Message}`) and errors are stringly-typed. Both are validation issues — they should share a structure. The dogfood synthesis from P2.0 / P2.4 review concluded: lift errors to a typed `ConfigError` with the same shape as `Warning`, and bundle both with the parsed `*Config` into one `ValidationResult`.

- **`pkg/orchestra` is still Experimental and there are no external consumers pinned.** The acceptable-breaking window closes at v0.1.0. The runway after P2.4 is: (1) MA backend integration test in CI, (2) decide type-alias strategy for `RunState`/`TeamState`, (3) file extractions in `pkg/orchestra`, (4) flaky-test fix, (5) a real consumer on the SDK, (6) `CONTRIBUTING.md`. None of those need the validation surface to settle, but a real consumer (item 5) very much does.

- **CLI parity is trivially preserved.** `cmd/validate.go`, `cmd/run.go`, `cmd/plan.go`, `cmd/init_cmd.go`, `cmd/spawn.go` all consume the existing tuple in the same way (warn each warning, exit-1 on error). The CLI rendering becomes one iteration over `result.Warnings` and one print of `result.Err()` — byte-identical output via `Err()` preserving the `"validation errors:\n  - ..."` format.

The package is Experimental, no external consumers are pinned to P2.0's shapes, and P2.4 already established the precedent of replacing rather than extending. This chapter takes the same freedom for the validation surface.

---

## 2. Requirements

**Functional.**

- F1. A new `ValidationResult` type aggregates `*Config`, `[]Warning`, and `[]ConfigError` into a single value. Exported from `pkg/orchestra`.
- F2. `LoadConfig(path string) (*ValidationResult, error)` replaces the current `(*Config, []Warning, error)` shape. The `error` return is reserved for I/O / parse failures (file not found, malformed YAML); structural validation issues live in `result.Errors`. `result` is non-nil whenever `error == nil`.
- F3. `Validate(cfg *Config) *ValidationResult` replaces the current `([]Warning, error)` shape. No `error` return — `result.Err()` carries it. Validation has no I/O so no separate error class to surface.
- F4. `result.Valid() bool` returns `len(result.Errors) == 0`. Warnings do not affect validity.
- F5. `result.Err() error` returns `nil` when `Valid()`. Otherwise returns a wrapped error preserving the existing `"validation errors:\n  - <msg>\n  - <msg>..."` format so the CLI's `orchestra validate` output stays byte-identical and any caller doing `fmt.Println(err)` keeps working. The wrapped error is `errors.Is`-equal to a new sentinel `ErrInvalidConfig`.
- F6. `result.Config` is the parsed, defaults-resolved `*Config`. **Nil** when `Valid() == false` (i.e., hard validation errors) so consumers cannot accidentally hand an invalid config to `Run`. Also nil when `LoadConfig` returned a non-nil `error` (parse / IO failure).
- F7. `Warning` and `ConfigError` are two parallel types with the same field set: `Field []string`, `Team string`, `Message string`. They share semantics — only their semantic category differs. `Warning.String()` and `ConfigError.String()` preserve the existing `team %q: %s` / `%s` formats so CLI rendering is unchanged.
- F8. `Field []string` is the structured path to the offending YAML node, e.g. `["teams", "0", "tasks", "2", "verify"]` for a missing verify on team 0's third task. Empty slice means project-level (e.g. missing project name, unknown backend.kind). Populated by every existing validator in `internal/config/schema.go` and `internal/config/repository.go`; no validator omits it.
- F9. `Team string` denormalizes the team name when `Field` points into a team subtree. Empty otherwise. Exists for ergonomic display — matches today's `Warning.Team` field — so callers don't have to walk Field + index back into Config to print "team \"backend\": ...". Programmatic consumers should prefer `Field` as the source of truth.
- F10. `internal/config.Load(path) (*Result, error)` and `(*Config).Validate() *Result` — internal counterparts to F2/F3 — adopt the same shape, so the CLI (which calls `internal/config` directly today) and the SDK share one validation implementation. The `Result` type lives in `internal/config` and is re-exported via type alias from `pkg/orchestra` as `ValidationResult` (matching the existing `Config = config.Config`, `Warning = config.Warning` pattern in [pkg/orchestra/types.go](../../pkg/orchestra/types.go)).
- F11. The five CLI call sites (`cmd/validate.go`, `cmd/run.go`, `cmd/plan.go`, `cmd/init_cmd.go`, `cmd/spawn.go`) migrate to the new shape. External CLI behavior is byte-identical for `orchestra validate`, `orchestra run`, `orchestra plan`, `orchestra init`, `orchestra spawn`.

**Non-functional.**

- NF1. **Hard break.** `pkg/orchestra` is Experimental; no external consumers are pinned. The old tuple shapes go away — no back-compat shim. CHANGELOG entry under "Experimental: breaking" documents the migration.
- NF2. **No new direct dependencies in `go.mod`.** Reuse `errors`, `fmt`, `strings` from stdlib. The structured `Field` path is just `[]string` — no AST library, no path-DSL package.
- NF3. **No transitive `internal/` exposure.** `ValidationResult`, `Warning`, `ConfigError`, and `ErrInvalidConfig` are exported from `pkg/orchestra` only. Same enforcement as P2.0 NF3 / P2.4 NF5.
- NF4. **Godoc on every new exported identifier.** Same standard as P2.4. Package doc snippet rewritten for the new `LoadConfig` shape.
- NF5. **Validators populate `Field` on every issue.** Adding `Field` is the chapter's main value-add; an unpopulated `Field` defeats the purpose. The reshape touches every validator in `internal/config/schema.go` and `internal/config/repository.go`; tests assert `Field` is non-empty (or empty for project-level issues, with explicit assertion).
- NF6. **CLI parity is byte-identical.** `cmd/validate.go` output (warnings + success line + per-team summary), `cmd/run.go` output (warnings + run output), `cmd/plan.go` output, `cmd/init_cmd.go` output, `cmd/spawn.go` output: all unchanged externally.

---

## 3. Scope

### In-scope

- **New types** (in `internal/config`, re-exported from `pkg/orchestra` via type alias):
  - `Result` (re-exported as `ValidationResult`) — the aggregate `{Config, Warnings, Errors}` plus `Valid() bool`, `Err() error` methods.
  - `ConfigError` — typed counterpart to `Warning` for hard validation failures. Same `{Field []string, Team string, Message string}` shape, same `String()` semantics.
- **Reshaped existing types:**
  - `Warning` grows `Field []string`. Existing `Team string` and `Message string` retained. `String()` unchanged.
- **New sentinel:** `pkg/orchestra.ErrInvalidConfig`. Returned via `errors.Is` from `result.Err()` when `!result.Valid()`. Lets callers do `if errors.Is(err, orchestra.ErrInvalidConfig)` for typed handling.
- **Reshaped functions:**
  - `internal/config.Load(path) (*Result, error)`
  - `(*config.Config).Validate() *Result`
  - `pkg/orchestra.LoadConfig(path) (*ValidationResult, error)`
  - `pkg/orchestra.Validate(cfg *Config) *ValidationResult`
- **Validator changes** in `internal/config/schema.go` + `internal/config/repository.go`:
  - Every error appended to the local `errs []string` slice becomes a `ConfigError{Field, Team, Message}` appended to a new `errs []ConfigError` slice. `validateTopLevel`, `validateTeamNames`, `validateTeams`, `validateTasks`, `validateDependencies`, `detectCycles`, `validateRepositoryHard` all touched.
  - Every warning gains a `Field` value (today they only carry `Team` + `Message`). `validateBackendWarnings`, `validateTeamSize`, `validateTaskRatio`, `validateRepositoryWarnings`, `validateTasks` (the empty-details / empty-verify warnings) all touched.
- **CLI migration** — all five sites convert `(cfg, warnings, err)` to `result := config.Load(path)` (or `orchestra.LoadConfig`) and iterate `result.Warnings`:
  - [cmd/validate.go](../../cmd/validate.go) — uses `config.Load`
  - [cmd/run.go](../../cmd/run.go) — uses `orchestra.LoadConfig`
  - [cmd/plan.go](../../cmd/plan.go) — uses `config.Load`
  - [cmd/init_cmd.go](../../cmd/init_cmd.go) — uses `config.Load`
  - [cmd/spawn.go](../../cmd/spawn.go) — uses `config.Load` (currently discards warnings)
- **Test migrations:**
  - `internal/config/schema_test.go`, `internal/config/loader_test.go`, `internal/config/repository_test.go` — every assertion against `(warnings, err)` becomes an assertion against `result.Warnings` / `result.Errors` / `result.Valid()`.
  - `pkg/orchestra/orchestra_test.go`, `pkg/orchestra/handle_test.go`, `pkg/orchestra/events_test.go`, `pkg/orchestra/inspect_test.go`, `pkg/orchestra/steering_test.go` — every `cfg, _, err := orchestra.LoadConfig(...)` becomes `res, err := orchestra.LoadConfig(...); cfg := res.Config`.
- **New tests** asserting the chapter's value-add:
  - `TestValidate_PopulatesFieldPath` — every validator populates `Field`. Table-driven across each validator that emits issues, asserting on the expected `[]string` path.
  - `TestValidate_WarningsAndErrorsCoexist` — a config with both soft and hard issues returns both slices populated and `Valid() == false`.
  - `TestValidationResult_ErrIsErrInvalidConfig` — `errors.Is(result.Err(), orchestra.ErrInvalidConfig)` is true when invalid, `result.Err() == nil` when valid.
  - `TestValidationResult_ErrFormatPreservesCLIByteOutput` — the `Err().Error()` string round-trips the existing `"validation errors:\n  - ...\n  - ..."` format the CLI relies on.
  - `TestLoadConfig_ParseErrorReturnsErrorNotResult` — malformed YAML returns `(nil, error)` — the `error` channel is reserved for I/O / parse, not validation.
  - `TestValidate_NilConfigReturnsConfigError` — `Validate(nil)` returns `&ValidationResult{Errors: [...]}` (no panic, no error return).
  - `TestValidate_ConfigNilWhenInvalid` — for a config that fails validation, `result.Config == nil`.
- **Doc updates:**
  - [pkg/orchestra/doc.go](../../pkg/orchestra/doc.go) — package-doc snippet rewritten to use `result := orchestra.LoadConfig(path)`.
  - [README.md](../../README.md) — line 241's snippet (`cfg, warnings, err := orchestra.LoadConfig("orchestra.yaml")`) rewritten.
  - Godoc on `ValidationResult`, `ConfigError`, `Warning` (refresh), `LoadConfig`, `Validate`, `Valid`, `Err`, `ErrInvalidConfig`.
- **CHANGELOG entry** under `## Unreleased` → new `### Experimental: breaking — pkg/orchestra validation result reshape (P2.5)` section, listing removed shapes (`(*Config, []Warning, error)` / `([]Warning, error)`) and added types (`ValidationResult`, `ConfigError`, `ErrInvalidConfig`, reshaped `Warning`).

### Out-of-scope

- **YAML line / column tracking on `Field`.** Would require keeping `yaml.Node` references through `Validate`. Real value but a much bigger lift; defer until a consumer asks. Open Q.
- **Render helpers on `ValidationResult`** — e.g. `(r *ValidationResult) PrintTo(w io.Writer)`, `Summary() string`. CLI iteration is four lines today; a render method is premature abstraction. Cut.
- **JSON marshaling of `ValidationResult` and an `orchestra validate --json` flag.** No consumer has asked. Defer.
- **Filter helpers** like `WarningsForTeam(name)`, `ErrorsForField(prefix)`. Premature; callers can `for _, w := range result.Warnings { if w.Team == x { ... } }`.
- **Splitting `Warning` and `ConfigError` into a unified `Issue` with a `Severity` field.** Considered (see "Borderline" in §1's lean-scope brief). Two parallel types matches what the brief named and what consumers ask for ("warnings" and "errors" are domain-distinct). Open Q for a future merge if a consumer wants severity-based filtering.
- **Schema changes.** No new YAML fields; this is purely a Go-API surface reshape.
- **Stability commitment.** Experimental tier holds.
- **Back-compat shim.** Hard break; no `LoadConfigLegacy` or `WarningsAndError` wrapper. P2.4 set this precedent and the chapter inherits it.

---

## 4. API surface

### 4.1 `pkg/orchestra/types.go` (additions / reshapes)

```go
package orchestra

import (
    "errors"

    "github.com/itsHabib/orchestra/internal/config"
)

// ValidationResult is the aggregate output of [LoadConfig] and [Validate]:
// the parsed config (when valid), the structured warnings (soft issues), and
// the structured errors (hard validation failures). Use Valid to gate
// further use of Config; use Err for an error-shaped view of the validation
// failures suitable for `if err != nil` patterns.
//
// Experimental: aliased from internal/config; field set may grow.
type ValidationResult = config.Result

// Warning is a soft validation issue surfaced by [LoadConfig] or [Validate].
// It does not block execution. Field is the structured YAML path to the
// offending node (empty for project-level issues); Team is the denormalized
// team name when Field points into a team subtree.
//
// Experimental: aliased from internal/config.
type Warning = config.Warning

// ConfigError is a hard validation failure surfaced by [LoadConfig] or
// [Validate]. Same shape and semantics as [Warning]; only the severity
// differs. A non-empty Errors slice on a [ValidationResult] makes
// Valid return false and Err return non-nil.
//
// Experimental: aliased from internal/config.
type ConfigError = config.ConfigError

// ErrInvalidConfig is wrapped by [ValidationResult.Err] whenever the result
// has at least one entry in Errors. Callers can use errors.Is to recognize
// validation failures regardless of how the formatted message changes:
//
//   res, err := orchestra.LoadConfig("orchestra.yaml")
//   if errors.Is(err, orchestra.ErrInvalidConfig) { ... }
//
// Experimental: this sentinel is kept stable across breaking surface
// changes so callers can rely on errors.Is checks.
var ErrInvalidConfig = errors.New("orchestra: invalid config")
```

### 4.2 `pkg/orchestra/run.go` (reshaped)

```go
// LoadConfig parses a YAML config from path, applies defaults, and runs
// validation. Returns a [ValidationResult] aggregating the parsed config,
// any warnings, and any errors. The error return is reserved for I/O or
// parse failures (file not found, malformed YAML); structural validation
// issues live in result.Errors.
//
// Typical use:
//
//   res, err := orchestra.LoadConfig("orchestra.yaml")
//   if err != nil {
//       return err // I/O or parse failure
//   }
//   for _, w := range res.Warnings {
//       fmt.Fprintln(os.Stderr, w)
//   }
//   if !res.Valid() {
//       return res.Err()
//   }
//   _, err = orchestra.Run(ctx, res.Config)
//
// Experimental.
func LoadConfig(path string) (*ValidationResult, error)

// Validate runs the config validator standalone. Useful for callers that
// build configs programmatically. Mirrors what [Run] does internally:
// applies ResolveDefaults to cfg, then validates. A nil cfg is treated as
// a hard validation failure (one ConfigError entry, empty Field) rather
// than a panic — Validate never returns nil.
//
// Experimental.
func Validate(cfg *Config) *ValidationResult
```

### 4.3 `internal/config` (the source-of-truth shape)

```go
// Result is the aggregate output of Load and (*Config).Validate.
// pkg/orchestra re-exports this as ValidationResult.
type Result struct {
    // Config is the parsed, defaults-resolved config. Nil when the result
    // is invalid (Valid() == false) or when Load returned a non-nil error.
    Config *Config

    // Warnings is the slice of soft validation issues. Order is the order
    // each validator emitted them; same as today's []Warning slice.
    Warnings []Warning

    // Errors is the slice of hard validation failures. Empty when Valid().
    Errors []ConfigError
}

// Valid returns true when no hard validation errors were recorded.
// Warnings do not affect validity.
func (r *Result) Valid() bool { return len(r.Errors) == 0 }

// Err returns nil when Valid. Otherwise returns an error wrapping
// ErrInvalidConfig with the formatted "validation errors:\n  - ..." text
// the CLI relies on for byte-identical output. Use errors.Is to test
// for invalidity:
//
//   if errors.Is(res.Err(), orchestra.ErrInvalidConfig) { ... }
func (r *Result) Err() error

// Warning is a soft validation issue.
type Warning struct {
    // Field is the structured YAML path to the offending node, e.g.
    // {"teams", "0", "tasks", "2", "verify"} for a missing verify on
    // team 0's third task. Empty for project-level issues (missing
    // project name, unknown backend.kind, etc.).
    Field []string
    // Team is the denormalized team name when Field points into a team
    // subtree; empty otherwise. Exists for ergonomic display so String()
    // can render "team \"foo\": message" without walking Field back into
    // Config. Programmatic consumers should prefer Field.
    Team string
    // Message is the human-readable description of the issue.
    Message string
}

// String returns the human-readable form preserved from pre-P2.5:
// `team "foo": message` when Team is non-empty, else just Message.
func (w Warning) String() string

// ConfigError is a hard validation failure with the same shape and
// semantics as Warning. Two parallel types — not a unified Issue with
// Severity — because the warning vs. error distinction is domain-meaningful
// and consumers iterate the slice they care about.
type ConfigError struct {
    Field   []string
    Team    string
    Message string
}

// String matches Warning.String exactly.
func (e ConfigError) String() string

// Load reads and parses an orchestra config from the given YAML file path.
// Resolves defaults and validates. The error return is reserved for I/O
// or parse failures; structural validation issues live in result.Errors.
func Load(path string) (*Result, error)

// Validate is a method on *Config that returns a *Result. Replaces the
// pre-P2.5 ([]Warning, error) tuple. Never returns nil.
func (c *Config) Validate() *Result
```

### 4.4 Sample call-site migrations

**`cmd/validate.go` before:**

```go
cfg, warnings, err := config.Load(args[0])
if err != nil {
    logger.Error("Validation failed: %s", err)
    os.Exit(1)
}
for _, w := range warnings {
    logger.Warn("%s", w)
}
logger.Success("Config is valid: %d teams, project %q", len(cfg.Teams), cfg.Name)
```

**after:**

```go
res, err := config.Load(args[0])
if err != nil {
    logger.Error("Validation failed: %s", err)
    os.Exit(1)
}
for _, w := range res.Warnings {
    logger.Warn("%s", w)
}
if !res.Valid() {
    logger.Error("Validation failed: %s", res.Err())
    os.Exit(1)
}
logger.Success("Config is valid: %d teams, project %q", len(res.Config.Teams), res.Config.Name)
```

External output is byte-identical: warnings render the same (`Warning.String()` unchanged), and `res.Err()` produces the same `"validation errors:\n  - ..."` string the previous `err` carried.

**`pkg/orchestra/orchestra_test.go` migration pattern:**

```go
// Before
cfg, _, err := orchestra.LoadConfig(configPath)
if err != nil { t.Fatal(err) }

// After
res, err := orchestra.LoadConfig(configPath)
if err != nil { t.Fatal(err) }
cfg := res.Config
```

(Tests that explicitly want to inspect warnings or errors gain `res.Warnings` / `res.Errors` access without a separate function call.)

---

## 5. Semantics & lifecycle

### 5.1 Result population matrix

| Scenario                                       | `error` (LoadConfig only) | `result.Config` | `result.Warnings`     | `result.Errors`             | `result.Valid()` |
| ---------------------------------------------- | ------------------------- | --------------- | --------------------- | --------------------------- | ---------------- |
| File not found / malformed YAML                | non-nil                   | nil (no result) | n/a (no result)       | n/a (no result)             | n/a              |
| Parse OK, validation OK, no warnings           | nil                       | non-nil         | empty                 | empty                       | true             |
| Parse OK, validation OK, warnings present      | nil                       | non-nil         | populated             | empty                       | true             |
| Parse OK, validation failed (no warnings)      | nil                       | **nil**         | empty                 | populated                   | false            |
| Parse OK, validation failed, warnings present  | nil                       | **nil**         | populated             | populated                   | false            |
| `Validate(nil)` (no `error` return)            | n/a                       | nil             | empty                 | one entry, empty Field      | false            |

Key: when `Valid() == false`, `Config` is nil — consumers cannot accidentally hand an invalid config to `Run`. When `LoadConfig` returns `error != nil`, the result is also nil (parse couldn't even produce a config to validate).

### 5.2 `Err()` format

`Result.Err()` returns nil when `Valid()`. Otherwise it returns an error built from the existing format string in [internal/config/schema.go:218-220](../../internal/config/schema.go), wrapped with `ErrInvalidConfig`:

```go
return fmt.Errorf("validation errors:\n  - %s%w",
    strings.Join(messages, "\n  - "), wrapInvalidConfig{})
```

(or equivalent — the implementation-level choice is `errors.Join` vs a custom wrapper, with the constraint that `Err().Error()` produces the existing string and `errors.Is(err, ErrInvalidConfig)` is true.)

CLI byte-parity: `cmd/validate.go` printing `res.Err()` produces the same multi-line message today's `logger.Error("Validation failed: %s", err)` produces.

### 5.3 `Field` path conventions

- Project-level issues: empty slice. Examples: missing project name, unknown `backend.kind`, negative `defaults.ma_concurrent_sessions`.
- Team-scoped issues: starts with `["teams", "<index>"]`. The index is the position in the YAML array; not the team name. The denormalized `Team` field carries the name.
- Nested team issues: `["teams", "<index>", "tasks", "<index>", "<field>"]` for tasks, `["teams", "<index>", "depends_on"]` for dependency issues, `["teams", "<index>", "members"]` for member-related warnings.
- Backend issues: `["backend", "kind"]`, `["backend", "managed_agents", "repository", "url"]`, etc.
- Coordinator issues: `["coordinator", "<field>"]`.

The path is a `[]string` of literal YAML field names and stringified array indices — no expression DSL (no `[0]` brackets, no `.` separators). Consumers print however they want; an example helper `FieldPathString(field []string) string` is **not** part of this chapter (consumers can `strings.Join(field, ".")` or build a richer formatter).

### 5.4 Why two parallel types instead of unified `Issue{Severity}`

Considered and rejected:

- Two domain categories. Soft and hard issues are semantically distinct in this codebase — warnings document YAML smells (members under managed_agents, large team size) while errors document structural breakage (missing field, dependency cycle). Consumers iterate the slice they care about.
- Most callers want one or the other, not both. CLI: warnings to stderr, error to exit-1. SDK consumers (the future TUI / dashboard / audit tool): warnings as info badges, errors as blocking modals.
- A `Severity` field on a unified type forces every consumer to filter by severity. Two slices is ergonomic.
- Unification is reversible later. Adding a `Severity` field to a unified `Issue` and replacing the two slices is mechanical if a consumer ever needs severity-based aggregation. The reverse (splitting an `Issue` back into two types) is breaking.

Open Q tracks this — if a real consumer needs severity-aware filtering before v0.1.0, the merge happens. Until then, two types.

### 5.5 Why `LoadConfig` keeps an `error` return but `Validate` doesn't

`LoadConfig` does I/O (reads a file, parses YAML). I/O failures are a different error class from structural validation — transient (file not found, permission denied) vs. permanent (config wrong). Keeping them in the `error` return preserves the standard Go idiom (`if err != nil { return err }`) for the I/O class.

`Validate` is pure — given a `*Config`, no I/O happens. There's no second error class to surface. A single `*ValidationResult` return matches the brief's preferred shape (Q3 of the user-supplied decision list) and removes a redundant return value.

Asymmetry is intentional and documented in the godoc on each function.

---

## 6. Implementation order

One PR. The reshape is simultaneous across `internal/config` and `pkg/orchestra` because they share a return type — a partial migration would break the build mid-stream.

Suggested commit-by-commit ordering inside the single PR:

1. **Add new types** in `internal/config`: `Result`, `ConfigError`, sentinel `ErrInvalidConfig`. Add the new `Field []string` to `Warning`. Don't touch validators or callers yet — package compiles, all existing tests still pass against the old function signatures.
2. **Reshape validators**: `validateTopLevel`, `validateTeamNames`, `validateTeams`, `validateTasks`, `validateDependencies`, `detectCycles`, `validateRepositoryHard`, `validateBackendWarnings`, `validateTeamSize`, `validateTaskRatio`, `validateRepositoryWarnings`. Each gains `Field` population. Internal-only changes; existing `(*Config).Validate()` signature still in place but accumulates `[]ConfigError` internally.
3. **Reshape `(*Config).Validate()` and `config.Load`** to return `*Result` / `(*Result, error)`. Migrate `internal/config/loader_test.go`, `schema_test.go`, `repository_test.go` simultaneously. Internal package now compiles + tests pass against new shape.
4. **Reshape `pkg/orchestra.LoadConfig` and `pkg/orchestra.Validate`** to the new shape. Re-export the new types via type alias in `pkg/orchestra/types.go`. Migrate the five `pkg/orchestra/*_test.go` files. SDK package compiles + tests pass.
5. **Migrate the five CLI call sites** (`cmd/validate.go`, `cmd/run.go`, `cmd/plan.go`, `cmd/init_cmd.go`, `cmd/spawn.go`). CLI builds + integration tests pass.
6. **Update doc snippets** in `pkg/orchestra/doc.go` and `README.md`. Add CHANGELOG entry.
7. **Add new tests** asserting the chapter's value-add: `TestValidate_PopulatesFieldPath`, `TestValidate_WarningsAndErrorsCoexist`, `TestValidationResult_ErrIsErrInvalidConfig`, `TestValidationResult_ErrFormatPreservesCLIByteOutput`, `TestLoadConfig_ParseErrorReturnsErrorNotResult`, `TestValidate_NilConfigReturnsConfigError`, `TestValidate_ConfigNilWhenInvalid`.

---

## 7. Acceptance criteria (chapter-level)

- [ ] `pkg/orchestra` exports `ValidationResult` (alias of `internal/config.Result`), `ConfigError` (alias of `internal/config.ConfigError`), reshaped `Warning` (with `Field []string`), `ErrInvalidConfig`.
- [ ] `pkg/orchestra.LoadConfig` returns `(*ValidationResult, error)`; `pkg/orchestra.Validate` returns `*ValidationResult`.
- [ ] `internal/config.Load` returns `(*Result, error)`; `(*Config).Validate()` returns `*Result`.
- [ ] Every validator in `internal/config/schema.go` and `internal/config/repository.go` populates `Field` on every issue it emits.
- [ ] The five CLI commands produce byte-identical external output to pre-chapter for `orchestra validate`, `orchestra run`, `orchestra plan`, `orchestra init`, `orchestra spawn`.
- [ ] `errors.Is(result.Err(), orchestra.ErrInvalidConfig)` is true whenever `!result.Valid()` and false otherwise.
- [ ] `result.Err().Error()` on an invalid config produces the existing `"validation errors:\n  - ...\n  - ..."` format; CLI message strings are unchanged.
- [ ] All migrated tests pass; new tests for `Field` population, sentinel wrapping, and parse-error-vs-validation-error separation pass.
- [ ] Godoc on every new exported identifier; package doc snippet rewritten.
- [ ] CHANGELOG entry under "Experimental: breaking" listing removed shapes and added types.
- [ ] No new direct dependencies in `go.mod`.
- [ ] `make vet`, `make test`, `go test -race ./...`, `go tool golangci-lint run ./...` all green.

---

## 8. Open questions

1. **YAML line / column tracking on `Field`.** A natural future extension: `Warning` and `ConfigError` grow a `Line int` / `Column int` populated from `yaml.Node.Line` / `yaml.Node.Column`. Requires keeping a parsed `yaml.Node` tree alongside the unmarshaled `*Config` and threading node references through the validators. Real value for IDE consumers (squiggle on the right line). Big lift — the entire validator surface would need refactoring. Defer until a consumer asks. Lean: when the IDE/LSP consumer materializes, this is the next chapter on this surface.

2. **`Field` path string helper.** A `FieldPathString(field []string) string` (or `(w Warning) FieldPath() string`) that renders `["teams", "0", "tasks", "2", "verify"]` as `"teams[0].tasks[2].verify"` would simplify CLI rendering. Skipped from this chapter — `strings.Join(field, ".")` is one line at the call site, and bracket-vs-dot rendering is opinionated. Add when a consumer wants it.

3. **Unifying `Warning` + `ConfigError` into `Issue{Severity}`.** Two parallel types is the chapter's choice. If a future consumer wants severity-based filtering or a unified diagnostics stream (LSP-style), the merge is mechanical: introduce `type Issue = Warning` + `type Severity int` constants + `Issues []Issue` slice, deprecate the two-slice shape. Not on the critical path.

4. **`ValidationResult` rendering helper.** `(r *ValidationResult) PrintTo(w io.Writer)` would replace the four-line iteration loops in the CLI. Cut from this chapter as premature abstraction. Worth revisiting when the SDK gets its first real consumer (TUI / dashboard) and that consumer wants a "render a validation report" primitive.

5. **`Validate` name collision with `(*Config).Validate()`.** The package-level `pkg/orchestra.Validate(cfg)` and the method `cfg.Validate()` (now returning `*Result`) coexist. Today the method exists on `*config.Config` (which is the type-alias target of `pkg/orchestra.Config`). Callers can do either `cfg.Validate()` or `orchestra.Validate(cfg)`; both return `*ValidationResult`. Documented in godoc; not a problem in practice. Lean: keep both — the method is convenient for code that already has a `*Config`, the function is convenient for code that may have a nil `*Config`.

6. **Pre-`ResolveDefaults` validation.** `Validate` calls `ResolveDefaults` before validating, matching today's behavior. A future caller might want to validate the raw YAML-as-loaded shape (before defaults). Skipped — no consumer has asked, and validating the raw shape would emit warnings for fields the defaults would have filled in. Add as `ValidateRaw` or `ValidateOptions{ResolveDefaults: false}` if a consumer surfaces.
