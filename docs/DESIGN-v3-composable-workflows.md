# DESIGN-v3: Composable Workflows on Orchestra

Created: 2026-05-02
Last revised: 2026-05-02 (post adversarial review + simplifications)
Status: Draft (proposed)
Owners: Michael Habib
Related: `docs/DESIGN-v2.md` (MA backend, repo-as-artifact-bus); `docs/DESIGN-ship-feature-workflow.md` (first concrete recipe); `docs/feedback-mcp-server.md` (MCP feedback from dogfood run #1); `docs/reviews/v3-{architecture-critic,scope-skeptic,alternatives-advocate}.md` (adversarial review of an earlier draft)

---

## 1. TL;DR

Orchestra v2 is a static DAG runner: write `orchestra.yaml`, get parallel-within-tier / sequential-across-tiers execution of agents, plus signal_completion. **It runs one graph end-to-end.**

v3 turns Orchestra into a **substrate for composable, multi-stage workflows**, with the following simplifications baked in (changes from the first draft, post-review):

- **MA is the default path; local is the dev-convenience path that happens to share filesystem.** Every primitive must work cleanly on MA first; local follows.
- **There is no message bus.** Inter-agent communication is via DAG-shaped data flow, not free-form chat. The existing `send_message`/`read_messages` surface is replaced by two narrower primitives: agents emit `signal_completion(...)` (terminal output, captured host-side from session events); coordinators emit `steer(...)` (a one-way `user.message` into a running session).
- **Artifacts are payloads of signal events, persisted host-side.** No separate filesystem storage layer. Both backends produce signal events; orchestra captures and stores them once, server-side, regardless of where the agent ran.
- **Credentials (e.g., `GITHUB_TOKEN`) are a Phase A hard requirement, not a §12 open question.** The dogfood proved that any "ship a PR" recipe is gated on this. MA-first means we solve it in the first ship.
- **Recipes are the language; agents are the runtime; the chat-side LLM is the author.** No GUI, no marketplace, no streaming.

This is not a workflow framework. It's a small set of orthogonal primitives — DAG, agents, signal events, steering, recipes — that the chat-side LLM (or a saved skill) composes for real work.

## 2. Motivation

### 2.1 Where we are
- v1 (local) and v2 (MA) prove the DAG-of-agents primitive works.
- The dogfood run on 2026-05-02 (`docs/feedback-mcp-server.md`) ran two parallel agents through MCP. Real implementation work happened in MA; only credential injection blocked the PR ship.
- Every reviewer of the prior v3 draft (`docs/reviews/`) identified the same shape problems: artifact storage was filesystem-only (broken on MA), the message bus was over-engineered for the actual workflow shape, scope was padded.

### 2.2 The vision
Feature work has obvious phases that today are stitched together by humans or one-off skills:

1. **Design** — designer agent drafts; critic agent challenges; reviewer agent gates. Output: design artifact.
2. **Implementation** — engineers execute against the design. Output: PRs.
3. **Review and feedback** — reviewers gate. Failures loop back. Output: ready-to-merge.

Each phase is a sub-DAG. Each phase's output flows to the next via structured signal events. Each phase has gate criteria that determine proceed / loop / abort.

The user's framing is the binding requirement: *"being able to orchestrate this with orchestra primitives is really where I want the project to go."* Orchestra primitives, composed into recipes, executed through MCP — that's the deliverable.

### 2.3 Why now
- v2 stabilized MA + MCP. Foundation is solid.
- The dogfood and the adversarial review surfaced exactly the gaps that v3 closes.
- "Composition lives in the LLM" stays the architectural thesis. The chat-side LLM gets richer primitives to compose with.

## 3. Goals

### 3.1 Functional (must, for v3.0)
1. **Multi-phase composability** — recipes chain phases (design → impl → review); phases pass structured signal-event payloads to one another.
2. **Reusable agent templates** — named, parameterized agent definitions instantiated multiple times in one recipe or across recipes.
3. **Structured outputs as signal events** — agents produce typed outputs via `signal_completion(status, artifacts={...})`; orchestra persists host-side.
4. **Bounded conditional re-entry** — recipes can express "if review_rejected, loop back to engineer; max 3 iterations." No Turing-completeness; predicate-guarded loops with caps only. v3.0 ships hardcoded predicates (`signal_status == "blocked"`, `signal_status == "rejected"`); a richer expression language is v3.x if motivated.
5. **Naming alignment** — replace `team` with `agent` in user-facing surfaces (CLI, MCP, docs). Aliases for backwards compat.
6. **Recipe registry + MCP surface** — recipes are first-class: discoverable via `list_recipes`, invokable via `run_recipe`, inspectable via `get_recipe`. Same for templates.
7. **Both backends supported, MA-first** — every recipe primitive must work cleanly on MA. Local is the cheaper-because-shared-filesystem path; nothing in the design *requires* local-specific behavior.
8. **Steering primitive** — replaces the message bus. `mcp__orchestra__steer(run_id, agent, content)` injects a `user.message` into a running session. Backend-specific implementation, unified surface.
9. **Credential injection (NEW, surfaced by dogfood)** — recipes can declare `requires_credentials: [github_token, anthropic_api_key, ...]`; orchestra resolves from local env/config/secret store and injects into the agent's session at start. Without this, no "ship a PR" recipe can run on MA. Phase A blocker.

### 3.2 Functional (should, for v3.x)
1. Recipe versioning with `name@version` syntax.
2. Artifact lineage in `get_run` (which agent produced which artifact, when, against which inputs).
3. Dry-run / planning mode (`run_recipe(..., dry_run=true)` returns resolved DAG without spawning).

### 3.3 Non-functional (binding)
1. **MA-first.** Every primitive's correctness is verified on MA before it's verified on local.
2. **Backwards compatibility for v2 yaml.** Existing `orchestra.yaml` keeps working. v3 is additive.
3. **No new latency tax on MA.** Recipe resolution is local YAML/JSON expansion; ≤ 100 ms before kickoff.
4. **No prompt-cache regression.** Template instantiation must hit the prompt cache the same way an inline prompt does.
5. **Observability does not regress.** Existing `RunView` fields keep working; new fields are additive.
6. **One transport for inter-agent data.** Signal events and steering, both backend-neutral. **No filesystem-shared-workspace assumptions.**
7. **Composer = LLM, not orchestra.** No editor, no DSL beyond YAML/JSON, no GUI.
8. **Senior Go style and existing conventions.** Atomic file writes, `internal/`, ctx-first, terse comments.

## 4. Non-goals (binding)

- Visual workflow builder / GUI.
- Turing-complete loop semantics.
- Workflow-level transactions / rollback.
- Cross-org recipe marketplace (use git).
- Free-form inter-agent messaging / chat. The bus is removed; agents communicate via DAG edges + signal events. **If you find yourself wanting two agents to chat live, that's a sign your DAG is wrong.**
- Reactive / event-triggered workflows (recipes are invoked, not subscribed to).
- Streaming partial outputs between agents (artifacts are emitted at signal_completion).
- Provider-neutral abstractions (Claude only — local CLI + MA).
- Workflow-as-code Go DSL.
- Sub-recipes / nested recipe invocation in v3.0 (deferred to v3.x; complexity outweighs evidence so far — see §10).

## 5. Existing primitives

| Primitive | Where | Status in v3 |
|---|---|---|
| DAG with tiers | `internal/dag/` | KEEP |
| Agent execution (claude -p / MA Session) | `internal/spawner/` | KEEP, generalized for templates |
| Roles | `internal/injection/` | KEEP |
| `signal_completion(status, summary, pr_url?)` | `internal/customtools/` | KEEP, EXTEND with `artifacts={...}` payload |
| Run state | `internal/run/`, `internal/store/filestore/` | KEEP, EXTEND |
| Agent / env caches (MA) | `internal/agents/` | KEEP (with the 401-fallback fix from feedback-mcp-server.md) |
| MCP server | `internal/mcp/` | KEEP, MAJOR EXTEND |
| File-based message bus (local only) | `internal/messaging/` | **DELETE** |
| MCP `send_message` / `read_messages` | `internal/mcp/messages_tool.go` | **REPLACE** with `steer` (and `read_signal_events` if useful) |

## 6. Design overview

```
┌──────────────────────────────────────────────────────────────────────┐
│ Recipe (YAML)                                                        │
│   declarative DAG of agent-template instances + parameter bindings   │
│   + artifact contracts + (optional) gate predicates + credentials    │
└──────────────────────────────────────────────────────────────────────┘
                                  │ instantiated by
                                  ▼
┌──────────────────────────────────────────────────────────────────────┐
│ Agent template (YAML)                                                │
│   named, parameterized agent definition (role, prompt template,      │
│   declared inputs, declared output schema)                           │
└──────────────────────────────────────────────────────────────────────┘
                                  │ executed by
                                  ▼
┌──────────────────────────────────────────────────────────────────────┐
│ Agent run (existing primitive, renamed from "team")                  │
│   single claude-p subprocess (local) or MA session, deps + signals   │
└──────────────────────────────────────────────────────────────────────┘

Data flow between agents:
  Agent A → signal_completion(artifacts={key: value}) → orchestra captures
  via session events → orchestra persists host-side → Agent B's prompt
  template interpolates: "{{ artifacts.A.key }}" → Agent B sees value.

Coordinator can steer at any time:
  mcp__orchestra__steer(run_id, agent="X", content="...") → backend-specific
  user.message injection (MA: session-events; local: stdin/file/restart).
```

A run is to a recipe what an instance is to a class. `run_recipe(name, params)` resolves recipe → instantiates templates → builds DAG → spawns agents → captures signal events → persists artifacts → triggers gates → loops or proceeds → returns final outputs.

## 7. Detailed design

### 7.1 The "agent" rename

`team` → `agent` in user-facing surfaces:
- `orchestra.yaml`: `teams:` → `agents:` (parser accepts both during transition).
- MCP `inline_dag.teams` → `inline_dag.agents` (same alias).
- `RunView.Teams` → `RunView.Agents` (field renamed; old name aliased for one minor version).
- CLI: `--team` → `--agent` (alias).
- Internal Go: `internal/spawner/Team` → `Agent`. Mechanical.

Why now: v3 is the breaking-change window anyway. Bundling the rename costs less than ~6 months from now after another wave of references.

### 7.2 Structured outputs as signal-event payloads

**Replaces the artifact-storage layer from the previous draft.** Agents do not write to a filesystem path. They include outputs in `signal_completion`:

```
signal_completion(
  status="done",
  summary="...",
  artifacts={
    "design_doc": {"type": "text", "content": "<markdown>"},
    "decision": {"type": "json", "content": {"decision": "proceed", "rationale": "..."}}
  }
)
```

Orchestra's spawner already captures every tool-use as a session event. The signal-event payload is persisted host-side in the run's state, regardless of backend (local or MA). The host-side store is the canonical artifact location.

**Schema validation.** The agent template declares each artifact's type and (optional) JSON schema:

```yaml
outputs:
  design_doc:
    type: text
  decision:
    type: json
    schema:
      type: object
      required: [decision, rationale]
      properties:
        decision: {enum: [proceed, revise, abort]}
```

Orchestra validates the artifacts at the moment of capture. Schema mismatch → orchestra rejects the signal_completion and surfaces a clear error to the agent (and the next steer can correct).

**Reading artifacts downstream.** Recipe interpolation uses `{{ artifacts.AGENT.KEY }}` Go-template-style syntax (deliberately different from gate expressions, see §7.5):

```yaml
agents:
  - name: implementer
    template: engineer
    deps: [designer]
    inputs:
      design: "{{ artifacts.designer.design_doc }}"
```

For text-typed artifacts the interpolation pastes content. For json-typed, the agent receives a serialized form (and can `json.parse` it in its own logic).

**Why this works for both backends:** signal events are produced by every backend's spawner already (orchestra captures stream-json from local `claude -p` subprocess; orchestra subscribes to `session.events` from MA). The new tooling is host-side capture-and-persist; agent-side it's just "include artifacts in signal_completion." The MA spike's "container writes invisible to host" finding is irrelevant — we never write to the container's filesystem in the first place.

### 7.3 Reusable agent templates

Definition format (YAML), in `templates/` (project-local) or `~/.config/orchestra/templates/` (user-global):

```yaml
# templates/critic-with-rubric.yaml
name: critic-with-rubric
version: 1
role: critic
parameters:
  rubric: {type: text, required: true}
prompt: |
  You are a critical reviewer. Apply this rubric to the input artifact.
  
  Rubric:
  {{ params.rubric }}
  
  Input:
  {{ inputs.draft }}
  
  Produce a structured critique...

inputs:
  draft: {type: text, required: true}

outputs:
  critique:
    type: json
    schema: {...}
```

Instantiation in a recipe:
```yaml
agents:
  - name: critic_a
    template: critic-with-rubric@1
    params:
      rubric: |
        - is the design SOLID?
        - are constraints respected?
    deps: [designer]
    inputs:
      draft: "{{ artifacts.designer.design_doc }}"
```

Resolution: at recipe load time, the template engine merges defaults + per-instance params, renders the prompt, validates inputs against declared types, and produces a fully-resolved agent definition that the existing spawner consumes. **No spawner changes** — templates are a pre-processor.

Versioning: `name@1` pins to a major version. Without a version, latest is used (with a load-time warning). Templates evolve as new versions; old versions stay available.

### 7.4 Recipes

A recipe is a YAML file in `recipes/<name>.yaml` (project-local) or `~/.config/orchestra/recipes/<name>.yaml` (user-global). Phases are an authoring convenience — at runtime they expand into a flat DAG with synthetic gate nodes between phase boundaries.

```yaml
name: feature-end-to-end
version: 1
description: "Design → impl → review for one feature."

requires_credentials: [github_token]   # NEW: Phase A blocker

parameters:
  feature_name: {type: string, required: true}
  feature_brief: {type: text, required: true}
  repo: {type: string, default: "itsHabib/orchestra"}

phases:
  design:
    agents:
      - name: designer
        template: designer@1
        params: {brief: "{{ params.feature_brief }}"}
      - name: critic
        template: critic-with-rubric@1
        params: {rubric: "{{ load 'rubrics/design.md' }}"}
        deps: [designer]
        inputs: {draft: "{{ artifacts.designer.design_doc }}"}
      - name: design_reviewer
        template: reviewer@1
        deps: [critic]
        inputs: {draft: "{{ artifacts.designer.design_doc }}", critique: "{{ artifacts.critic.critique }}"}
    gate:
      predicate: "design_reviewer.signal_status == done"   # see §7.5
      on_fail: { re_enter: design, max_iters: 2 }

  implementation:
    deps: [design]
    agents:
      - name: engineer
        template: engineer@1
        params: {repo: "{{ params.repo }}"}
        inputs: {design: "{{ artifacts.designer.design_doc }}"}

  review:
    deps: [implementation]
    agents:
      - name: pr_reviewer
        template: pr-reviewer@1
        inputs: {pr_url: "{{ artifacts.engineer.pr_url }}"}
    gate:
      predicate: "pr_reviewer.signal_status == done"
      on_fail: { re_enter: implementation, max_iters: 3 }

outputs:
  pr_url: "{{ artifacts.engineer.pr_url }}"
  design_doc: "{{ artifacts.designer.design_doc }}"
```

### 7.5 Gates: hardcoded predicates in v3.0

To answer the architecture-critic and scope-skeptic concerns about the prior gate-DSL plan, **v3.0 ships only a small set of hardcoded predicate forms**:

| Predicate form | Meaning |
|---|---|
| `<agent>.signal_status == done` | Last agent in phase signaled `status="done"`. |
| `<agent>.signal_status == blocked` | Last agent in phase signaled `status="blocked"`. |
| `<agent>.signal_status in [done, ok]` | Status matches any of the listed values. |

That's it. No general expression language, no JSON path access, no comparisons over artifact contents. **If real recipes outgrow this in v3.x, we add a small expression engine then, with concrete evidence.** For now, decide-by-status covers the design → impl → review loop.

If a recipe author needs richer gates than status checks (e.g., "decision artifact says proceed"), they can express it as **another agent** that reads the artifact and signals `done` or `blocked`. This makes gate logic agent-shaped, not DSL-shaped — addressing the alternatives-advocate critique that gates often need agent judgment.

Re-entry semantics:
- A failed gate triggers re-execution of the named phase.
- The re-entry counter is recorded in `state.json.phase_iters`.
- Re-entered agents see the previous gate failure as additional input (so the engineer knows what review feedback to address).
- Cap: `max_iters` (default 3). After cap, the recipe fails with `phase_X: max_iters_exceeded` in `RunView.LastError`.

### 7.6 (Removed: Sub-recipes were §7.6 in the prior draft)

Deferred to v3.x. Reasons:
- The architecture-critic correctly identified that load-time inline expansion is irreconcilable with a `instances: "{{ params.N }}"` fan-out.
- The scope-skeptic correctly identified that no current evidence motivates this primitive in v3.0.
- The chat-side LLM can compose recipes by calling `run_recipe` multiple times.

If real workflows in v3.0 hit this wall, we revisit with a concrete proposal.

### 7.7 MCP surface (full v3 shape)

#### 7.7.1 Tools

| Tool | Inputs | Output | Notes |
|---|---|---|---|
| `run` | `{inline_dag?, config_path?, project_name?}` | `{run_id, ...}` | EXISTING. `inline_dag.teams` aliased to `inline_dag.agents`. |
| `get_run` | `{run_id}` | `RunView` (extended) | EXISTING + many new fields per dogfood feedback (`Phase`, `LastError`, `iter_count`, `Agents[].LastTool`, `LastEventAt`, `Tokens`, `Artifacts`). |
| `list_runs` | `{active_only?}` | `[RunView]` | EXISTING. Same shape extension. |
| `cancel_run` | `{run_id, reason?}` | `{cancelled_at}` | NEW. The long-deferred ask from `docs/feedback-mcp-server.md`. |
| **NEW** `steer` | `{run_id, agent, content}` | `{steered_at}` | One-way `user.message` injection into a running agent. Backend-specific implementation. **Replaces `send_message`** (which was bus-shaped). |
| **NEW** `run_recipe` | `{name, version?, params, project_name?, dry_run?}` | `{run_id, ...}` | Resolves recipe + params → DAG; spawns. `dry_run=true` returns resolved DAG without spawning. |
| **NEW** `list_recipes` | `{}` | `[RecipeMeta]` | Project-local + user-global recipes. |
| **NEW** `get_recipe` | `{name, version?}` | `RecipeView` | Full recipe definition. |
| **NEW** `list_templates` | `{}` | `[TemplateMeta]` | Agent templates. |
| **NEW** `get_template` | `{name, version?}` | `TemplateView` | Full template definition. |
| **NEW** `get_artifacts` | `{run_id, agent?, phase?}` | `[ArtifactMeta]` | Lists artifacts produced by signal_completion calls; metadata only. |
| **NEW** `read_artifact` | `{run_id, agent, key}` | `{type, content}` | Reads one artifact's content. |
| **REMOVED** `send_message` | — | — | Bus is deleted. Use `steer` for coordinator → agent; nothing for agent → agent (use DAG edges + signal events). |
| **REMOVED** `read_messages` | — | — | Bus is deleted. Use `read_artifact` for outputs; `get_run.Agents[i].LastEventAt`/`LastTool` for "what is the agent doing right now." |

#### 7.7.2 Resources

| URI | Returns |
|---|---|
| `orchestra://runs` | EXISTING. |
| `orchestra://runs/{id}` | EXISTING. |
| **NEW** `orchestra://runs/{id}/artifacts` | All artifacts. |
| **NEW** `orchestra://runs/{id}/artifacts/{agent}/{key}` | One artifact's content. |
| **NEW** `orchestra://recipes` | Recipe catalog. |
| **NEW** `orchestra://recipes/{name}` | One recipe. |
| **NEW** `orchestra://templates` | Template catalog. |
| **NEW** `orchestra://templates/{name}` | One template. |
| **REMOVED** `orchestra://runs/{id}/messages` | — |

#### 7.7.3 DTOs (additive over `internal/mcp/dto.go`)

```go
type RunView struct {
    // existing fields...
    RecipeName    string         `json:"recipe_name,omitempty"`
    RecipeVersion int            `json:"recipe_version,omitempty"`
    Phase         string         `json:"phase,omitempty"`
    PhaseIters    map[string]int `json:"phase_iters,omitempty"`
    LastError     string         `json:"last_error,omitempty"`
    Agents        []AgentView    `json:"agents"`        // renamed from Teams
}

type AgentView struct {
    Name         string    `json:"name"`
    Status       string    `json:"status"`
    SignalStatus string    `json:"signal_status,omitempty"`
    Phase        string    `json:"phase,omitempty"`
    LastTool     string    `json:"last_tool,omitempty"`
    LastEventAt  time.Time `json:"last_event_at,omitempty"`
    LastError    string    `json:"last_error,omitempty"`
    Tokens       TokenView `json:"tokens"`
    Artifacts    []string  `json:"artifacts,omitempty"`   // keys produced
}

type ArtifactMeta struct {
    RunID      string    `json:"run_id"`
    Agent      string    `json:"agent"`
    Phase      string    `json:"phase,omitempty"`
    Key        string    `json:"key"`
    Type       string    `json:"type"`           // text | json
    SizeBytes  int64     `json:"size_bytes"`
    ProducedAt time.Time `json:"produced_at"`
}

type RecipeMeta struct {
    Name                 string      `json:"name"`
    Version              int         `json:"version"`
    Description          string      `json:"description"`
    Parameters           []ParamMeta `json:"parameters"`
    Phases               []string    `json:"phases"`
    RequiresCredentials  []string    `json:"requires_credentials,omitempty"`
}

type TemplateMeta struct {
    Name        string    `json:"name"`
    Version     int       `json:"version"`
    Role        string    `json:"role"`
    Description string    `json:"description"`
    Parameters  []ParamMeta `json:"parameters"`
    Inputs      []IOMeta  `json:"inputs"`
    Outputs     []IOMeta  `json:"outputs"`
}
```

DTOs live in `internal/mcp/dto.go`. Internal types stay internal; the MCP layer is a thin adapter (no business logic), per the layering rule from `docs/IDEAS-orchestra-mcp-addon.md`.

### 7.8 Storage layout

```
~/.config/orchestra/
├── recipes/                       # user-global recipes
├── templates/                     # user-global templates
├── config.json                    # api_key, secrets
├── credentials.json               # NEW: secret store for github_token, etc.
├── agents.json                    # MA agent cache
└── envs.json                      # MA env cache

<project>/
├── orchestra.yaml                 # legacy (still works)
├── recipes/                       # project-local recipes
└── templates/                     # project-local templates

<workspace>/.orchestra/
├── state.json                     # run state (extended with phase, phase_iters, agent.artifacts[])
├── registry.json
├── run.lock
├── results/                       # per-agent signal_completion summaries (existing)
├── logs/<agent>.ndjson            # per-agent activity log (existing)
└── artifacts/                     # NEW: host-side persistence of signal-event artifacts
    └── <agent>/<key>.{txt,json}
```

**No more `messages/` dir.** No more "filesystem-shared workspace" assumption. The `artifacts/` dir on the host is populated by orchestra capturing signal events from either backend's stream — agents never write to it directly.

### 7.9 Internal Go layout

```
cmd/recipe.go                        # NEW: orchestra recipe {list,show,run,validate}
cmd/template.go                      # NEW: orchestra template {list,show,validate}
internal/recipes/                    # NEW: recipe loading + resolution (reintroduced from old delete)
internal/templates/                  # NEW: template loading + parameter binding
internal/artifacts/                  # NEW: signal-event capture + host-side persistence + read API
internal/credentials/                # NEW: secret store + per-recipe injection (Phase A)
internal/mcp/recipes_tool.go         # NEW
internal/mcp/templates_tool.go       # NEW
internal/mcp/artifacts_tool.go       # NEW
internal/mcp/cancel_tool.go          # NEW
internal/mcp/steer_tool.go           # NEW (replaces messages_tool.go)
internal/run/recipe_lifecycle.go     # NEW: phase transitions + gate eval + re-entry
internal/customtools/signal_with_artifacts.go  # NEW: extends signal_completion to accept artifacts={}
internal/messaging/                  # **DELETED**
internal/mcp/messages_tool.go        # **DELETED** (alongside the bus)
```

The `internal/messaging/` deletion is a tracked PR dependency: if any caller outside `internal/mcp/` uses the bus, it's removed in the same PR. Per recent PRs (#29, #30) only the MCP layer used messaging anyway.

## 8. Tradeoffs

### 8.1 YAML for recipes vs Go-defined recipes
**Chosen:** YAML.
**Alternative:** Go-defined recipes (the `internal/recipes/` from before #29).
**Tradeoff:** Go gives type safety + IDE help. YAML gives discoverability, LLM-authorability, hot-reload, shareability. The "LLM is the composer" thesis makes YAML correct here. Type safety recovered via schema validation.

### 8.2 Gates: hardcoded predicates vs expression DSL
**Chosen:** Hardcoded `signal_status == X` predicates.
**Alternative:** A mini-DSL for arbitrary expressions over artifacts.
**Tradeoff:** A DSL is more expressive but premature. Status-only predicates plus "use another agent for richer gates" is sufficient for the design → impl → review loop. Cheaper, no parser to write, easy to extend later.

### 8.3 Removing the message bus
**Chosen:** Remove. Replace with `signal_completion` (terminal artifacts) + `steer` (coordinator→agent injection).
**Alternative:** Keep as a backend-neutral inter-agent comms primitive.
**Tradeoff:** The bus was solving "agents chat" — but real workflows don't chat, they pipeline. The dogfood prompt explicitly told agents not to use messaging. Removing the bus collapses two transports (filesystem on local, session-events on MA) into one, eliminates the artifact-storage cross-backend pain, and shrinks `internal/messaging/` from the codebase entirely. If a real workflow ever needs free-form chat, that's a sign the DAG is wrong.

### 8.4 MA-first vs equal backend support
**Chosen:** MA-first; local is a happy-cheaper-case.
**Alternative:** Treat backends as equally privileged.
**Tradeoff:** "Equally privileged" pushed the prior draft to design for the lowest common denominator (filesystem-shared workspace), which is wrong for MA. MA-first lets us pick the right primitive (signal events) and let local be a fast happy-path. The user has stated MA is the strategic direction, so this aligns with intent.

### 8.5 Hard-coded credential injection vs general secret broker
**Chosen:** Recipe declares `requires_credentials: [...]`; orchestra resolves from `~/.config/orchestra/credentials.json` or env.
**Alternative:** A general secret-broker integration (Vault, 1Password CLI, etc.).
**Tradeoff:** Brokers add infra weight. A simple local file + env path covers the dogfood use case (GitHub token + Anthropic key). Brokers are a v4 concern.

### 8.6 Sub-recipes deferred
**Chosen:** Not in v3.0.
**Alternative:** Inline-expand or recursive runs.
**Tradeoff:** The architecture-critic correctly identified that inline-expand + dynamic instance counts are irreconcilable. Recursive runs fragment observability. Without strong evidence of the use case, deferring is correct.

## 9. Migration plan (v2 → v3)

Principle: every existing user is unaffected unless they opt in.

| What user has today | What v3 does |
|---|---|
| `orchestra.yaml` with `teams:` | Parsed unchanged (alias). |
| `mcp__orchestra__run` with `inline_dag.teams` | Accepted unchanged. |
| `signal_completion(summary)` | Still valid. Recipes that don't declare outputs treat the agent as artifact-less. |
| `mcp__orchestra__send_message` | **Tool removed.** Callers must migrate to `steer` (for coordinator→agent) or read `get_run.Agents[i].Artifacts` (for inter-agent data). |
| `mcp__orchestra__read_messages` | **Tool removed.** Callers migrate to `read_artifact` and the new `get_run` fields. |
| `internal/messaging/` package callers | None outside `internal/mcp/` per recent PRs. Mechanical removal in the same PR. |

The `send_message` removal is a breaking MCP change. Documented in the migration guide; warn in `mcp__orchestra__run`'s tool description.

Deprecation timeline:
- v3.0: `team`/`teams` accepted with a warning. `send_message`/`read_messages` removed.
- v3.x: warnings escalate.
- v4.0: `team` removed.

## 10. Phasing / rollout

**v3.0 ships in two phases, then a polish pass.** Designed so each phase ships independently and provides standalone value.

### Phase A (foundation — single sprint, ~1 week)
Goal: substrate. Bus removed; signal-events the canonical inter-agent transport; observability fixes from `feedback-mcp-server.md`; credentials work; `agent` rename.

- A.1: `team` → `agent` rename, with aliases.
- A.2: `RunView.Agents[i].LastError`, `LastTool`, `LastEventAt`, `Tokens` populated from session events.
- A.3: `cancel_run` MCP tool.
- A.4: `signal_completion(artifacts={})` extension; orchestra captures artifacts from session events; persists in `<workspace>/.orchestra/artifacts/` host-side.
- A.5: `get_artifacts`, `read_artifact` MCP tools.
- A.6: `steer` MCP tool (replaces `send_message`); delete `internal/messaging/`.
- A.7: **Credential injection.** `~/.config/orchestra/credentials.json` schema; `requires_credentials: []` field on recipes (and on inline_dag for direct callers); orchestra reads + injects via env into MA session start (and into local `claude -p` env). First credential supported: `github_token`.

**Ships:** Working substrate. Bus is gone. Observability is real. Artifacts flow through signal events end-to-end. Credentials injected. Bus-removal-only consumers can migrate.

### Phase B (templates + recipes — single sprint, ~1.5 weeks)
Goal: workflows are first-class.

- B.1: `internal/templates/` — load, validate, parameter-bind.
- B.2: `internal/recipes/` — load, validate, expand to flat DAG with synthetic gate nodes.
- B.3: Phase transitions + status-predicate gates + bounded re-entry.
- B.4: `cmd/recipe.go`, `cmd/template.go`.
- B.5: MCP tools: `list_recipes`, `get_recipe`, `run_recipe` (+ `dry_run`), `list_templates`, `get_template`.
- B.6: Three canonical templates (`designer`, `engineer`, `reviewer`) and one canonical recipe (`feature-end-to-end`).

**Ships:** First end-to-end recipe runnable from a chat-side LLM via one MCP call. The vision is operational.

### Phase C (polish + dogfood + cookbook — short, ~half sprint)
- C.1: Run `feature-end-to-end` against a real orchestra backlog item end-to-end on MA.
- C.2: Cookbook of 3–5 worked examples.
- C.3: README rewrite and migration guide.
- C.4: Performance benchmark: recipe expansion ≤ 100 ms for a 10-agent recipe.
- C.5: Fix anything the dogfood surfaces.

**Total: ~3 sprints (~3 weeks).** This is materially shorter than the prior draft's 6 phases / 5 sprints, by cutting sub-recipes, the gate DSL, and the artifact filesystem layer.

## 11. Testing and validation plan

Repo convention: real binary + mock claude script, no mocks at the seams.

### 11.1 Unit
- `internal/templates/`: parameter binding edge cases.
- `internal/artifacts/`: signal-event capture under contention; type validation; oversize rejection; schema-mismatch reporting.
- `internal/recipes/`: every cookbook recipe expands cleanly; bad recipes rejected at validate time.
- `internal/credentials/`: env override, file fallback, missing-credential detection.
- DTO round-trip: every new DTO JSON-serializes losslessly.

### 11.2 Integration
The mock claude script grows behaviors:
- "produce artifact X with content Y on prompt Z"
- "include artifacts in signal_completion"
- "fail with session.error of type T"

Integration tests at `test/integration/`:
- `recipe_linear` — design → impl → review with no gates.
- `recipe_with_gate_pass` — first iter passes.
- `recipe_with_gate_fail_recover` — fails once, succeeds on second iter.
- `recipe_with_gate_max_iters` — fails 3 times, recipe fails clean.
- `recipe_template_param_binding` — same template instantiated 3× with different params.
- `recipe_dry_run` — DAG returned without spawning.
- `recipe_credentials_injected` — agent can read injected env var.
- `steer_into_running_agent` — coordinator injects user.message; agent responds in next turn.

### 11.3 Both backends in CI
Every recipe-level integration test runs against both backends. Local-backend matrix uses mock claude; MA matrix uses real MA, gated by env var so devs without access skip.

### 11.4 MA-first verification gate
**Phase ship gate:** Phase A and Phase B PRs cannot merge until their integration tests pass on MA first. Local-passes-but-MA-fails is a hard block.

### 11.5 Self-dogfood
Phase C re-runs the dogfood-1 flow (the PR-shipping pattern) using the new `feature-end-to-end` recipe. The expected outcome: PRs land on real GitHub via injected credentials, no manual intervention. This is the success bar.

### 11.6 Cookbook validation
Every cookbook recipe passes `orchestra recipe validate`. Every cookbook template passes `orchestra template validate`. `make cookbook-test` runs `orchestra recipe run --dry-run cookbook/X --params ...` against each.

## 12. Open questions and risks

### 12.1 Open questions
- **Steering on local backend.** MA has `session.user.message`; local has `claude -p` subprocess with stdin. Options: (a) keep `claude -p` open and write to stdin on steer; (b) signal file the agent polls; (c) "no mid-flight steering on local in v3.0; restart instead." Probably (a). Decide in Phase A.
- **Artifact size limits.** `signal_completion(artifacts={"x": "<huge string>"})` could blow up. Soft cap at 256KB per artifact, hard cap at 4MB total per signal_completion call. Above that → write to a host-side file and reference by path (only practical on local; MA needs a different escape hatch). Defer detailed handling to v3.x.
- **Template prompt-cache contract.** Template `prompt: |` content is interpolated per-instance; orchestra must take care that the cache-stable prefix is identical across instances so prompt caching doesn't degrade. Worth a dedicated test in Phase B.
- **`requires_credentials` discovery for in-flight runs.** What if a recipe declares a credential not on the host? Fail at `run_recipe` time with a clear message naming which credential. Phase A handles this.

### 12.2 Risks
- **Removing the bus is breaking.** Mitigated by clear migration. If we discover a real use case the bus solved that signal-events + steer can't, we revisit.
- **Recipe debugging difficulty.** Mitigated by `RunView.Phase`, per-agent `LastError`, `LastTool`, and `read_artifact`. A `cmd/recipe.go debug <run-id>` subcommand prints phase-tree-with-status.
- **MA-first validation requires real MA in CI.** Without it, MA gates are aspirational. Solve before Phase A merges (CI matrix decision).
- **Cross-version drift.** Each recipe declares `min_orchestra_version`; CLI errors clearly if mismatched.
- **Credential surface area.** Storing tokens in `~/.config/orchestra/credentials.json` is fine for personal use but not for shared environments. Document; defer broker integration.

## 13. Out of scope re-emphasis

**Not in v3:**
- GUI / visual builder.
- Cron / webhook / file-watch triggers.
- Recipe marketplace.
- Cross-run pub/sub.
- Streaming partial outputs.
- Provider-neutral abstractions.
- Workflow-as-code Go DSL.
- Free-form inter-agent messaging (the bus).
- Sub-recipes / nested invocations.
- Sub-recipe `instances:` fan-out.
- Gate expression DSL beyond hardcoded status predicates.

If any of these come up in PR review, the answer is: "Not in v3, see §4 / §13."

## 14. Success criteria

- A new user runs `mcp__orchestra__run_recipe(name='feature-end-to-end', params={...})` from a fresh Claude Code session and gets a real, multi-phase run that ships a PR end-to-end on MA, with credentials cleanly injected.
- The dogfood-1 flow's friction (no GH auth, retries-exhausted-but-no-reason, lost-implementation-in-MA-container) all disappear.
- `mcp__orchestra__get_run` returns enough information to debug a failed recipe without ever opening `.orchestra/logs/*.ndjson` by hand.
- A new recipe can be authored in YAML by a chat-side LLM, validated, and run successfully — no Go required.
- The `re-entry` gate is exercised by at least one production-shaped recipe before v3.0 GA.
- `internal/messaging/` is gone; `internal/mcp/` is shorter than it is today (despite gaining new tools), because `messages_tool.go` is replaced by the smaller `steer_tool.go`.

## 15. Appendix: the canonical recipe (Phase B target)

```yaml
name: feature-end-to-end
version: 1
description: "One feature, end to end: design → impl → review → ready to merge."

requires_credentials: [github_token]

parameters:
  feature_name: {type: string, required: true}
  feature_brief: {type: text, required: true}
  repo: {type: string, default: "itsHabib/orchestra"}

phases:
  design:
    agents:
      - name: designer
        template: designer@1
        params:
          brief: "{{ params.feature_brief }}"
      - name: critic
        template: critic-with-rubric@1
        params:
          rubric: "{{ load 'rubrics/design.md' }}"
        deps: [designer]
        inputs:
          draft: "{{ artifacts.designer.design_doc }}"
      - name: design_reviewer
        template: reviewer@1
        deps: [critic]
        inputs:
          draft: "{{ artifacts.designer.design_doc }}"
          critique: "{{ artifacts.critic.critique }}"
    gate:
      predicate: "design_reviewer.signal_status == done"
      on_fail: { re_enter: design, max_iters: 2 }

  implementation:
    deps: [design]
    agents:
      - name: engineer
        template: engineer@1
        params:
          repo: "{{ params.repo }}"
          branch: "feature/{{ params.feature_name }}"
        inputs:
          design: "{{ artifacts.designer.design_doc }}"

  review:
    deps: [implementation]
    agents:
      - name: pr_reviewer
        template: pr-reviewer@1
        inputs:
          pr_url: "{{ artifacts.engineer.pr_url }}"
    gate:
      predicate: "pr_reviewer.signal_status == done"
      on_fail: { re_enter: implementation, max_iters: 3 }

outputs:
  pr_url: "{{ artifacts.engineer.pr_url }}"
  design_doc: "{{ artifacts.designer.design_doc }}"
```

End of document.
