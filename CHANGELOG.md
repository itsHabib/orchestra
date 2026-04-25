# Changelog

This file records breaking and notable changes to orchestra's public surface.
The Go SDK at `pkg/orchestra` is currently **experimental**: surface changes
may occur in any release without semver signaling until the surface is marked
stable. Each release that touches the SDK surface gets an entry here.

## Unreleased

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
