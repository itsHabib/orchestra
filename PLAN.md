# Orchestra: Multi-Team Project Orchestration

## What This Is

A Go CLI + Claude Code skill that orchestrates large software projects across multiple teams. You define your entire project upfront — all teams, their members, their tasks, their dependencies — in one YAML file. You hit run, and it builds your project team by team in the right order, passing context forward.

```
You write:  orchestra.yaml  (the whole project plan)
You run:    orchestra run orchestra.yaml
You get:    a built project
```

## How It Works

### The Core Loop

1. **Parse config** — read teams and their `depends_on` relationships
2. **Build DAG** — topological sort (Kahn's algorithm) produces execution tiers
3. **Execute tier by tier** — teams within a tier run in parallel, tiers run sequentially
4. **State flows forward** — each team gets the results/context from all prior teams injected into its prompt

### Example

Given this config:
```yaml
teams:
  - name: backend        # no deps → tier 0
  - name: frontend       # depends_on: [backend] → tier 1
    depends_on: [backend]
  - name: devops          # depends_on: [backend] → tier 1 (parallel with frontend)
    depends_on: [backend]
  - name: integration     # depends_on: [frontend, devops] → tier 2
    depends_on: [frontend, devops]
```

Execution:
```
Tier 0: [backend]              ← runs alone
Tier 1: [frontend, devops]     ← both run in parallel (both only need backend)
Tier 2: [integration]          ← waits for tier 1, gets ALL prior results
```

### Teams vs Solo Agents

Each config entry can be either:

- **A team** (has `members`) — CLI spawns a `claude -p` session for the lead. The lead's prompt tells it to call `TeamCreate`, assign work to teammates via `SendMessage`, coordinate their work, verify, and return a result. The lead is a top-level `claude -p` session so it CAN call `TeamCreate`.

- **A solo agent** (no `members`) — CLI spawns a single `claude -p` session that just does the work directly. No `TeamCreate` needed.

### Agents Are Pure Functions

- **Input**: a prompt with everything the agent needs (role, tasks, results from prior teams)
- **Output**: JSON result captured from `claude -p --output-format stream-json`
- **No mid-run communication**. No messaging. No polling. No inbox.
- The CLI is the coordinator — it reads results, updates shared state, builds prompts for the next tier.

### How Completion Detection Works

`claude -p --output-format stream-json` streams NDJSON lines. The final line is always:
```json
{"type": "result", "subtype": "success", "result": "...", "cost_usd": 0.45, "num_turns": 42, "duration_ms": 120000, "session_id": "abc-123"}
```
The CLI blocks on each spawned process. When the process exits, the result is captured. No polling needed.

---

## Config Format (orchestra.yaml)

```yaml
name: "my-saas-app"

defaults:
  model: sonnet
  max_turns: 200
  permission_mode: acceptEdits
  timeout_minutes: 45

teams:
  - name: backend
    lead:
      role: "Backend Lead"
      model: opus                    # override default model for this lead
    context: |                       # domain-specific context injected into all prompts
      Tech stack: Go 1.22, Chi router, sqlc for query generation, Postgres 16.
      Auth: JWT access tokens (15min TTL) + refresh tokens in httpOnly cookies.
      API style: REST, JSON request/response, standard HTTP status codes.
      Validation: use go-playground/validator struct tags.
      Error format: {"error": "message", "code": "MACHINE_READABLE"}.
    members:
      - role: "API Engineer"
        focus: "REST endpoints, request validation, error handling"
      - role: "DB Engineer"
        focus: "Postgres schema, migrations, query layer using sqlc"
      - role: "Auth Specialist"
        focus: "JWT access/refresh tokens, RBAC middleware, session management"
    tasks:
      - summary: "Design and implement the REST API"
        details: "Create Chi router with /api/v1 prefix. Endpoints: users CRUD, auth (login/signup/refresh/logout). Use middleware for request logging and panic recovery."
        deliverables: ["src/api/router.go", "src/api/handlers/", "src/api/middleware/"]
        verify: "go build ./... && go test ./src/api/..."
      - summary: "Set up database with migrations"
        details: "Postgres schema for users, sessions, roles tables. Use golang-migrate for migrations. Generate type-safe queries with sqlc."
        deliverables: ["migrations/", "src/db/queries/", "src/db/sqlc/"]
        verify: "sqlc generate && go build ./..."
      - summary: "Implement authentication and authorization"
        details: "JWT signing/verification, refresh token rotation, RBAC middleware with roles: admin, user, viewer."
        deliverables: ["src/auth/jwt.go", "src/auth/rbac.go", "src/api/middleware/auth.go"]
        verify: "go test ./src/auth/..."

  - name: frontend
    depends_on: [backend]
    lead:
      role: "Frontend Lead"
    context: |
      Framework: React 18 + TypeScript, Vite bundler, TanStack Query for data fetching.
      Styling: Tailwind CSS. Component library: shadcn/ui.
      Auth: JWT stored in httpOnly cookies (set by backend), frontend just calls /api/v1/auth/*.
      Routing: react-router v6 with protected route wrapper.
    members:
      - role: "UI Engineer"
        focus: "React components, routing, protected routes, Tailwind styling"
      - role: "API Integration"
        focus: "TanStack Query hooks, API client with interceptors, error boundaries"
    tasks:
      - summary: "Build core layout and routing"
        details: "App shell with sidebar nav, protected route wrapper that redirects to /login if no session. Pages: Dashboard, Users, Settings, Login, Signup."
        deliverables: ["src/components/Layout.tsx", "src/routes/", "src/components/ProtectedRoute.tsx"]
        verify: "npm run build && npm run typecheck"
      - summary: "Implement API client and data fetching"
        details: "Axios client with base URL config, request/response interceptors for auth. TanStack Query hooks for each backend endpoint. Error boundary for API failures."
        deliverables: ["src/api/client.ts", "src/api/hooks/", "src/components/ErrorBoundary.tsx"]
        verify: "npm run build && npm run typecheck"
      - summary: "Implement auth flows"
        details: "Login form with validation, signup with password strength indicator, auto-refresh token on 401, logout clears session."
        deliverables: ["src/pages/Login.tsx", "src/pages/Signup.tsx", "src/hooks/useAuth.ts"]
        verify: "npm run build && npm run typecheck"

  - name: devops
    depends_on: [backend]
    lead:
      role: "DevOps Lead"
    context: |
      Containerization: multi-stage Docker builds. Backend is a static Go binary.
      CI: GitHub Actions. Run lint, test, build, push image.
      Target: single docker-compose for local dev, Dockerfile ready for cloud deploy.
    tasks:                            # no members = solo agent
      - summary: "Dockerize all services"
        details: "Multi-stage Dockerfile for Go backend (build + scratch). Docker-compose with backend, postgres, and frontend dev server. Include .dockerignore."
        deliverables: ["Dockerfile", "docker-compose.yml", ".dockerignore"]
        verify: "docker compose build"
      - summary: "Set up CI/CD pipeline"
        details: "GitHub Actions workflow: lint (golangci-lint), test (go test -race), build Docker image, push to ghcr.io on main branch."
        deliverables: [".github/workflows/ci.yml"]
        verify: "act --list"

  - name: integration
    depends_on: [frontend, devops]
    lead:
      role: "QA Lead"
    context: |
      Test framework: Playwright for e2e. Test against docker-compose stack.
      Coverage targets: auth flows, CRUD operations, error states.
      Run all tests headless in CI.
    tasks:                            # no members = solo agent
      - summary: "Write end-to-end tests"
        details: "Playwright tests for: login/logout flow, signup with validation errors, CRUD users as admin, 403 on unauthorized access. Use page object pattern."
        deliverables: ["e2e/tests/", "e2e/pages/", "playwright.config.ts"]
        verify: "npx playwright test --reporter=list"
      - summary: "Verify deployment pipeline"
        details: "docker-compose up, wait for health checks, run e2e suite, tear down. Script should exit non-zero on any failure."
        deliverables: ["scripts/integration-test.sh"]
        verify: "bash scripts/integration-test.sh"
```

---

## CLI Commands

| Command | Purpose |
|---------|---------|
| `orchestra validate <config.yaml>` | Parse + validate config, print warnings (team size, task quality), exit |
| `orchestra init <config.yaml>` | Validate config, create `.orchestra/` workspace with seeded state + registry |
| `orchestra run <config.yaml>` | Full orchestration: init → DAG → spawn tiers → collect → summary |
| `orchestra spawn <config.yaml> --team <name>` | Spawn a single team (used internally by `run` and by the skill) |
| `orchestra status [--workspace .orchestra/]` | Print team tree with statuses, costs, durations |

---

## Workspace Layout (.orchestra/)

Created by `orchestra init`. Grows as teams complete.

```
.orchestra/
├── state.json              # shared state — what each team produced (updated after each tier)
├── registry.json           # all teams: name, status, pid, session_id, timestamps
├── results/
│   └── <team-name>.json    # full result per team
└── logs/
    └── <team-name>.log     # raw claude -p stdout capture
```

### state.json (the shared context that grows)

After backend completes:
```json
{
  "project": "my-saas-app",
  "teams": {
    "backend": {
      "status": "done",
      "result_summary": "Built REST API with 12 endpoints, JWT auth, Postgres with 4 tables...",
      "artifacts": ["src/api/", "src/db/", "migrations/"],
      "cost_usd": 1.20,
      "duration_ms": 130000
    }
  }
}
```

When frontend spawns, this state is injected into its prompt — so the frontend lead knows exactly what backend built.

---

## Package Structure

```
agent-orchestra/
├── main.go
├── go.mod                          # module: github.com/michaelhabib/orchestra
├── cmd/
│   ├── root.go                     # cobra root command, global flags
│   ├── validate.go                 # orchestra validate — parse + check config + print warnings
│   ├── init_cmd.go                 # orchestra init
│   ├── run.go                      # orchestra run — the main orchestration loop
│   ├── spawn.go                    # orchestra spawn --team <name>
│   └── status.go                   # orchestra status
├── internal/
│   ├── config/
│   │   ├── schema.go               # Config, Team, Lead, Member, Defaults types + Validate()
│   │   └── loader.go               # Load(path) → *Config
│   ├── workspace/
│   │   ├── workspace.go            # Workspace struct, Init(), Open()
│   │   ├── state.go                # State, TeamState types + read/write (atomic rename)
│   │   ├── registry.go             # Registry, RegistryEntry types + read/write (atomic rename)
│   │   └── results.go              # TeamResult type + read/write
│   ├── dag/
│   │   └── dag.go                  # BuildTiers(teams) → [][]string (Kahn's algorithm)
│   ├── injection/
│   │   └── builder.go              # BuildPrompt — constructs the full prompt for a team
│   ├── spawner/
│   │   └── spawner.go              # Spawn(ctx, opts) — runs claude -p, parses stream-json
│   └── log/
│       └── log.go                  # colored, team-prefixed terminal logger
```

Dependencies: `github.com/spf13/cobra`, `gopkg.in/yaml.v3`, `github.com/fatih/color`

---

## Key Types

### Config (internal/config/schema.go)

```go
type Config struct {
    Name     string   `yaml:"name"`
    Defaults Defaults `yaml:"defaults"`
    Teams    []Team   `yaml:"teams"`
}

type Defaults struct {
    Model          string `yaml:"model"`            // default: "sonnet"
    MaxTurns       int    `yaml:"max_turns"`         // default: 200
    PermissionMode string `yaml:"permission_mode"`   // default: "acceptEdits"
    TimeoutMinutes int    `yaml:"timeout_minutes"`   // default: 30
}

type Team struct {
    Name      string   `yaml:"name"`
    Lead      Lead     `yaml:"lead"`
    Members   []Member `yaml:"members"`    // optional — empty = solo agent
    Tasks     []Task   `yaml:"tasks"`
    DependsOn []string `yaml:"depends_on"` // team names this depends on
    Context   string   `yaml:"context"`    // domain-specific context injected into prompts
}

type Task struct {
    Summary      string   `yaml:"summary"`       // short description (required)
    Details      string   `yaml:"details"`        // specific requirements, tech choices, constraints
    Deliverables []string `yaml:"deliverables"`   // files/dirs this task produces
    Verify       string   `yaml:"verify"`         // command to verify completion
}

type Lead struct {
    Role  string `yaml:"role"`
    Model string `yaml:"model"`  // override defaults.model
}

type Member struct {
    Role  string `yaml:"role"`
    Focus string `yaml:"focus"`
}

// Validate checks:
// - non-empty name, non-empty teams, unique team names
// - each team has at least one task with non-empty summary
// - depends_on references exist, no self-references, no cycles
// - team sizing warnings (soft): members > 5 logs a warning
// - task ratio warnings (soft): < 2 or > 8 tasks per member logs a warning
// Hard errors prevent execution. Warnings print but don't block.
func (c *Config) Validate() error

// ResolveDefaults fills zero-value fields with defaults.
func (c *Config) ResolveDefaults()

// TeamByName returns a pointer to the team or nil.
func (c *Config) TeamByName(name string) *Team

// HasMembers returns true if the team has members (is a real team, not solo).
func (t *Team) HasMembers() bool
```

### Workspace types (internal/workspace/)

```go
// state.go
type State struct {
    Project string                `json:"project"`
    Teams   map[string]TeamState  `json:"teams"`
}
type TeamState struct {
    Status        string   `json:"status"`          // "pending", "running", "done", "failed"
    ResultSummary string   `json:"result_summary"`
    Artifacts     []string `json:"artifacts"`
    CostUSD       float64  `json:"cost_usd"`
    DurationMs    int64    `json:"duration_ms"`
}

// registry.go
type Registry struct {
    Project string          `json:"project"`
    Teams   []RegistryEntry `json:"teams"`
}
type RegistryEntry struct {
    Name      string    `json:"name"`
    Status    string    `json:"status"`     // "pending", "running", "done", "failed"
    SessionID string    `json:"session_id"`
    PID       int       `json:"pid"`
    StartedAt time.Time `json:"started_at,omitempty"`
    EndedAt   time.Time `json:"ended_at,omitempty"`
}

// results.go
type TeamResult struct {
    Team       string  `json:"team"`
    Status     string  `json:"status"`      // "success", "error"
    Result     string  `json:"result"`      // the agent's final output text
    CostUSD    float64 `json:"cost_usd"`
    NumTurns   int     `json:"num_turns"`
    DurationMs int64   `json:"duration_ms"`
    SessionID  string  `json:"session_id"`
}
```

### Workspace operations (internal/workspace/workspace.go)

```go
type Workspace struct {
    Path string
    mu   sync.Mutex  // protects concurrent writes within one orchestra process
}

func Init(cfg *config.Config) (*Workspace, error)    // creates .orchestra/ + seeds files
func Open(path string) (*Workspace, error)            // opens existing workspace

func (w *Workspace) ReadState() (*State, error)
func (w *Workspace) WriteState(s *State) error                          // atomic: write tmp → os.Rename
func (w *Workspace) UpdateTeamState(name string, ts TeamState) error    // read-modify-write

func (w *Workspace) ReadRegistry() (*Registry, error)
func (w *Workspace) WriteRegistry(r *Registry) error
func (w *Workspace) UpdateRegistryEntry(name string, fn func(*RegistryEntry)) error

func (w *Workspace) WriteResult(r *TeamResult) error
func (w *Workspace) ReadResult(name string) (*TeamResult, error)

func (w *Workspace) LogWriter(teamName string) (io.WriteCloser, error)  // opens logs/<team>.log
```

All writes use atomic rename pattern (write to `.tmp`, then `os.Rename`). The mutex prevents races within a single `orchestra run` process.

---

## Injection Protocol (internal/injection/builder.go)

### Solo agent prompt (no members)

```
You are: {role}
Project: {project_name}

## Technical Context
{team.context}

## Your Tasks
{for each task}
### Task: {task.summary}
{task.details}
Expected deliverables: {task.deliverables}
Verify: `{task.verify}`
{end}

## Context from Previous Teams
{for each dependency}
### {dep_name} ({dep_role}) — Completed
Summary: {result_summary}
Artifacts: {artifact_list}
{end}

## Instructions
Work through your tasks in order. After completing each task, run its
verify command to confirm it works. When all tasks are done, provide a
brief summary of what you accomplished and list all files created/modified.
```

### Team lead prompt (has members)

```
You are: {role}
Project: {project_name}

## Technical Context
{team.context}

## Your Tasks
{for each task}
### Task: {task.summary}
{task.details}
Expected deliverables: {task.deliverables}
Verify: `{task.verify}`
{end}

## Your Team
You have {N} teammates. Assign each teammate 2-6 tasks from the list above
based on their focus area. Each teammate's spawn prompt MUST include:
1. The full Technical Context above (they don't inherit your conversation)
2. Their specific assigned tasks with details, deliverables, and verify commands
3. Any relevant results from previous teams

Teammates:
{for each member}
- {member.role}: {member.focus}
{end}

## Context from Previous Teams
{for each dependency}
### {dep_name} ({dep_role}) — Completed
Summary: {result_summary}
Artifacts: {artifact_list}
{end}

## Instructions
1. Use TeamCreate to create your team and assign tasks to teammates based on
   their focus areas. Give each teammate a detailed prompt — include technical
   context, specific tasks with verify commands, and relevant upstream results.
   They cannot see your conversation, so the prompt is ALL they get.
2. Use SendMessage to coordinate with teammates and relay relevant messages
   from the message bus. Only the team lead polls the message bus.
3. As results come back, run each task's verify command yourself to confirm
4. If a verify fails, use SendMessage to give the teammate specific feedback
5. When all tasks pass verification, provide your summary
```

### Function signatures

```go
// BuildPrompt constructs the full prompt for a team's claude -p session.
// If the team has members, includes TeamCreate/spawn instructions.
// If the team has dependencies, includes their result summaries from state.
func BuildPrompt(team config.Team, projectName string, state *workspace.State, cfg *config.Config) string
```

The `cfg` parameter is needed to look up dependency teams' lead roles for the context section.

---

## Spawner (internal/spawner/spawner.go)

```go
type SpawnOpts struct {
    TeamName       string
    Prompt         string
    Model          string    // "sonnet", "opus", etc.
    MaxTurns       int
    PermissionMode string
    TimeoutMinutes int
    LogWriter      io.Writer // tee stdout to this (the log file)
    Command        string    // defaults to "claude" — tests override to a mock script
}

// Spawn runs claude -p with the given prompt, parses stream-json output,
// and returns the structured result. Blocks until the process exits or times out.
func Spawn(ctx context.Context, opts SpawnOpts) (*workspace.TeamResult, error)
```

Internal flow:
1. Build args: `["-p", prompt, "--output-format", "stream-json", "--model", model, "--max-turns", N, "--permission-mode", mode]`
2. Create `exec.CommandContext(ctx, command, args...)`
3. Pipe stdout through `bufio.Scanner` (line-by-line NDJSON)
4. Tee all lines to `LogWriter`
5. For each line, unmarshal JSON:
   - `type: "system", subtype: "init"` → capture `session_id`
   - `type: "assistant"` → capture last text content (fallback)
   - `type: "result"` → build `TeamResult` with all fields
6. If process exits without result message but exit code 0 → use last assistant text, status "success"
7. If timeout → kill process, return error
8. If non-zero exit → return error with stderr

---

## DAG (internal/dag/dag.go)

```go
// BuildTiers takes teams and returns execution tiers using Kahn's algorithm.
// Each tier is a []string of team names that can run in parallel.
// Returns error if there's a cycle.
func BuildTiers(teams []config.Team) ([][]string, error)
```

Standard Kahn's:
1. Build adjacency list + in-degree map from `depends_on`
2. Seed queue with all teams where in-degree == 0
3. Per iteration: current queue = one tier, decrement dependents' in-degree
4. If processed < total → cycle error

---

## Orchestration Loop (cmd/run.go)

```go
func runOrchestration(ctx context.Context, cfg *config.Config) error {
    // 1. Init workspace
    ws, err := workspace.Init(cfg)

    // 2. Build DAG
    tiers, err := dag.BuildTiers(cfg.Teams)

    // 3. Execute tier by tier
    for tierIdx, tierNames := range tiers {
        logger.TierStart(tierIdx, tierNames)

        // Read current state (includes all prior tier results)
        state, _ := ws.ReadState()

        // Spawn all teams in this tier concurrently
        type result struct {
            name string
            res  *workspace.TeamResult
            err  error
        }
        results := make(chan result, len(tierNames))
        var wg sync.WaitGroup

        for _, name := range tierNames {
            wg.Add(1)
            go func(teamName string) {
                defer wg.Done()
                team := cfg.TeamByName(teamName)

                // Update registry → running
                ws.UpdateRegistryEntry(teamName, func(e *workspace.RegistryEntry) {
                    e.Status = "running"
                    e.StartedAt = time.Now()
                })

                // Build prompt with current state
                prompt := injection.BuildPrompt(*team, cfg.Name, state, cfg)
                logWriter, _ := ws.LogWriter(teamName)
                defer logWriter.Close()

                // Spawn claude -p
                res, err := spawner.Spawn(ctx, spawner.SpawnOpts{
                    TeamName:       teamName,
                    Prompt:         prompt,
                    Model:          team.Lead.Model, // or cfg.Defaults.Model
                    MaxTurns:       cfg.Defaults.MaxTurns,
                    PermissionMode: cfg.Defaults.PermissionMode,
                    TimeoutMinutes: cfg.Defaults.TimeoutMinutes,
                    LogWriter:      logWriter,
                })

                results <- result{teamName, res, err}
            }(name)
        }

        wg.Wait()
        close(results)

        // Collect results, update state
        var failed []string
        for r := range results {
            if r.err != nil {
                failed = append(failed, r.name)
                ws.UpdateTeamState(r.name, workspace.TeamState{Status: "failed"})
                ws.UpdateRegistryEntry(r.name, func(e *workspace.RegistryEntry) {
                    e.Status = "failed"
                    e.EndedAt = time.Now()
                })
                continue
            }
            ws.WriteResult(r.res)
            ws.UpdateTeamState(r.name, workspace.TeamState{
                Status:        "done",
                ResultSummary: r.res.Result,
                CostUSD:       r.res.CostUSD,
                DurationMs:    r.res.DurationMs,
            })
            ws.UpdateRegistryEntry(r.name, func(e *workspace.RegistryEntry) {
                e.Status = "done"
                e.SessionID = r.res.SessionID
                e.EndedAt = time.Now()
            })
        }

        if len(failed) > 0 {
            return fmt.Errorf("tier %d: teams failed: %v", tierIdx, failed)
        }
    }

    // 4. Print summary
    printSummary(ws)
    return nil
}
```

---

## Summary Output

After all tiers complete:

```
═══════════════════════════════════════════════════
  Orchestra: my-saas-app — Complete
═══════════════════════════════════════════════════

  Team          │ Status  │ Cost    │ Turns │ Duration
  ──────────────┼─────────┼─────────┼───────┼──────────
  backend       │ success │ $1.20   │ 87    │ 4m 12s
  frontend      │ success │ $0.85   │ 62    │ 3m 05s
  devops        │ success │ $0.45   │ 28    │ 1m 30s
  integration   │ success │ $0.30   │ 15    │ 0m 48s
  ──────────────┼─────────┼─────────┼───────┼──────────
  Total         │         │ $2.80   │ 192   │ 6m 30s

  Wall clock: 6m 30s (tiers 0-2, frontend+devops ran in parallel)
  Results:    .orchestra/results/
  Logs:       .orchestra/logs/
```

---

## Skill: /team-orchestra

Location: `~/.claude/skills/team-orchestra/SKILL.md`

```yaml
---
name: team-orchestra
description: Run a multi-team project orchestration from an orchestra.yaml config. Spawns teams in DAG order with shared state.
argument-hint: [path/to/orchestra.yaml]
disable-model-invocation: false
---
```

The skill is a thin wrapper:
1. Validate that `$ARGUMENTS` points to a valid YAML file
2. Run `orchestra run $ARGUMENTS` via Bash
3. When done, run `orchestra status --workspace .orchestra/` and present results
4. If any team failed, show the relevant log file

---

## Build Order

| Step | What | Files | Depends On |
|------|------|-------|------------|
| 1 | Project scaffold | `go.mod`, `main.go` | — |
| 2 | Config types + validation | `internal/config/schema.go`, `loader.go` | — |
| 3 | Workspace types | `internal/workspace/state.go`, `registry.go`, `results.go` | — |
| 4 | Workspace I/O | `internal/workspace/workspace.go` | step 3 |
| 5 | DAG engine | `internal/dag/dag.go` | step 2 |
| 6 | Logger | `internal/log/log.go` | — |
| 7 | Injection/prompt builder | `internal/injection/builder.go` | steps 2, 3 |
| 8 | Spawner | `internal/spawner/spawner.go` | step 3 |
| 9 | CLI: root, validate, init, status | `cmd/root.go`, `cmd/validate.go`, `cmd/init_cmd.go`, `cmd/status.go` | steps 2, 4 |
| 10 | CLI: spawn | `cmd/spawn.go` | steps 2, 4, 7, 8 |
| 11 | CLI: run | `cmd/run.go` | everything |
| 12 | Tests | `*_test.go` in each package | corresponding package |
| 13 | Skill | `~/.claude/skills/team-orchestra/SKILL.md` | CLI built |

---

## Test Strategy

**config** (`internal/config/`)
- Validate(): unique names, valid deps, self-reference rejection, missing fields, empty teams
- Validate(): team size warning when members > 5
- Validate(): task ratio warning when tasks/members outside [2, 8]
- Validate(): task quality warning when details or verify is empty
- Load(): parse valid YAML, reject invalid YAML, resolve defaults
- Load(): parse structured Task (summary/details/deliverables/verify) correctly

**workspace** (`internal/workspace/`)
- Init() creates correct directory structure
- Read/Write roundtrips for state, registry, results
- Atomic write safety (no partial files)
- Concurrent UpdateTeamState with `-race` flag

**dag** (`internal/dag/`)
- Linear chain: A→B→C = 3 tiers
- Diamond: A→[B,C]→D = 3 tiers
- Parallel: [A,B,C] no deps = 1 tier
- Cycle detection returns error
- Empty input, single team

**injection** (`internal/injection/`)
- Solo agent prompt (no members, no deps) — includes technical context + structured tasks with verify
- Solo agent with dependencies (includes upstream results section)
- Team lead prompt (has members) — includes task assignment instructions + "pass full context to teammates"
- Team lead with dependencies (both members and upstream context)
- Verify that team.context is injected verbatim into the Technical Context section
- Verify that task.details, task.deliverables, task.verify all appear in the prompt

**spawner** (`internal/spawner/`)
- Mock `claude` with a shell script that prints stream-json NDJSON
- Test: success, timeout, error exit code, missing result message fallback
- `SpawnOpts.Command` set to mock script path

**All**: `go test -race ./... && go vet ./...`

---

## Verification Checklist

1. `go build -o orchestra && go test -race ./... && go vet ./...` — all pass
2. `./orchestra init examples/simple.yaml` — creates `.orchestra/` with valid state.json and registry.json
3. `./orchestra status --workspace .orchestra/` — prints team tree (all pending)
4. `./orchestra spawn examples/simple.yaml --team backend` — spawns a real `claude -p` session, captures result
5. `./orchestra run examples/simple.yaml` — full DAG execution end-to-end, all teams succeed, summary prints

---

## Team Sizing & Task Guidance

These are enforced as warnings during `orchestra validate` / `orchestra init`, not hard errors. The CLI prints them prominently so the user can decide.

### Team size: 3-5 members per team (recommended)

- Each teammate has its own context window and costs tokens independently
- Coordination overhead increases non-linearly with team size
- 3 focused teammates often outperform 5 scattered ones
- If you have > 5 members, consider splitting into two teams with a dependency edge

**Validation**: `members` count > 5 → warning. No hard cap — the user might know what they're doing.

### Task count: 2-6 tasks per teammate

- Too few tasks (1) → the coordination overhead of having a teammate wasn't worth it
- Too many tasks (> 8) → the teammate works too long without check-ins, risk of wasted effort
- Sweet spot: 3-5 self-contained tasks that each produce a clear deliverable

**Validation**: `len(tasks) / max(len(members), 1)` outside [2, 8] → warning.

### Task quality: each task should be specific

- **Bad**: `"Build the frontend"` — too large, no clear deliverable, no verify
- **Good**: `summary: "Build login page"` with `details`, `deliverables`, and `verify`
- **Validation**: task with empty `details` or empty `verify` → warning

---

## Design Decisions

- **No messaging / no gossip** — agents are pure functions. State flows through the coordinator (CLI), not between agents. This is simpler and more reliable than cooperative polling.
- **Atomic file writes** (tmp + `os.Rename`) — safe on APFS/ext4, prevents half-written state on crash.
- **Mutex on Workspace, not file locks** — single `orchestra run` process owns all writes. File locking would only matter for concurrent orchestra processes (not supported).
- **`SpawnOpts.Command` for testing** — tests point to a mock shell script instead of real `claude`. No interfaces, no mocks, no dependency injection frameworks.
- **Solo vs team determined by `members` field** — if empty, solo agent. If present, lead gets TeamCreate instructions. One config format, two behaviors.
- **Config is flat** — no phases, no modes, no kickoff configs. Just teams with dependencies. Complexity lives in the injection prompts, not the config schema.
