# Orchestra

Go CLI that orchestrates multi-team AI agent workflows. Reads `orchestra.yaml`, builds a DAG (Kahn's algorithm), and spawns `Codex -p` subprocesses per team lead — parallel within a tier, sequential across tiers.

## Build & Test

```bash
make build        # build binary
make test         # go test ./...
make vet          # go vet ./...
make install      # install to $GOBIN
```

Requires Go 1.22+.

## Project Structure

- `cmd/` — Cobra CLI commands (run, runs, plan, spawn, validate, init, status, debug)
- `internal/config/` — YAML parsing and validation
- `internal/dag/` — Topological sort (Kahn's algorithm) → execution tiers
- `internal/spawner/` — Spawns `Codex -p --output-format stream-json` subprocesses
- `internal/injection/` — Prompt construction (role + context + tasks + dependency results)
- `internal/agents/` — Managed Agents service (MA client, agent cache, prune/reconcile)
- `internal/artifacts/` — Host-side artifact persistence for `signal_completion(artifacts={...})`
- `internal/files/` — Anthropic Files API uploader for agent-declared file mounts
- `internal/run/` — Run lifecycle service (lock, archive, seed state, team transitions)
- `internal/workspace/` — Atomic file I/O helpers for registry, results, logs
- `internal/store/` — Persistence layer: run state, agent/env registries, run locks (memstore + filestore)
- `internal/fsutil/` — Atomic file operations (write .tmp → os.Rename)
- `internal/log/` — NDJSON logging
- `examples/` — Complete example projects with orchestra.yaml configs

## Conventions

- All file writes use atomic pattern: write `.tmp` then `os.Rename`
- Tests use real binary + mock Codex script (no mocks/interfaces for spawner)
- Config validation has hard errors (block execution) and soft warnings (print only)
- Inter-agent data flows via `signal_completion(artifacts={...})` (captured host-side under `<workspace>/.orchestra/artifacts/`); coordinator → agent steering uses `mcp__orchestra__steer`. The v2 file message bus was removed in v3 phase A.
- Do not force-push unless the human explicitly approves it; prefer follow-up commits on PR branches

## Companion Skills

This project includes Codex skills in `.Codex/skills/`:

- `/orchestra-coord` — Bootstrap a coordinator session for an active run
- `/orchestra-init` — Interactively generate an orchestra.yaml
- `/orchestra-monitor` — Status dashboard (team progress, costs, activity)

Older skills `/orchestra-msg` and `/orchestra-inbox` target the v2 file message bus and no longer function in v3; use `mcp__orchestra__steer` and `mcp__orchestra__read_artifact` instead.
