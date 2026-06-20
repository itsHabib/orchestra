# Orchestra

Define a set of AI agents, the tasks each one owns, and the dependencies between them in a single `orchestra.yaml`. Orchestra builds a dependency graph, runs the agents tier-by-tier — parallel within a tier, sequential across tiers — and flows each completed agent's output into the prompts of the agents that depend on it. Each agent is either a `claude -p` subprocess on the host (`backend: local`) or a sandboxed [Managed Agents](https://platform.claude.com/docs/en/managed-agents) session (`backend: managed_agents`).

There are two ways to drive it:

- **CLI** — `orchestra run project.yaml` validates the config, builds the DAG, executes every tier, and prints a summary. This is the human-driver path.
- **MCP server** — `orchestra mcp` exposes the run primitives (`run`, `list_runs`, `get_run`, `cancel_run`, `get_artifacts`, `read_artifact`, `steer`) to a parent Claude Code session so the chat-side LLM spawns runs, polls them, reads what agents produced, and steers them — without a separate human at the terminal.

```bash
orchestra run project.yaml
```

> **v3 naming.** Agents replaced the v2 noun "teams" across YAML, the MCP surface, and the SDK. The legacy `teams:` key still parses (with a one-shot deprecation warning); `pkg/orchestra` keeps `Team*` aliases for the migration window. New work uses `agents:` and the `Agent*` types. The aliases are removed in v3.x.

## Why it exists

Orchestra is a focused DAG runner for agents. Its job is: topologically sort agents into tiers, run a tier in parallel, inject completed-dependency output into the next tier's prompts, and persist run state. The core stays minimal and unopinionated on purpose — richer behavior composes *around* it as add-ons rather than swelling the core. That's a bias toward a sharp core and sequencing, not a vow to stay small; the core earns the next capability when a real need shows up.

Some jobs live outside the core by design — where they compose best, revisited as the work demands:

- **Autonomous coordinator.** The v2 coordinator agent was removed; the chat-side LLM (driving via MCP) or a human plays coordinator. `coordinator: { enabled: true }` still parses but the spawn is suppressed with a deprecation warning. Coordination lives with whoever drives the run.
- **General message bus.** The v2 file message bus is gone. Cross-agent data flows one way: an upstream agent's output is injected into downstream prompts. Structured payloads travel via `signal_completion(artifacts={...})`; the chat-side LLM reads them with `read_artifact`, and a human nudges a running agent with `steer` / `orchestra msg`. Free-form chat isn't the model today.
- **Scheduling, queueing, retries.** Orchestra runs a graph once and exits; re-running and recurrence live in whatever drives it.
- **Model/provider abstraction.** Agents are Claude — `claude -p` locally or Managed Agents remotely — until a second provider is a real requirement.

## Status

Dogfooded WIP. The v3 "phase A" surface — the agent rename, artifact substrate, `steer`, file mounts, the MCP server, the two backends, and the Go SDK — has landed and been self-dogfooded (see [docs/feedback-phase-a-dogfood.md](docs/feedback-phase-a-dogfood.md)). Known gaps, all tracked:

