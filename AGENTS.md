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

- `cmd/` — Cobra CLI commands (run, plan, spawn, validate, init, status)
- `internal/config/` — YAML parsing and validation
- `internal/dag/` — Topological sort (Kahn's algorithm) → execution tiers
- `internal/spawner/` — Spawns `Codex -p --output-format stream-json` subprocesses
- `internal/injection/` — Prompt construction (role + context + tasks + dependency results)
- `internal/run/` — Run lifecycle service (lock, archive, seed state, team transitions)
- `internal/messaging/` — File-based cross-team message bus
- `internal/workspace/` — Atomic file I/O helpers for registry, results, logs
- `internal/fsutil/` — Atomic file operations (write .tmp → os.Rename)
- `internal/log/` — NDJSON logging
- `examples/` — Complete example projects with orchestra.yaml configs

## Conventions

- All file writes use atomic pattern: write `.tmp` then `os.Rename`
- Tests use real binary + mock Codex script (no mocks/interfaces for spawner)
- Config validation has hard errors (block execution) and soft warnings (print only)
- Teams communicate via file-based message bus under `.orchestra/messages/`
- Do not force-push unless the human explicitly approves it; prefer follow-up commits on PR branches

## Companion Skills

This project includes Codex skills in `.Codex/skills/`:

- `/orchestra-coord` — Bootstrap a coordinator session for an active run
- `/orchestra-init` — Interactively generate an orchestra.yaml
- `/orchestra-monitor` — Status dashboard (team progress, costs, activity, messages)
- `/orchestra-inbox` — Read messages from any team/coordinator inbox
- `/orchestra-msg` — Send messages to teams or broadcast to all
