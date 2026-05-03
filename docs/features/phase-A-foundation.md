# Phase A kickoff: Orchestra v3.0 foundation

Created: 2026-05-02
Use: paste the **BEGIN PROMPT** section into a fresh Claude Code session at the orchestra repo root (`<repo_root>`) to ship Phase A of the v3 design.
Reads from: `docs/DESIGN-v3-composable-workflows.md`, `docs/feedback-mcp-server.md`, `docs/reviews/v3-*.md`.

Phase A goal in one line: **rip out the message bus, add the artifact + credential + observability substrate, rename `team` → `agent`** — so Phase B (recipes + templates) has a clean foundation.

---

## BEGIN PROMPT

```
I'm working on orchestra (Go CLI, multi-agent DAG runner) in the local
clone at `<repo_root>`. We are shipping Phase A of v3, which
is the foundation rewrite: bus removal, artifact substrate, observability
fixes, credentials, agent rename. Phase B (recipes + templates) is OUT OF
SCOPE; do not touch recipes/templates beyond what's listed below.

Read these in order before doing anything (do not skim §10 of the design):

1. docs/DESIGN-v3-composable-workflows.md  — the binding design.
2. docs/feedback-mcp-server.md             — the dogfood findings that drove the design.
3. docs/reviews/v3-architecture-critic.md
   docs/reviews/v3-scope-skeptic.md
   docs/reviews/v3-alternatives-advocate.md  — the adversarial review that shaped the doc.
4. .claude/CLAUDE.md                       — repo conventions (atomic writes, internal/, ctx-first, terse comments).

Strategic framing (binding):
- MA is the default backend; local is the dev-convenience path. Every primitive
  must work on MA first. Local-passes-but-MA-fails is a hard block on every PR.
- "Composition lives in the LLM" is the architectural thesis. Orchestra ships
  primitives, not workflow features.
- The chat-side LLM is the composer. No GUI, no DSL beyond YAML/JSON, no
  marketplace. See §4 / §13 of the design doc for the full non-goals list.

────────────────────────────────────────────────────────────────────────────
SCOPE: Phase A is exactly these five PRs. Each is independently mergeable.

PR 1: `team` → `agent` rename + extended RunView observability
PR 2: `cancel_run` MCP tool
PR 3: artifact substrate (signal_completion(artifacts={}) + persistence + read tools)
PR 4: `steer` MCP tool + delete `internal/messaging/` + delete `mcp__orchestra__send_message` and `read_messages`
PR 5: `internal/credentials/` + recipe credential injection + `cmd/credentials.go`

START with PR 1. PR 2 and PR 5 are independent and can land in parallel after
PR 1. PR 3 must wait for PR 1 (uses RunView.Agents[i].Artifacts). PR 4 must
wait for PR 1 (uses the new RunView shape; deleting send_message touches the
same files renamed in PR 1).

────────────────────────────────────────────────────────────────────────────
PR 1: rename + RunView extension

Modified:
- internal/spawner/: Team → Agent (gofmt -r). Method receivers, types, locals.
- internal/dag/: Team references → Agent.
- internal/run/service.go: TeamTransition → AgentTransition (alias old name).
- internal/store/filestore/: state.json `teams` key — accept on read, write
  under new `agents` key going forward. Add a one-time migration that copies
  `teams` → `agents` in-place if state.json has only `teams`.
- internal/config/: YAML parser accepts both `teams:` and `agents:` (alias);
  writes use `agents:`.
- internal/mcp/dto.go: RunView.Teams → RunView.Agents. KEEP `Teams []AgentView`
  as a JSON alias field tagged `json:"teams,omitempty"` for v3.0 only —
  remove in v3.x.
- internal/mcp/dto.go: extend RunView with `Phase string`, `LastError string`,
  `PhaseIters map[string]int`. Extend AgentView with `LastTool string`,
  `LastEventAt time.Time`, `LastError string`, `Tokens TokenView`,
  `Artifacts []string`.
- internal/mcp/runs.go (or wherever get_run/list_runs build the view): populate
  the new fields from state.json (already tracked) and from per-agent NDJSON
  log tail (LastTool, LastEventAt, LastError).
- internal/spawner/: emit LastTool from session events as they stream in;
  write to state.json on each event.
- cmd/: every flag and message that says "team" → "agent". `--team` aliased
  to `--agent`.
- examples/*/orchestra.yaml: update to `agents:`. Keep ONE example with
  `teams:` for the migration test.
- docs/*.md: every team → agent except historical references.

Source for new fields:
- LastError: most recent `session.error` event's `error.message` from the
  agent's NDJSON log.
- LastTool: most recent `agent.tool_use.name`.
- LastEventAt: most recent event's `processed_at`.
- Tokens: aggregated `model_usage` from `span.model_request_end` events.

Tests (use real binary + mock claude script per repo convention):
- Existing tests pass after rename.
- New: yaml with `teams:` loads; yaml with `agents:` loads; round-trip yields
  `agents:`.
- New: state.json with only `teams` migrates cleanly to `agents`.
- New: mock claude emits a session.error → RunView.Agents[i].LastError set.
- New: mock claude emits successive tool_use → LastTool reflects latest.
- New: token totals match emitted model_usage values.

PR shape: ONE PR. Big diff but mechanical. Run `make vet`, `make test`, and
`go tool golangci-lint run ./...` before push. Title:
"v3 phase A: rename team → agent + extend RunView observability"

────────────────────────────────────────────────────────────────────────────
PR 2: cancel_run

New:
- internal/mcp/cancel_tool.go: handler for cancel_run({run_id, reason?}).
  Sets state.json.run.cancellation = {requested_at, reason}; SIGTERM the
  orchestra subprocess (CTRL_BREAK_EVENT on Windows); for MA, also call
  spawner.UserInterrupt for each running session. Wait up to 10s for cleanup.
- Register in internal/mcp/server.go.

Modified:
- internal/run/service.go: detect cancellation flag; transition all running
  agents to `cancelled` state.

Tests:
- cancel_run on a running mock-claude DAG: agents transition to cancelled,
  run-level status = cancelled.
- cancel_run on MA-backed run (gated by env var): same outcome.
- cancel_run on already-finished run: idempotent, returns gracefully.

PR shape: ONE PR. Title: "v3 phase A: add cancel_run MCP tool"

────────────────────────────────────────────────────────────────────────────
PR 3: artifact substrate (depends on PR 1)

Modified:
- internal/customtools/signal_completion.go: extend tool input schema to
  accept `artifacts: map[string]{type: "text"|"json", content: string|object}`.
  Validate type ∈ {text, json}.
- internal/spawner/: when capturing the agent's signal_completion tool-use
  event from the stream, extract the artifacts map and pass to
  internal/artifacts.Store.Put. Also append each key to
  state.json.agents[name].artifacts[].

New:
- internal/artifacts/store.go: Store interface { Put(runID, agent, key string,
  art Artifact) error; Get(...) (Artifact, error); List(runID, agent string)
  ([]ArtifactMeta, error) }. Filestore impl: writes to
  <workspace>/.orchestra/artifacts/<agent>/<key>.{txt,json} atomically (`.tmp`
  → os.Rename per repo convention). NO compression; no encryption; flat layout.
- internal/mcp/artifacts_tool.go: handlers for get_artifacts(run_id, agent?,
  phase?) and read_artifact(run_id, agent, key).
- Register in internal/mcp/server.go.

Tests:
- Mock claude emits signal_completion(artifacts={"foo": {type: "text",
  content: "bar"}}) → file at .orchestra/artifacts/<agent>/foo.txt;
  state.json updated.
- Same on MA. (Gated.)
- get_artifacts returns metadata; read_artifact returns content.
- Soft cap: artifact > 256KB → orchestra rejects with clear error.
- Hard cap: total artifacts > 4MB per signal_completion → reject.

PR shape: ONE PR. Title: "v3 phase A: artifact substrate via signal events"

────────────────────────────────────────────────────────────────────────────
PR 4: steer + bus deletion (depends on PR 1)

New:
- internal/mcp/steer_tool.go: handler for steer(run_id, agent, content). Read
  state to determine backend; dispatch:
  - MA: resolve agent's MA SessionID via spawner.SteerableSessionID; call
    spawner.SendUserMessage with 4-retry budget (matches `orchestra msg` CLI).
    Reuses the Steerer / SessionSteerer pattern that was dropped in PR #29 —
    bring it back here under steer_tool.go.
  - Local: in v3.0, return a clear "local steering not supported; restart the
    run with appended context" error. Document as a v3.x todo. Reasoning:
    keeping `claude -p` stdin open requires deeper changes in how local agents
    are spawned; not worth the cost in Phase A. MA-first wins.
- Register in internal/mcp/server.go.

Removed:
- internal/messaging/ (entire package).
- internal/mcp/messages_tool.go.
- mcp__orchestra__send_message + read_messages registrations.
- All callers of internal/messaging — search and remove. (Per recent PRs, only
  the MCP layer used it. Verify with grep.)
- docs and examples that mention the bus.

Tests:
- steer on MA-backed running agent: agent's next turn includes the steered
  content as a user.message. (Gated.)
- steer on local-backend running agent: returns the documented error cleanly.
- send_message and read_messages no longer registered in the MCP server.
- Build is clean after deletion (no unused imports, no broken references).

PR shape: ONE PR. Breaking MCP change — call out in description with
migration: "send_message → steer for coordinator→agent; for inter-agent data,
use signal_completion(artifacts={}) + interpolation in downstream prompts."
Title: "v3 phase A: replace send_message bus with steer; delete internal/messaging"

────────────────────────────────────────────────────────────────────────────
PR 5: credentials

New:
- internal/credentials/credentials.go: Resolve(names []string) (map[string]string, error).
  Reads from ~/.config/orchestra/credentials.json (a flat JSON map name → value)
  AND from env vars. Env wins on conflict (dev override). Returns the resolved
  map; errors clearly if a required name is missing in both sources.
- cmd/credentials.go: subcommand `orchestra credentials set/get/list/delete <name>`.
  `set` writes to file with mode 0600. `get` only confirms presence (never prints
  the value). `list` prints names only.

Modified:
- internal/spawner/: each spawner accepts a `requires_credentials []string`
  field on the agent. Local: pass via cmd.Env. MA: pass via Sessions.Create
  env block.
- internal/config/: yaml parser accepts `requires_credentials:` at agent
  level and at `defaults:` level (defaults inherited by all agents).
- internal/mcp/run_tool.go: inline_dag accepts a top-level `requires_credentials`
  array.
- examples/*/orchestra.yaml: NONE NEED THIS YET. Don't add to examples.

Tests:
- Mock claude script reads $GITHUB_TOKEN in turn 1; orchestra injects → token
  available.
- Missing credential: orchestra fails fast at run-time with "credential X is
  required by agent Y but not found in credentials.json or env" — clear
  message naming what's missing.
- File precedence: credentials.json has X, env has Y → env wins.
- `orchestra credentials set foo bar` writes file with mode 0600.
- File mode wrong (e.g., 0644) on read → warn but don't reject (Windows has
  no real chmod equivalent so this is best-effort).

PR shape: ONE PR. Title: "v3 phase A: credential injection (closes the dogfood gh-auth blocker)"

────────────────────────────────────────────────────────────────────────────
COMMON CONVENTIONS (binding):

- All file writes use `.tmp` then os.Rename (atomic). See internal/fsutil/.
- Tests: use real orchestra binary + mock claude script (no spawner mocks).
- ctx-first parameter order; no named returns; terse comments.
- All packages live under internal/ (memory: this is project policy until SDK
  extraction is real work).
- For each PR: `make vet`, `make test`, `go tool golangci-lint run ./...`
  must all be clean BEFORE push. Never `--no-verify`. Never force-push.
- After opening a PR, post these as separate comments in this order:
    1. gh pr comment $PR --body "@codex review"
    2. gh pr comment $PR --body "@claude review"
    3. gh pr edit $PR --add-reviewer copilot-pull-request-reviewer
  (NEVER `@copilot` mention — that triggers a branch edit.)

────────────────────────────────────────────────────────────────────────────
SELF-DOGFOOD (after all 5 PRs merged):

Re-run the dogfood-1 prompt — but now with `requires_credentials: [github_token]`
declared. Expected: ma-dispatch and recipes-cleanup ship two PRs on real
GitHub, no manual intervention. If self-dogfood fails: triage, fix, re-dogfood.
DO NOT start Phase B until self-dogfood is clean.

────────────────────────────────────────────────────────────────────────────
HOUSEKEEPING:

- Anthropic API key: orchestra reads it from
  `~/.config/orchestra/config.json` (`%APPDATA%\orchestra\config.json` on
  Windows) with shape `{"api_key": "sk-ant-..."}`. Use whatever local
  mechanism you already have to populate that file before starting Phase A;
  if it is missing, every MA-backed test will fail at session start with a
  clear "no Anthropic API key" error.
- GitHub auth: use the gh CLI on the host (orchestra does not configure it
  for you). For the self-dogfood at the end, populate
  `~/.config/orchestra/credentials.json` with the credentials your recipes
  declare in `requires_credentials:` — at minimum a `github_token` for any
  PR-shipping flow.
- Active branches that are subsumed by Phase A:
  - `feature/ma-dispatch-send-message` — superseded by PR 4 (steer replaces
    send_message). Close that branch / PR with a "subsumed by v3 Phase A"
    comment if it exists on the remote.
  - `chore/remove-internal-recipes` — the OLD internal/recipes/ package
    deletion. Subsumed; the deletion happens implicitly when nothing in
    Phase A uses it. Close with same comment.
- Never modify settings.json or .claude/ files unless explicitly asked.
- If something in the design doc turns out to be wrong as you go, fix it in
  the doc, don't paper over it in code. Note the change in the PR description.

────────────────────────────────────────────────────────────────────────────
DONE CRITERIA:

1. All 5 PRs merged.
2. internal/messaging/ is gone; internal/mcp/messages_tool.go is gone.
3. mcp__orchestra__get_run returns the new fields populated correctly on both
   backends.
4. mcp__orchestra__steer works end-to-end on MA; returns documented error on
   local.
5. mcp__orchestra__cancel_run works on both backends.
6. mcp__orchestra__get_artifacts and read_artifact work end-to-end.
7. Self-dogfood produces 2 PRs on real GitHub via injected credentials, no
   manual steps.
8. README and a brief migration-guide section updated for: agent rename, bus
   removal (with migration path send_message → steer + signal_completion
   artifacts), new artifact + credential model.

When done: cut a `v3.0-alpha` tag and report. Phase B starts after.
```

## END PROMPT