- The Go SDK at `pkg/orchestra` is labelled **experimental**: the surface may change without semver signaling until it is marked stable. See [CHANGELOG.md](CHANGELOG.md).
- `steer` works only on `backend: managed_agents`. Local-backend steering is deferred to v3.x; restart the run with appended context as the workaround.
- `requires_credentials` resolution works end-to-end on `backend: local`, but on `managed_agents` the secret does not reach the sandbox — the Managed Agents SDK does not yet expose per-session env injection. GitHub is the exception (the `repository` resource path works). Tracked at [issues/42](https://github.com/itsHabib/orchestra/issues/42).
- `orchestra mcp --transport http` listens with **no authentication**; loopback only, trusted hosts only. HTTP auth is a follow-up.

## Architecture

```
                 orchestra.yaml
                       │
         ┌─────────────┴──────────────┐
         │                            │
   CLI (cobra)                  MCP server
   run / plan / status          run / list_runs / get_run
   spawn / runs / sessions      cancel_run / steer
   msg / interrupt / mcp        get_artifacts / read_artifact
   skills / credentials                 │
         │                              │
         └──────────────┬───────────────┘
                        ▼
              pkg/orchestra.Run
   ┌────────────────────────────────────────────┐
   │ dag.BuildTiers (Kahn) → [][]string tiers     │
   │ for each tier:                               │
   │   snapshot state → inject deps into prompts  │
   │   spawn agents in parallel (goroutines)      │
   │   collect results → persist state            │
   └────────────────────────────────────────────┘
         │ local backend          │ managed_agents backend
         ▼                        ▼
   claude -p subprocess     Beta.Sessions container
   (host filesystem)        (sandboxed; optional repo mount)
         │                        │
         └──────────┬─────────────┘
                    ▼
              .orchestra/
   state.json · per-agent results · logs · artifacts · archive
```

| Package | Responsibility |
|---------|----------------|
| `cmd/` | Cobra CLI commands |
| `internal/config/` | YAML parsing + two-level validation (hard errors / soft warnings) |
| `internal/dag/` | Topological sort (Kahn's algorithm) → execution tiers |
| `internal/injection/` | Prompt construction (role + context + tasks + dependency results) |
| `internal/spawner/` | Spawns `claude -p --output-format stream-json`; Managed Agents spawner |
| `internal/agents/`, `internal/machost/` | Managed Agents client, session lifecycle, cache prune/reconcile |
| `internal/artifacts/` | Host-side persistence for `signal_completion(artifacts={...})` |
| `internal/files/` | Anthropic Files API uploader for agent-declared file mounts |
| `internal/ghhost/` | GitHub PAT resolution + branch/PR flow for repo-backed MA agents |
| `internal/run/` | Run lifecycle service (lock, archive, seed state, agent transitions) |
| `internal/store/` | Persistence: run state, agent/env registries, run locks (memstore + filestore) |
| `internal/mcp/` | MCP server + tool handlers |
| `internal/credentials/`, `internal/skills/`, `internal/customtools/` | Credential store, skills cache, host-side custom tools |
| `internal/fsutil/`, `internal/workspace/`, `internal/log/` | Atomic file I/O, workspace helpers, NDJSON logging |
| `pkg/orchestra/` | Experimental Go SDK — the same code path the CLI uses |

## Install

```bash
make build      # build ./orchestra
make install    # build + copy to $GOBIN
```

Requires Go 1.26+ and the [Claude CLI](https://docs.anthropic.com/en/docs/claude-code) on `PATH`. `backend: managed_agents` additionally requires `ANTHROPIC_API_KEY`.

## Quick start

**1. Write an `orchestra.yaml`** (`backend: local` is the default):

```yaml
name: my-saas-app

defaults:
  model: sonnet
  max_turns: 200
  permission_mode: acceptEdits
  timeout_minutes: 45

agents:
  - name: backend
    lead:
      role: Backend Lead
      model: opus
    context: |
      Go 1.22, Chi router, PostgreSQL, sqlc
    members:
      - role: API Engineer
        focus: REST endpoints, request validation
      - role: DB Engineer
        focus: Postgres schema, migrations, queries
    tasks:
      - summary: Design and implement REST API
        details: Create a Chi router with CRUD endpoints for users and projects
        deliverables:
          - src/api/router.go
          - src/api/handlers/
        verify: go build ./...

  - name: frontend
    depends_on: [backend]
    lead:
      role: Frontend Lead
    context: |
      React 18, TypeScript, Tailwind CSS
    tasks:
      - summary: Build dashboard UI
        details: Create React components consuming the backend API
        deliverables:
          - web/src/components/
        verify: npm run build
```

**2. Validate, preview, run:**

```bash
orchestra validate orchestra.yaml
orchestra plan orchestra.yaml                 # DAG order without running
orchestra plan orchestra.yaml --show-prompts  # full prompt each agent would receive
orchestra run orchestra.yaml
```

Runnable examples live under [`examples/`](examples/): `miniflow`, `taskqueue`, `url-shortener` (local backend); `ma_multi_team`, `ma_repo_relay` (managed agents).

## CLI commands

| Command | What it does |
|---------|--------------|
| `orchestra validate <config>` | Parse, validate, print a config summary. Exits 1 on hard errors. |
| `orchestra plan <config>` | Show the DAG execution order without running. `--show-prompts`, `--json`. |
| `orchestra init <config>` | Validate and create the `.orchestra/` workspace. |
| `orchestra run <config>` | Build the DAG, execute every tier, print the summary. |
| `orchestra spawn <config> --agent <name>` | Spawn a single agent; print its raw result JSON. (`--team` is the deprecated alias.) |
| `orchestra status [--workspace <path>]` | Per-agent status table: status, tokens, duration. |
| `orchestra runs ls` | List recent active and archived runs (status, cost, duration, start time). |
| `orchestra runs show <run-id>` | One run's agents, DAG tier, cached MA agent/env/session IDs. |
| `orchestra runs prune [--older-than 720h] [--apply]` | Dry-run stale MA cache pruning in a run context; `--apply` deletes. |
| `orchestra mcp [--transport stdio\|http]` | Start the MCP server (parent-Claude entry point). |
| `orchestra msg --team <name> --message <text> [--no-retry]` | Deliver a `user.message` to a running MA agent. See [Steering](#steering-a-run). |
| `orchestra interrupt --team <name>` | Deliver a `user.interrupt` to a running MA agent (always at-most-once). |
| `orchestra sessions ls [--all]` | List agents in the active run with their MA session info. |
| `orchestra skills upload\|ls\|sync` | Manage skills registered with the Anthropic Skills API (MA backend). |
| `orchestra credentials set\|get\|list\|delete` | Manage credentials Orchestra injects into agent sessions. |
| `orchestra debug agents ls\|prune` | Low-level MA cache inspection. |

### MCP tools

`orchestra mcp` registers these tools for a parent Claude Code session (stdio by default):

| Tool | What it does |
|------|--------------|
| `run` | Spawn a run subprocess from an `inline_dag` or a `config_path`. Returns the run id. |
| `list_runs` | List MCP-managed runs with derived status and per-agent `signal_completion` outcomes. |
| `get_run` | One run's current status and per-agent breakdown. |
| `cancel_run` | Request cancellation (local or MA); records it, signals the subprocess, waits for clean shutdown. |
| `get_artifacts` | List artifacts an agent emitted via `signal_completion` (metadata only). |
| `read_artifact` | Read one artifact's content (`{type, phase?, content}`). |
| `steer` | Inject a `user.message` into a running MA agent's session. MA backend only. |

## Use as a Go library

`pkg/orchestra` is the same code path the CLI uses — `orchestra run` is a thin wrapper over `orchestra.Run`. Load (or build) a `Config`, call `Run`, inspect per-agent results.

```go
import (
	"context"
	"fmt"
	"os"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

func main() {
	res, err := orchestra.LoadConfig("orchestra.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, err) // I/O or parse failure
		os.Exit(1)
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, w)
	}
	if !res.Valid() {
		fmt.Fprintln(os.Stderr, res.Err())
		os.Exit(1)
	}

	out, err := orchestra.Run(context.Background(), res.Config,
		orchestra.WithEventHandler(func(ev orchestra.Event) {
			orchestra.PrintEvent(os.Stderr, ev)
		}),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, agent := range out.Agents {
		fmt.Printf("%s: %s (%d turns, %.2f USD)\n",
			name, agent.Status, agent.NumTurns, agent.CostUSD)
	}
}
```

`Start` returns a `*Handle` for the streaming case (`Events()`, `Wait()`, `Cancel()`, `Send`, `Interrupt`). Options are `WithWorkspaceDir`, `WithEventBuffer`, `WithEventHandler`. The surface is **experimental** — see [CHANGELOG.md](CHANGELOG.md) for per-release changes.

## Configuration reference

### Top-level

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Project name |
| `agents` | yes | List of agent definitions (legacy `teams:` accepted with a warning) |
| `backend` | no | `local` (default) or `managed_agents`; scalar or mapping form |
| `defaults` | no | Defaults applied to every agent |
| `coordinator` | no | Deprecated; the spawn is suppressed with a warning |

### `defaults`

| Field | Default | Description |
|-------|---------|-------------|
| `model` | `sonnet` | Claude model for all agents |
| `max_turns` | `200` | Max agentic turns per agent |
| `permission_mode` | `acceptEdits` | Permission mode for the `claude` subprocess |
| `timeout_minutes` | `30` | Per-agent spawn timeout |
| `ma_concurrent_sessions` | `20` | Cap on in-flight `Beta.Sessions.New` calls (MA backend; bounds against the 60/min org limit) |
| `requires_credentials` | — | Credential names every agent needs; resolved at run start (see [credentials](#credentials)) |

### `agents[]`

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Unique agent identifier |
| `lead.role` | yes | Role description injected into the prompt |
| `lead.model` | no | Model override for this agent |
| `context` | no | Technical context injected verbatim |
| `tasks` | yes | At least one task (`summary` required; `details` and `verify` recommended; `deliverables` optional) |
| `depends_on` | no | Names of agents this one depends on |
| `members` | no | Sub-roles (`role` + `focus`); presence enables team-lead mode. Ignored with a warning under `managed_agents` |
| `requires_credentials` | no | Credential names this agent needs (unioned with `defaults`) |
| `skills` | no | Skills to attach to the MA agent (resolve via `orchestra skills upload`). MA backend only |
| `custom_tools` | no | Host-side custom tools by registered name. MA backend only |
| `files` | no | Host files to upload + mount read-only in the MA container. MA backend only |
| `environment_override.repository` | no | Per-agent repository override (MA repo flow) |

Validation is two levels. **Hard errors** block the run: empty project/agent name, no agents or no tasks, duplicate names, `depends_on` on an unknown agent, self-dependency, dependency cycle. **Soft warnings** print but don't block: an agent with > 5 members, a task/member ratio outside 2–8, a task missing `details` or `verify`, and the legacy-key / unsupported-field-under-backend notices.

### Solo vs team-lead mode

An agent's mode is determined by `members`. **Solo** (no `members`): the lead works through all tasks directly. **Team-lead** (`members` present): the lead spawns teammates via `TeamCreate`, assigns tasks, and verifies. Team-lead mode is `local`-backend only.

## Backends

The top-level `backend` selects where agents run; the default is `local`. Cross-agent data flows the same way on both: each agent's final output is persisted under `.orchestra/results/<agent>/summary.md` and inlined into the prompts of downstream agents.

### `backend: local` (default)

Each agent is a `claude -p` subprocess on the host. Agents have direct host filesystem access; `skills`, `custom_tools`, and `files` are ignored with a warning.

### `backend: managed_agents`

Each agent is a sandboxed [Managed Agents](https://platform.claude.com/docs/en/managed-agents) session provisioned through the Anthropic Beta SDK. Requires `ANTHROPIC_API_KEY`.

```yaml
backend:
  kind: managed_agents
defaults:
  ma_concurrent_sessions: 20
```

Agents whose deliverable is code can push to a deterministic branch instead of returning a text summary. Add a `repository` block:

```yaml
backend:
  kind: managed_agents
  managed_agents:
    repository:
      url: https://github.com/your-user/your-repo
      mount_path: /workspace/repo  # default
      default_branch: main         # default
    open_pull_requests: false      # true also opens PRs host-side
```

Per-agent overrides go on `agents[i].environment_override.repository`. Orchestra resolves a GitHub PAT at startup: `GITHUB_TOKEN` → `github_token` in `<user-config-dir>/orchestra/credentials.json` → `github_token` in `<user-config-dir>/orchestra/config.json` (legacy; warns). The token is never persisted. Each agent pushes to `orchestra/<agent>-<run-id>`; downstream agents get each upstream branch mounted read-only at `/workspace/upstream/<upstream-agent>/`. On `end_turn`, Orchestra reads the branch via the GitHub API and records a `repository_artifacts[]` entry on the agent in `state.json`.

Caveats under `managed_agents`: `members:` and `coordinator:` are ignored (warned). Cross-repo dependencies are out of scope (warned; upstream branch not mounted). Generic `requires_credentials` secrets do not reach the sandbox yet — see [Status](#status).

A two-agent repo example lives under [`examples/ma_repo_relay/`](examples/ma_repo_relay/orchestra.yaml); a text-only multi-agent example under [`examples/ma_multi_team/`](examples/ma_multi_team/orchestra.yaml). Opt-in live-MA fixtures live under [`test/integration/`](test/integration/).

## Inter-agent communication

Three primitives carry everything the v2 message bus and coordinator used to:

| Primitive | What it does | Surface |
|---|---|---|
| `signal_completion(artifacts={...})` | An agent emits structured outputs at completion; Orchestra captures them host-side. | Per-agent custom tool. Caps: 256 KB / artifact, 4 MB / signal. |
| `get_artifacts` / `read_artifact` | The chat-side LLM reads what an agent produced. | MCP, read-only. |
| `steer` / `orchestra msg` | Inject a `user.message` into a running MA agent's session. | MCP / CLI. MA backend only. |

### Steering a run

While `orchestra run` is in flight, three commands nudge a running agent without restarting:

```bash
orchestra sessions ls                                       # what's currently steerable
orchestra msg --team <name> --message "use the JSON store"  # deliver a user.message
orchestra interrupt --team <name>                           # deliver a user.interrupt
```

These read the workspace's `state.json` lock-free (atomic writes keep the snapshot consistent; data may be stale but is never torn) to look up the agent's MA session id, then talk to Managed Agents directly. The running `orchestra run` picks up MA's echo and logs it as `[agent] human: <text>`. `orchestra msg` is at-least-once with retry on 429/5xx (pass `--no-retry` for at-most-once); `orchestra interrupt` is always at-most-once. All three require `backend: managed_agents`.

## Credentials

`orchestra credentials set/get/list/delete` manages secrets stored under `<user-config-dir>/orchestra/credentials.json`. Names listed in `defaults.requires_credentials` or `agents[i].requires_credentials` are resolved at run start (failing fast on any missing name). On `backend: local` the resolved values reach `claude -p` via the child environment. On `managed_agents` they are resolved but **not** injected into the sandbox (SDK gap — see [Status](#status)); GitHub uses the `repository` resource path instead.

## Companion skills

Orchestra ships [Claude Code skills](https://docs.anthropic.com/en/docs/claude-code) under `.claude/skills/`, available when you open the project in Claude Code:

| Skill | Description |
|-------|-------------|
| `/orchestra-coord` | Bootstrap a session as the human coordinator — reads config, shows status, starts a monitor loop. |
| `/orchestra-init` | Interactively generate an `orchestra.yaml` from a short conversation. |
| `/orchestra-monitor` | Single-pass status dashboard (agent progress, costs, activity); pair with `/loop`. |

The older `/orchestra-msg` and `/orchestra-inbox` skills targeted the v2 file message bus and no longer function; use `steer` and `read_artifact`.

## Workspace

`orchestra run` creates an `.orchestra/` directory. All writes are atomic (write `.tmp`, then `os.Rename`); concurrent access within a single run process is mutex-guarded.

```
.orchestra/
├── state.json          # per-agent status, results, token counts, duration
├── registry.json       # execution metadata: PIDs, session IDs, timestamps
├── results/<agent>/    # per-agent result + summary.md inlined into downstream prompts
├── artifacts/          # signal_completion(artifacts={...}) payloads
├── logs/<agent>.log    # raw NDJSON stream from the agent
└── archive/<run-id>/   # prior run state, results, logs
```

## Develop

```bash
make build       # build ./orchestra
make test        # go test ./...
make test-race   # go test -race ./... (needs CGO)
make vet         # go vet ./...
make lint        # go vet + golangci-lint
make check       # lint + test + build
make install     # build + copy to $GOBIN
```

The unit + integration suite covers config, DAG, injection, spawner, store, MCP, and the SDK. End-to-end tests build the real binary and drive a mock `claude` script emitting valid stream-json — the spawner is tested through its real code path, no interfaces or mocks. Live Managed Agents fixtures (`make e2e-ma*`) hit real Anthropic infrastructure, spend tokens, and are opt-in (see [TESTING.md](TESTING.md)).

## Docs

| Doc | Contents |
|-----|----------|
| [docs/DESIGN-v3-composable-workflows.md](docs/DESIGN-v3-composable-workflows.md) | The v3 design: artifact substrate, steering, removal of the message bus and coordinator |
| [docs/feedback-phase-a-dogfood.md](docs/feedback-phase-a-dogfood.md) | Self-dogfood findings that drove the phase-A polish |
| [CHANGELOG.md](CHANGELOG.md) | Per-release public-surface changes (SDK is experimental) |
| [TESTING.md](TESTING.md) | Test layout + live-MA cost estimates |

## License

MIT
