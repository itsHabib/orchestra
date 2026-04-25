# Testing

Three layers — unit, integration, e2e — distinguished by what they actually exercise. Unit and integration run on every commit; e2e is opt-in and spends real money.

## Layers

### Unit

One package, no external dependencies, no shared state. Pure-function tests, table-driven where it pays off. Examples: `internal/config/schema_test.go`, `internal/dag/dag_test.go`, the cache helpers in `internal/spawner/managed_agents_*_test.go`.

### Integration

Multiple components in-process, with mocks or test substitutes for anything outside the binary. Production code paths are exercised end-to-end inside the test binary; the only fakes are at the boundary (HTTP servers, the `claude` CLI, the Managed Agents SDK).

Examples in this repo:
- `e2e_test.go` (top-level) — builds the orchestra binary and drives a full local-backend run against a mock `claude` shell script that emits valid stream-json. Despite the filename, this is integration: the *binary* is real, the model isn't.
- `cmd/run_ma_test.go` `TestRunTier_MA*` / `TestRunTiers_MA*` — exercises tier scheduling and the MA codepath through `orchestrationRun`, with a test substitute (`startTeamMAForTest`) standing in for the live Managed Agents spawner.
- `internal/spawner/managed_agents_*_test.go` — fake `sessions.New` / event stream / pager APIs sit behind the SDK's interface boundary so we can assert ordering, dedup, retry, and concurrency cap behavior without hitting MA.

### E2E

Real, live, hits the actual external system. For orchestra today that means the Anthropic Managed Agents API. Lives under `test/integration/ma_*/` (the directory name is historical — the contents are e2e by this taxonomy).

E2E fixtures are **opt-in** and **cost real money**. They are not part of `make test` and they do not run in CI. They run when:
- a human invokes the make target with an API key, or
- a maintainer runs them manually before a release / before flipping the default backend.

## Make targets

| Target | What it runs | When |
|---|---|---|
| `make test` | Unit + integration (`go test ./...`) | Every commit, CI |
| `make test-race` | Same with `-race` (needs CGO) | CI |
| `make vet` | `go vet ./...` | CI |
| `make lint` | `go vet` + `golangci-lint run` | CI |
| `make check` | lint + test + build | Local pre-push gate |
| `make e2e-ma` | All live MA fixtures | Manual; costs tokens |
| `make e2e-ma-single` | `test/integration/ma_single_team/` | Manual; costs tokens |
| `make e2e-ma-multi` | `test/integration/ma_multi_team/` | Manual; costs tokens |
| `make e2e-ma-steering` | `test/integration/ma_steering/` (live `orchestra msg` delivery) | Manual; costs tokens |

## Environment variables

| Variable | Required by | Purpose |
|---|---|---|
| `ANTHROPIC_API_KEY` | All `e2e-ma-*` targets | API key with Managed Agents beta access. Read from env or `~/.config/orchestra/config.json` `api_key` field. |
| `ORCHESTRA_MA_INTEGRATION` | All `e2e-ma-*` targets (set automatically by the make target) | Convention flag marking the run as a live-MA invocation. The make targets set this for you; manual `go run` invocations should set it too so logs and any future e2e gating recognise the run. |
| `CGO_ENABLED=1` | `make test-race` | The race detector requires cgo. Set automatically by the target; you only need a working C toolchain (gcc / clang / MinGW). |

## Costs (live MA, indicative)

Real numbers vary with model, prompt size, and turn count. The fixtures use `model: sonnet` and tight prompts — typical observed cost per run:

| Fixture | Teams | Approx. cost |
|---|---|---|
| `ma_single_team` | 1 | ≈ $0.05 |
| `ma_multi_team` | 2 | ≈ $0.15 |
| `ma_steering` | 1 | ≈ $0.05–0.10 |

Treat these as smoke runs, not regression suites. Run them when the MA codepath changes shape (spawner internals, prompt builder, event translator, session lifecycle), or before promoting a release.

## E2E post-run checks

`orchestra run` exits non-zero if any team fails, so the make target's exit code is the first signal. For a tighter check, inspect `.orchestra/`:

```bash
make e2e-ma-multi
jq '.teams | to_entries[] | {team: .key, status: .value.status, has_summary: (.value.result_summary != "")}' .orchestra/state.json
ls .orchestra/results/*/summary.md
```

For `ma_multi_team`, the analyst's summary should reference content from the planner's outline — that proves the dependency-result injection path actually flowed text between sessions.

See each fixture's `README.md` for the full checklist.

## CI

GitHub Actions runs `make vet`, `make lint`, and `make test-race` on every push and PR. E2E targets are deliberately not in CI — they would require storing an `ANTHROPIC_API_KEY` secret and would charge per run. A separate manual workflow with secret-backed credentials is the natural home if e2e ever needs to run on a schedule.
