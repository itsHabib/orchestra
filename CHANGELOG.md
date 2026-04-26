# Changelog

This file records breaking and notable changes to orchestra's public surface.
The Go SDK at `pkg/orchestra` is currently **experimental**: surface changes
may occur in any release without semver signaling until the surface is marked
stable. Each release that touches the SDK surface gets an entry here.

## Unreleased

### Experimental: breaking — `pkg/orchestra` validation result reshape (P2.5)

The P2.5 chapter collapses the `(*Config, []Warning, error)` and
`([]Warning, error)` tuples returned by `LoadConfig` and `Validate` into
a single `*ValidationResult` carrying the parsed config, structured
warnings, and structured errors. The reshape lifts hard validation
errors to a typed `ConfigError` with the same shape as `Warning` and
adds a `Field []string` path on both, so SDK consumers can render
structured reports without parsing strings out of `err.Error()`. CLI
output is byte-identical: warnings render the same and `result.Err()`
preserves the existing `"validation errors:\n  - ..."` format.

- **Reshaped**:
  - `LoadConfig(path) (*ValidationResult, error)` — the `error` return
    is now reserved for I/O / parse failures (file not found, malformed
    YAML); structural validation issues live in `result.Errors`.
  - `Validate(cfg *Config) *ValidationResult` — no `error` return;
    `result.Err()` carries it. A nil `cfg` is treated as a hard
    validation failure (one `ConfigError` entry) rather than a panic or
    a synthesized error.
  - `Warning` gains `Field []string`, the structured YAML path to the
    offending node (empty for project-level issues). `Team` and
    `Message` are unchanged; `String()` formatting is unchanged.
- **Added**:
  - `ValidationResult` type aliased from `internal/config.Result`, with
    `Valid() bool` and `Err() error` methods.
  - `ConfigError` type aliased from `internal/config.ConfigError` —
    parallel to `Warning` with the same `{Field, Team, Message}` shape.
  - `ErrInvalidConfig` sentinel — wrapped by `result.Err()` so callers
    can `errors.Is(err, orchestra.ErrInvalidConfig)`.
- **Removed**: the previous `(*Config, []Warning, error)` shape on
  `LoadConfig` and the `([]Warning, error)` shape on `Validate`. There
  is no back-compat shim — callers migrate as documented in
  `docs/features/11-p25-validation-result.md`.

### Experimental: breaking — `pkg/orchestra` operational SDK (P2.4)

The P2.4 chapter reshapes the SDK around an asynchronous `Handle` and adds
the surface dogfood apps need to observe and steer a live run. Callers that
previously called the one-shot `Run(ctx, cfg, opts...)` keep working with
no source change; the breaking pieces are the logging interface and the
implicit assumption that `Run` blocks the caller goroutine forever.

- **Removed**: `Logger` interface, `NewCLILogger()`, `NewNoopLogger()`, and
  `WithLogger`. Observability moves entirely to the structured `Event`
  channel and the optional `WithEventHandler` callback. Apps that printed
  through the Logger should consume `Handle.Events()` (or pass
  `WithEventHandler`) and render with `PrintEvent` or their own renderer.
- **Reshaped**: `Run(ctx, cfg, opts...) (*Result, error)` now wraps
  `Start + Wait`. Behavior for blocking callers is unchanged; the
  difference is that `Start` is the new primitive — it returns
  asynchronously with a `Handle`, and `Wait()` produces the final result.
- **Added**:
  - `Start(ctx, cfg, opts...) (*Handle, error)` plus the `Handle` type
    with `Wait`, `Cancel`, `Status`, `Events`, `Send`, `Interrupt`.
  - `WithEventBuffer(n int) Option` configures the bounded event channel.
  - `WithEventHandler(fn func(Event)) Option` registers a synchronous
    callback invoked on every emitted event before the channel send.
  - `PrintEvent(w io.Writer, ev Event)` is the canonical renderer used by
    `cmd/run` for streaming output; SDK callers can reuse it or write
    their own.
  - `ListRuns(workspaceDir) ([]RunSummary, error)`,
    `LoadRun(workspaceDir, runID) (*RunState, error)`, and
    `ListSessions(workspaceDir) ([]SessionInfo, error)` enumerate past and
    active runs and per-team managed-agents sessions. `cmd/runs` and
    `cmd/sessions` are migrated to call these helpers.
  - `RunSummary` and `SessionInfo` types describe the per-row data shapes
    those helpers return.
  - `Phase` (`PhaseInitializing`, `PhaseRunning`, `PhaseCompleting`,
    `PhaseDone`) and `TeamSnapshot` describe `Status()` output.
  - `Event` and `EventKind` types plus the kind constants
    (`EventTierStart`, `EventTeamStart`, `EventTeamMessage`,
    `EventToolCall`, `EventToolResult`, `EventTeamComplete`,
    `EventTeamFailed`, `EventTierComplete`, `EventRunComplete`,
    `EventDropped`, `EventInfo`, `EventWarn`, `EventError`).
  - Sentinel errors `ErrClosed`, `ErrTeamNotRunning`, and
    `ErrInterruptNotSupported` for steering call sites.

### Added — `pkg/orchestra` SDK (experimental)

- New package `pkg/orchestra` is the experimental Go SDK for the orchestra
  workflow engine. Surface is unstable — expect breaking changes without
  semver signaling until it is explicitly stabilized.
- `orchestra.Run(ctx, cfg, opts...) (*Result, error)` — drive a workflow
  from another Go program.
- `orchestra.LoadConfig(path)` and `orchestra.Validate(cfg)` for YAML- and
  programmatically-built configs.
- `orchestra.CloneConfig(cfg)` for callers sharing a config across
  goroutines that may invoke `Run` concurrently.
- `orchestra.Logger` interface plus `NewCLILogger()` and `NewNoopLogger()`
  constructors.
- Options: `WithLogger`, `WithWorkspaceDir`.
- Type aliases re-exporting `Config`, `Defaults`, `Backend`, `Coordinator`,
  `Team`, `Lead`, `Member`, `Task`, `Warning` from `internal/config`, and
  `RunState`, `TeamState`, `RepositoryArtifact` from `internal/store`.
- New `Result` and `TeamResult` types — the latter embeds `TeamState` so
  callers see status, cost, token counters, and turn count without dipping
  into the workspace.
- Sentinel error `ErrRunInProgress` returned by concurrent in-process `Run`
  invocations against the same workspace.
- Backend kind constants `BackendLocal` and `BackendManagedAgents`.

### Changed

- `cmd/run.go` is now a thin wrapper around `pkg/orchestra.Run`. The
  orchestration loop body lives in `pkg/orchestra` only.
- `printSummary` reads exclusively from `*orchestra.Result` rather than
  the workspace's `results/<team>.json` files.
- `Run` guarantees that all subprocesses it spawned (coordinator, team
  agents) are stopped before it returns, even on early-tier failure or
  context cancellation. The previous CLI-only behavior relied on the
  parent process exiting.

### Internal

- `internal/store.TeamState` gains a `NumTurns` field, populated during
  runs by `internal/run.RecordTeamComplete`. The CLI summary renderer
  consumes it directly through the SDK `Result`.
