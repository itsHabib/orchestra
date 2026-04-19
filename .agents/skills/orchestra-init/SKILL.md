---
name: orchestra-init
description: Interactively create an orchestra.yaml config for multi-team project orchestration.
argument-hint: [optional output path]
disable-model-invocation: false
---

# Orchestra Init Skill

Create an `orchestra.yaml` config from a short interactive conversation. Asks the user a few questions, infers team structure and DAG dependencies, shows a plan, then writes the file.

## Step 1 — Gather project details

Ask questions in TWO rounds. The first round captures what the project IS; the second round captures structural preferences.

### Round 1 — Project identity (use AskUserQuestion)

Ask these together:

1. **Project name** — what is this project called?
2. **What does it do?** — Ask the user to describe the project's PURPOSE, core features, and what a user/customer does with it. This is the most important question — do NOT offer generic category options like "Web app" or "CLI tool". Instead, use a single free-text option that prompts the user to describe their specific project. Example option labels: "I'll describe it" + "I have a doc/README to share". The user MUST provide a real description — without it you cannot design meaningful teams or tasks.
3. **Tech stack / languages** — languages, frameworks, key dependencies.

### Round 2 — Structure & runtime preferences (use AskUserQuestion)

Only ask this AFTER you have the project description from Round 1. Ask all of these together:

1. **Team count** — Based on the description, suggest a specific number of teams with names (e.g., "3 teams: backend, frontend, integration") as the recommended option, plus "auto" and a custom option. Show the user you understood their project.
2. **Model** — Which model should agents use? Options: "sonnet (Recommended)" (fast, good for most tasks), "opus" (strongest reasoning, slower and more expensive), "haiku" (fastest, cheapest, for simple tasks).
3. **Permission mode** — How much freedom should agents have? Options: "acceptEdits (Recommended)" (agents can read/write files but need approval for shell commands), "bypassPermissions" (agents can run any command freely — use when agents need to build, test, and install dependencies without blocking).

If `$ARGUMENTS` contains a description or output path, use it as context and skip questions you can already answer.

### Critical rule

Do NOT proceed to Step 2 until you have a concrete understanding of what the project does, its core features, and its domain. If the user gives a vague answer (e.g., just "a web app"), ask follow-up questions. You need enough detail to write specific, useful tasks — not generic boilerplate.

## Step 2 — Analyze and design the config

From the user's answers, infer the full orchestra config:

### Teams
- Name each team after its domain (e.g., `backend`, `frontend`, `data-pipeline`, `infra`)
- Each team gets a `lead` with a descriptive `role`
- Prefer teams with 2-5 `members` — this is what makes orchestra valuable. Even a small-scope team benefits from a "doer" and a "verifier" split. Solo agents (no members) are acceptable for very narrow tasks but should be the exception, not the default. Each member needs a distinct `role` and `focus` that owns a specific piece of the team's work.
- Write a rich `context` block for each team describing tech choices, conventions, schemas, and integration points

### DAG dependencies
- Determine which teams depend on which (e.g., `frontend` depends on `backend`, `integration` depends on both)
- No circular dependencies allowed
- Teams with no dependencies run in parallel (tier 0)

### Tasks per team
- 4-7 tasks per team (enough to distribute across members), each with:
  - `summary` (required): short one-line description
  - `details`: specific requirements, acceptance criteria
  - `deliverables`: list of files/dirs this task produces
  - `verify`: shell command to verify completion (e.g., `go test ./...`, `npm run build`)

### Defaults
Use the user's answers from Round 2 for `model` and `permission_mode`. For fields not asked:

| Field | Default | When to change |
|-------|---------|---------------|
| `max_turns` | `200` | Increase for large teams |
| `timeout_minutes` | `30` | Increase for teams with long builds |

## Step 3 — Print the execution plan

Show a formatted summary for user review:

```
## Orchestra: <name>

### Defaults
model: <model> | max_turns: <N> | timeout: <N>min | permission: <mode>

### Execution Tiers

**Tier 0** (parallel):
  - <team-name>: <lead role> + <N members>
    Tasks: <task summaries, comma-separated>

**Tier 1** (depends on: <tier 0 teams>):
  - <team-name>: <lead role> + <N members>
    Tasks: <task summaries, comma-separated>

...

### Files
  orchestra.yaml  — full orchestration config
```

Ask: "Does this look right? I'll write `orchestra.yaml`."

## Step 4 — Write `orchestra.yaml`

Write the file to the current directory (or to `$ARGUMENTS` if it looks like a path).

The YAML must follow this exact schema:

```yaml
name: "<project name>"

defaults:
  model: <model>
  max_turns: <number>
  permission_mode: <mode>
  timeout_minutes: <number>

teams:
  - name: <team-name>
    lead:
      role: "<role description>"
      model: <optional override>
    members:                           # omit if solo agent
      - role: "<member role>"
        focus: "<what this member owns>"
    context: |
      <rich domain context: tech stack, conventions, schemas, APIs,
       integration points. This is injected into every prompt for
       this team — make it specific and useful.>
    depends_on:                        # omit if tier 0
      - <team-name>
    tasks:
      - summary: "<short description>"
        details: "<specific requirements and acceptance criteria>"
        deliverables:
          - "<file or directory path>"
        verify: "<shell command to verify completion>"
```

### Quality checklist before writing
- Every team has at least one task
- Every task has a `summary`
- All `depends_on` references point to existing team names
- No circular dependencies
- `context` blocks are specific and useful (not generic filler)
- `verify` commands are real commands that would work
- `deliverables` list actual file paths

## Step 5 — Print next steps

After writing the file, print:

```
orchestra.yaml written!

Next steps:

  1. Review the generated config:
       cat orchestra.yaml

  2. Validate the config:
       orchestra validate orchestra.yaml

  3. Preview the execution plan:
       orchestra plan orchestra.yaml

  4. Run the orchestration:
       orchestra run orchestra.yaml
```
