# Feedback: orchestra MCP server (dogfood run #1)

Notes captured live while driving an MA-backed dogfood run from a Claude Code session via `mcp__orchestra__*`. Two parallel teams (`ma-dispatch`, `recipes-cleanup`), both clone the repo and ship a PR with CI green + reviews requested. Opinionated. Where I'm critical, I name what I'd actually change.

Run under observation: `20260502T052411.419445000Z` (2026-05-02). Workspace `%LOCALAPPDATA%\orchestra\mcp-runs\<run_id>\` on Windows (`~/.local/share/orchestra/mcp-runs/<run_id>/` on Linux/macOS).

## TL;DR — what hurts

The headline finding from this run: **the MCP server has no liveness signal on the orchestra subprocess it spawns.** When the subprocess died at startup with a clear, actionable "no Anthropic API key" error, `get_run` continued reporting `status: running, teams: pending` indefinitely. I only noticed because the user spotted "0 sessions in claude" and I went to read the workspace log directly. Without that human nudge I would have polled forever.

This is the #1 fix. Everything else is sand on top of it.

1. **`get_run` lies when the subprocess is dead.** No liveness check on the registered pid. State.json was never updated past `pending` because orchestra exited *before* the run loop started. The MCP wrapper has no fallback (orchestra.log tail, registered-pid check, anything). **Concrete fix:** in `get_run`, if `state.json.run_id` is unset *and* the registered pid is no longer alive, surface `status: failed` with a `reason` field populated from the last lines of `orchestra.log`. Cheap. Single-digit lines of code.
2. **`mcp__orchestra__run` doesn't wait for early failure.** Returned `{"pid": 34316, "started_at": ...}` as if everything was fine, even though the subprocess crashed within ~50ms with a clear stderr message. **Fix:** wait ~1-2s after spawn, check pid still alive *and* state.json initialized; if either fails, return the failure synchronously instead of silently registering a phantom run.
3. **No pre-flight check for backend prerequisites.** `backend: managed_agents` requires `ANTHROPIC_API_KEY` in env *or* `api_key` in `%APPDATA%\orchestra\config.json`. The tool description doesn't say this. The tool handler doesn't check it. The schema doesn't enum the backend field. **Fix:** validate prereqs in the handler before spawning. Single round-trip to fail fast, clear error message. The error orchestra itself emits is *good* — it just gets eaten.
4. **No in-band coordinator steering for MA runs.** The very PR Team A is opening (route `send_message` by backend) is the thing the coordinator most needs *during this run*. Until that lands, MCP `send_message` rejects MA recipients. The coordinator surface is unusable for MA mid-flight steering today.
5. **`get_run` is a thin snapshot — no activity, no logs, no rendered prompt.** All I get is `status` + `teams[].status`. Can't see what the agent is actually doing, can't tell why a team is stuck `pending`, can't peek the rendered prompt that was sent into the session. For a 30+ minute multi-team run, this is too coarse. (See #1 — same root cause: the MCP layer doesn't read its own subprocess outputs.)
6. **No `dry_run` / prompt preview on `run`.** Inline DAG → temp YAML → spawn happens behind a wall. If a team prompt is typo'd or the role is wrong, I find out 10 minutes later when the agent goes off the rails. A preview mode that returns the rendered injection bundle without spawning would catch this in 2 seconds.
7. **No abort/cancel.** Documented as deferred to v2 — but the only escape hatch right now is `kill <pid>`, which leaves state.json in whatever shape the SIGKILL caught it in. At minimum: a `cancel` MCP tool that does the equivalent of `orchestra runs cancel <id>` (graceful) would unblock most "oops" cases without needing full per-backend abort semantics.
8. **No way to surface costs / token usage during a run.** Especially salient for MA where each team is paid by-the-second compute. The workspace presumably has it (NDJSON logs include cost events), but the MCP surface doesn't expose it. I am running with a blank meter.

## Strengths worth keeping

- **Inline DAG was the right call.** Skipping the YAML round-trip for a one-off DAG is a real productivity win. The schema mirrors `orchestra.yaml` cleanly (project_name + backend + teams[]), and the response gives back enough to start polling immediately (`run_id`, `workspace_dir`, `pid`, `started_at`).
- **Run identity is durable.** `run_id` survives across MCP sessions; the registry-based design (per the mcp-server.md design doc) means I can crash the chat and pick up monitoring in a fresh session. That's the right primitive.
- **`backend: managed_agents` flips a single string.** No agent IDs, no env IDs, no auth knobs in the MCP call. Whatever provisioning is happening, it's hidden behind one field. That's the level of abstraction the chat-side LLM should be looking at.

## Detailed observations

### Discovery / schema

- The `inline_dag` schema buries `deps` under `teams[].deps` with `type: ["null", "array"]`. Forgetting `deps` works (treated as no deps). Typo'ing a dep name fails *at run time* with a Kahn cycle/missing-node error — not at schema time. **Suggestion:** validate dep references in the tool handler before spawning; fail the `run` call inline with a structured error instead of a 10-second-old run with `status: failed`.
- Two `project_name` fields: top-level and inside `inline_dag`. The schema says top-level overrides the inline one. **Pick one.** Having both invites mistakes ("which one wins?"). The inline one is the natural home for an inline DAG; drop the top-level override unless someone has a real use case beyond curiosity.
- The `prompt` field is a single free-form string folded into `Task.summary`. There's no shape — no required sections, no interpolation. Embedding a multi-step plan with shell commands and step numbering works, but it's also what an agent would receive verbatim with no scaffolding. **Suggestion:** at minimum, document what *role* and *context* injection adds on top of `prompt` so the caller knows what the agent actually sees. Better: expose a `preview` resource that renders the final injection.

### Lifecycle / observability

- `get_run` during the `pending → running` transition is opaque. Both teams sat at `pending` immediately after `run` returned. Pending could mean: (a) MA env not yet created; (b) agent not yet provisioned; (c) session not yet opened; (d) waiting for a tier dependency. The MCP surface doesn't distinguish. **Suggestion:** add a `phase` field — `provisioning_env`, `provisioning_agent`, `awaiting_session`, `running`, `awaiting_signal`, etc. — so I can tell whether to be patient or to investigate.
- No per-team last-event timestamp. If a team has been "running" for 12 minutes, is that because it's mid-edit or because the session went silent? `get_run` doesn't say. **Suggestion:** add `last_event_at` per team to the RunView.
- No `messages` resource preview in `get_run`. There's `orchestra://runs/{id}/messages` per the design doc, but it's a separate read. **Suggestion:** include `messages_count` and `last_message_at` in `get_run` so the coordinator knows whether to fetch messages.

### Steering surface

- The instruction I was given on this very run included: *"do NOT call send_message — MA backend rejects it, that's what Team A is shipping."* Self-aware but jarring: **the MCP server's own steering tool is broken for the backend Orchestra is most pushing toward**. PR will fix it; for the doc reader, the gap is real today.
- `read_messages` is the inverse and works for both backends? The design doc says yes. I haven't exercised it this run because there's nothing to read yet. Worth a follow-up note once messages start flowing.
- No "interrupt" / "user.interrupt" tool. For MA this would be the natural in-band cancel. For local, P1.9-E (per DESIGN-v2.md) is still pending. **Suggestion:** ship the MA-only path now (`interrupt({run_id, team})` → `Sessions.UserInterrupt`); local can ship later. Don't wait for parity to ship usefulness.

### Errors / failure modes

- The `run` tool returns the workspace dir but doesn't tell you whether log files exist yet, or where the run's NDJSON activity log lives within that dir. **Suggestion:** include `activity_log_path` in the response so the user can `tail -f` it without guessing.
- If `mcp__orchestra__run` is called with an invalid backend (e.g. `backend: "managed-agents"` with a hyphen), what happens? Schema says it's a free string. Fails downstream, not at the tool boundary. **Suggestion:** enum the field at schema level.

### Coordinator UX (Claude-as-coordinator)

- Polling cadence is on me. `ScheduleWakeup` ~270s keeps the prompt cache warm but burns a wake-up cycle every ~5 min for a run that may take 20-40. **Suggestion:** push notifications. If MCP supports it (it does, via resource subscriptions), surface team-status-change events so the coordinator can sleep until something actually changes. Cuts wake-ups by ~90%.
- The instruction set the coordinator received from the user was rich (full team prompts + monitoring rules). The MCP server has zero awareness of the coordinator's intent — there's no "coordinator goal" stored alongside the run. If a different chat session picks up this run, it has no idea what the goal was. **Suggestion:** optional `coordinator_intent` field on `run` (free-form string), surfaced in `get_run`. Cheap; useful.

## Setup friction (host-side)

Before this run could go anywhere, the API key had to reach the orchestra subprocess. That subprocess is spawned by the orchestra MCP server, which is itself spawned by the Claude host (Claude Desktop, in this case). On Windows, Claude Desktop doesn't inherit a shell env. The fix is: edit each `orchestra` entry's `env` block in `~/.claude.json` (both top-level `mcpServers` and project-scoped `projects[*].mcpServers`) and set `ANTHROPIC_API_KEY`. Then full quit + relaunch.

What the chat-side LLM has to figure out without help:
- Which config file holds the orchestra MCP registration. (It's `~/.claude.json`, but `claude_desktop_config.json` is also a config file the user might think of first.)
- That env vars on MCP servers go in the per-server `env` block, not as a parent-shell export.
- That a graceful in-app reload (`/mcp restart` or similar) likely doesn't re-read parent env, so a full app restart is required.
- That if orchestra is registered both globally and per-project, *both* need the env.

The orchestra README / `orchestra mcp --help` should call this out. **Suggestion:** make `orchestra mcp` print a one-line warning at startup if the chosen `default_backend` (or any MA usage) is missing `ANTHROPIC_API_KEY` in the spawning env. That way the next time someone configures the MCP server, the failure is visible at the *MCP server's stderr* (which the host shows) rather than buried in a per-run log file inside `%LOCALAPPDATA%`.

Bonus failure mode the user actually hit: I tried to use the `Edit` tool to patch `~/.claude.json` and it failed twice with "file changed since last read" — Claude Code itself is touching that file constantly, racing every Edit. Fell back to a Node script that read+modified+atomic-renamed in one syscall sequence. **Suggestion to Claude Code:** quiet the `~/.claude.json` writes during a single agent turn so config edits aren't impossible. **Suggestion to Orchestra:** ship an `orchestra mcp set-env` subcommand that does this edit safely — it's a foot-gun otherwise.

## The bigger lesson: env-via-host is the wrong default for MA

Orchestra's MA spawner reads `ANTHROPIC_API_KEY` from `os.Getenv` first, then falls back to `~/.config/orchestra/config.json` (`%APPDATA%\orchestra\config.json` on Windows). The error message offers both paths. **The config-file fallback is the path that actually works.**

I burned ~30 minutes on the env path before giving up:
- `~/.claude.json` got the env block edited (twice — both top-level and project-scoped)
- Claude Desktop got "restarted" (turned out to be window-close, not real quit)
- The orchestra MCP server got force-killed; Claude Desktop respawned it lazily on next tool call
- The respawned MCP server *still* didn't have the env propagated to its spawn of the orchestra subprocess

I never proved exactly which link in that chain was dropping the env, and I'm not going to — because the moment I wrote `{"api_key": "..."}` to `%APPDATA%\orchestra\config.json`, **the very next `mcp__orchestra__run` call sailed past auth on first try**, no host involvement.

**Strong opinion:** for the MA backend specifically, the chat-side LLM should default to telling users "drop the key in `~/.config/orchestra/config.json`," not "set an env var." Env-via-host plumbing is a maze of:
- Multiple host config files (`~/.claude.json`, `claude_desktop_config.json`, etc.)
- Multiple registration scopes (top-level, project-scoped — both need the env if you don't know which one is active)
- Host restart semantics that vary by OS and aren't documented in any one place
- MCP child lifecycle (lazy respawn, system-tray persistence) that's invisible to the user

Each link is a place the env can vanish silently. The orchestra config.json is one local file with predictable semantics — read on every spawn, no host involved.

**Suggestion to orchestra:**
- Add a top-level note in the README: "For MA usage, the simplest setup is `orchestra config set-api-key` (or write `~/.config/orchestra/config.json` directly). Env vars work too but require host configuration that varies by Claude app."
- Consider shipping `orchestra config set-api-key` as a CLI subcommand. Cheap to write; eliminates the entire failure mode for the most common setup.
- Make the env path a documented advanced-user option, not the first thing the error message recommends.

## Attempt 2: "restart Claude Desktop" doesn't restart the MCP child

After the env edit landed in `~/.claude.json`, the user did what the host UI calls "restart" and re-fired the run. **Same failure: `no Anthropic API key`.** A `Get-Process orchestra` showed PID 55676 with a 33-minute uptime — the orchestra MCP server had not been respawned. On Windows, Claude Desktop's "restart" (or close-window) typically backgrounds the app to the system tray rather than terminating it, so MCP child processes survive.

This is a *user-perceives-success-but-nothing-changed* trap. The host UI doesn't surface "MCP child process uptime" anywhere, so the user has no way to verify their restart actually rotated the env.

**Suggestions:**
- Host (Claude Desktop): expose MCP server PID + uptime in the MCP debug surface, and provide an explicit "Restart MCP server *X*" action that actually tears down the child.
- Orchestra: `mcp__orchestra__run` could include a `mcp_server_started_at` field in its response so the chat-side LLM has a sanity check ("did the env-changing restart actually happen?").
- Orchestra: consider reading `~/.config/orchestra/config.json` AND env vars in the spawned child, with config.json as a stable fallback. Then a JSON edit (which the chat-side LLM can do without restart) is enough — no host restart needed at all. This was the right call rejected earlier in this session for hygiene reasons; in hindsight, the "edit one config file and don't worry about app restart semantics" path may be worth the tradeoff for the MA backend specifically.
- Document the failure mode somewhere visible: "If you restart and the run still fails the same way, your host probably didn't respawn the MCP server. Quit fully via system tray."

## Live failure-mode walkthrough (this run, attempt 1)

What I saw, in order:

1. Called `mcp__orchestra__run` with the inline DAG. Got back a clean `{pid: 34316, run_id: ..., started_at: ..., workspace_dir: ...}`. **No indication of failure.**
2. Called `mcp__orchestra__get_run` immediately. Got `status: running`, both teams `status: pending`. Looked normal.
3. Scheduled a wakeup at +270s. Started writing the feedback doc.
4. User pinged: "0 sessions idle in Claude." First sign something was wrong.
5. Re-checked `get_run` — *still* `status: running, pending, pending`. Nothing changed in 2 minutes.
6. Read the workspace orchestra.log directly: `✗ Orchestration failed: managed-agents spawner: no Anthropic API key`. Subprocess had been dead for 2+ minutes.
7. Verified pid 34316 was indeed gone (`tasklist /FI "PID eq 34316"` → no tasks).
8. State.json showed both teams `pending`, `run_id` matching, but never updated — orchestra exited before the team-state-machine ran.
9. `mcp-runs.json` (MCP-server-side registry) still has the run, no failed marker.
10. `run.lock.holder` still names pid 34316. Stale.

Time-to-detect from a human if I hadn't been told: **infinity**. I would have kept polling at 270s intervals indefinitely. ScheduleWakeup doesn't get tired.

The orchestra binary itself emitted a perfectly fine error message. It even told the user the two ways to fix it. **The MCP server discarded that signal.** Every layer above the orchestra core has to be designed assuming the core can fail at startup, and surface that failure.

## Attempt 3: billing_error treated as retriable; orchestra hides the actual error

After the config.json fix, both teams got past auth and into MA sessions. Both immediately failed with:

```json
{"error":{"message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.","type":"billing_error"},"retry_status":{"type":"exhausted"}}
```

The orchestra-level output for this was: `FAILED: retries exhausted`. That's it. To learn it was *billing*, I had to crack open `.orchestra/logs/<team>.ndjson` by hand. The MCP `get_run` surface returned `status: failed` with no `reason` field, no `last_error`, nothing.

**Two issues compounded:**

1. **`billing_error` (and other 4xx classes like `permission_denied`, `invalid_request_error`) is not retriable.** Retrying it is wasted budget — the response will be identical every time. Orchestra retried 4× per team before giving up. The retry policy needs a class filter that fails fast on non-retriable HTTP/API errors.

2. **The actual error message was rich and actionable** ("Your credit balance is too low. Please go to Plans & Billing.") — the MA layer surfaced it cleanly. **Orchestra discards this in its public output.** Every layer above the MA event log loses information: team `signal_status` only sees "FAILED", run-level top-level log only sees "tier 0: teams failed", MCP `get_run` only sees `status: failed`. The chat-side LLM has to know to read NDJSON files in a specific subdirectory to get the truth.

**Suggestions:**
- **Retry policy:** classify retriable vs. non-retriable errors. `billing_error`, `authentication_error`, `permission_denied`, `invalid_request_error` → fail fast. `rate_limit_error`, `server_error`, `timeout` → retry with backoff. The MA SDK responses include `error.type`; filter on that.
- **Error propagation:** when a team fails, capture `last_error` (the most recent non-retry error from the session log) and surface it on the team's status. `RunView.Teams[i].LastError = "billing_error: Your credit balance is too low..."`. Costs nothing; saves the chat-side LLM from spelunking through NDJSON.
- **Per-team log resource:** add `orchestra://runs/{id}/teams/{name}/log` (deferred in DESIGN-v2 / mcp-server.md but suddenly very motivated by this run). The NDJSON format is already perfect; just expose it.

The lesson: **the rich data is there, the MCP layer just doesn't carry it across.** Every gap of "I had to look in a file the host can't see" is a place where the chat-side LLM falls off the rails until a human fishes the log out.

## Attempt 4: cached agent IDs + key rotation = orchestra dies

After three billing_error retries spaced over ~7 minutes, the next run failed differently:

```
FAILED: ensure_agent: GET https://api.anthropic.com/v1/agents/agent_011CadEYvSUo4TSWnyYHkZTn?beta=true: 401 Unauthorized
{"type":"authentication_error","message":"Authentication failed"}
```

Both teams hit this at `ensure_agent` — orchestra's "look up cached agent ID before creating a new one" path. Diagnosis: the user appears to have rotated the API key while resolving the billing issue, which:
1. Invalidated the key in `~/.config/orchestra/config.json` (recoverable: rewrite with new key).
2. Made the previously-cached agent IDs (created by Attempt 3 with the old key) unreachable from the new key — even if the new key were valid, those agents may belong to the old workspace/scope.

Right now orchestra fails-hard on this path. **Suggestion:** in `internal/agents/`, when `ensure_agent` gets a 401 or 404, fall back to creating a fresh agent and update the cache. The whole point of the cache is to avoid re-creating unchanged agents — but a stale cache shouldn't be a dead end. Treat 401/404 as "cache invalid, recompute."

This is part of a broader pattern worth naming: **orchestra's MA cache layer is optimistic about external state.** Agents/envs/sessions all get IDs cached locally (`~/.config/orchestra/{agents,envs}.json`), and orchestra trusts them. When the remote API has rotated keys, deleted resources, or had its workspace boundary changed, the cache becomes a foot-gun. **Suggestion: add a `--no-cache` (or `--force-fresh`) flag to `mcp__orchestra__run` that skips the lookup and creates fresh resources.** It's one line of code, and a great escape hatch when caches are suspect.

## Attempt 5: key fixed, billing_error returns; the workspace-vs-key mismatch trap

After the user pointed out the actual key lived in `~/pers/.keys` (a JSON file with multiple service keys, e.g. `claude` and `polygon`) — not the older single-purpose `~/pers/.key` we'd been using — I rewrote `config.json` with the correct key (it was even missing the `s` of `sk-` prefix, fixed automatically). The next run cleared `ensure_agent` and reached `model_request_start`, then bounced again with `billing_error` — *7 hours after* the user added credits.

The bigger story across attempts 3-5: **the failure mode keeps shifting one layer deeper.** Each fix unblocks one barrier; the next call hits the next one.

| Attempt | Failure | Root cause |
|---------|---------|------------|
| 1, 2 | "no Anthropic API key" | env not propagated through Claude Desktop |
| 3 | `billing_error` retried | (a) zero credits + (b) orchestra retried non-retriable error |
| 4 | `401 authentication_error` on `ensure_agent` | (a) old key invalidated + (b) orchestra fails-hard on stale cached agent IDs |
| 5 | `billing_error` again | new key valid, but credits live on a *different* workspace from the one the key was issued in |

None of these failure modes were caused by orchestra. All of them were *amplified* by orchestra's MCP layer:
- Auth path documented in error text but not in tool description.
- Same auth failure surfaces as `pending` (attempts 1-2) or `failed: retries exhausted` (3-5), never as the actual underlying message.
- Retry policy treats permanent errors (billing, auth) as transient.
- Cached agent IDs become a hard dependency on external state.
- Workspace boundary on Anthropic console is invisible from inside orchestra; a key + credits in different workspaces look identical to a key with no credits at all.

**One thing I want to call out positively:** the per-team NDJSON logs are *excellent* — they captured `billing_error`, `authentication_error`, request IDs, span timings, model_usage. The data is there. It's just trapped in `.orchestra/logs/<team>.ndjson` files inside `%LOCALAPPDATA%`, invisible to the chat-side LLM via MCP. The fix isn't more instrumentation — it's exposing what's already there.

**Action item (top priority):** add `RunView.Teams[i].LastError` derived from the team's most recent `session.error` event. One field. Costs nothing. Eliminates ~all of the "I don't know what failed" UX in this session.

## Friction summary, ranked

| # | Friction | Impact | Cost to fix |
|---|----------|--------|-------------|
| 1 | `get_run` lies when subprocess dies | Coordinator polls forever; users blame Orchestra for nothing | Small — add liveness check + log tail |
| 2 | `run` returns OK on dead subprocess | Same; failures look like healthy runs for first ~2 min | Small — sleep+check after spawn |
| 3 | No backend pre-flight (API key, etc.) | "Pending forever" is the failure mode for missing config | Small — validate before spawn |
| 4 | MA `send_message` rejected | Blocks coordinator entirely for MA mid-flight | Already in flight (Team A) |
| 5 | No `cancel`/`interrupt` | "Oops" recovery requires `kill <pid>` | Small (MA path); medium (local path) |
| 6 | No prompt preview / dry-run | Bad prompts cost ~10 min before they show | Small |
| 7 | `get_run` too thin (no phase/last_event/messages_count) | Can't tell why team is stuck | Small (add fields) |
| 8 | No cost surface | Running with blank meter | Medium (already tracked, just not exposed) |
| 9 | No live activity stream | Polling waste | Medium (resource subscription) |
| 10 | Schema drift (`project_name` x2) | Confusion | Small |
| 11 | Dep validation late | 10s failure, no schema catch | Small |
| 12 | Backend not enum'd in schema | `managed-agents` (hyphen) typo would fail late | Small |

## Real run live observations (attempt 6, 2026-05-02)

After ~6 attempts and ~24 hours of setup/billing wrangling, a real run finally landed. Live notes:

- **MA sandbox tooling overhead.** Both teams immediately ran `gh repo clone` → `gh: command not found`, then ~2-3 minutes installing `gh` via apt + curl-piped keyring before any code work could start. Multiplied across teams in the same run. **Suggestion to whoever maintains the MA agent template:** preinstall `gh` (and the standard CI essentials — `make`, `go`, `git`, jq) so multi-team coding runs don't pay this tax per-team. Or expose a `provision_extras: ["gh", "go"]` field on the team config that sets up tools once via a cached layer instead of per-session apt-get.
- **`get_run` doesn't surface what the agent is doing right now.** I had to crack open NDJSON to learn that ma-dispatch was "168s since last event, last call was apt-get install" — that's the difference between "patiently waiting for a slow tool" and "stuck, intervene." Reinforces the top-priority feedback item: bubble at least the latest tool-use's command (or its name) onto the team status. Even just `last_tool: "bash"` + `last_event_at` would dramatically improve coordinator UX.
- **Token visibility on state.json IS exposed**, just not via MCP. State.json shows `input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens` per team, updated as events stream in. Plumb this through `RunView.Teams[i]` and the cost-blindness pain point disappears overnight.
- **Cache reuse looks healthy:** 13K/24K cache_read_input_tokens vs 6K-7K creation each — Anthropic prompt caching is doing its job and most of the system prompt + tool definitions are being cached across calls. Nice to confirm.
- **Polling cadence (~270s) feels right** for the long-tail of these runs. After the initial setup phase the teams should be deep in code work; checking every 4-5 min is the sweet spot.

**Tick @ 5.6 min:** both teams cleared environment setup, in code-work phase. ma-dispatch exploring `internal/mcp/runs.go` (output 4956t, cache_read 651K). recipes-cleanup just landed in `/repo` (output 3680t, cache_read 393K). Healthy. No signal yet.

**Tick @ 16 min:** both teams stuck on **`gh` authentication** in the MA sandbox. ma-dispatch made it to `git push -u origin feature/...` and ran `gh auth status` (presumably empty). recipes-cleanup is poking at `git credential fill` and `~/.config/gh/`. Both have been silent for ~3.7 min on the same blocker. ma-dispatch produced 17K output tokens (~2.6M cache_read) so real implementation work happened in-sandbox; it just can't get the branch off the box.

This is a categorical gap, not a recipe-prompt typo. **MA sandboxes start with no GitHub auth.** Orchestra (or the recipe layer) needs a way to inject credentials at session-start: either an env var in the agent context (`GITHUB_TOKEN`) that the agent then uses to `gh auth login --with-token`, or a pre-provisioning step that drops `~/.config/gh/hosts.yml` into the sandbox before user.message #1.

Also a behavioral gap on the agent side: after multiple failed `gh auth status` / `git push` / `git credential fill` calls in a row, the agent should `signal_completion(blocked, "no GitHub credentials in sandbox")` rather than spin further. The kickoff prompt didn't tell them when to give up; that responsibility shifts to either the agent template (in v3) or the orchestra-injected scaffold ("if step N requires credentials and you've tried 3 things to find them, signal blocked").

Adding these to the v3 design doc as open questions on credential propagation and agent auto-bail heuristics.

(Live notes will keep being appended below as the run progresses.)

## Real run completed: observations

Final attempt (2026-05-02 23:40 UTC, run_id `20260502T234041.097786600Z`) finally reached agent execution after 6 setup attempts. Both agents ran, neither shipped a PR, but enough happened to fill in the items previously deferred.

### Run-shape facts

| Metric | ma-dispatch | recipes-cleanup |
|---|---|---|
| Final status | `done` | `failed` |
| Wall time | 18m 57s | 36m 47s (hit 30m team-timeout + ~7m before orchestra reaped) |
| Output tokens | 18,925 | 11,860 |
| Cache read tokens | 3.33M | 2.48M |
| Cache creation tokens | 155K | 72K |
| Tier | BLOCKED (no GH push) | BLOCKED (timeout) |

### Observations the simulated runs couldn't show

1. **Provisioning latency:** ~5 seconds from `mcp__orchestra__run` returning to the first MA `session.status_running` event. Cheap. Provisioning is not the bottleneck.
2. **Time-to-real-work:** ~3 minutes per team installing `gh` via apt-get. **Largest avoidable overhead** — ships the kickoff prompt before code work even starts.
3. **Cache reuse is excellent.** ~3.3M cache_read vs 155K cache_create on ma-dispatch (>20× reuse ratio). Anthropic prompt caching is doing real work; system prompt + tool defs cached aggressively across the agent's many tool calls.
4. **Polling cadence:** the ~270s wakeup interval was about right for the long tail. Early on (provisioning + setup) it's overkill; later (deep code work) it caught real progress between ticks. A two-phase poll cadence (60-90s for first 5 min, 270s after) would be ideal — but that requires the chat-side LLM (or a smarter run state machine) to know when "first 5 min" is over.
5. **Coverage of get_run was the same problem at every tick:** I knew the team was alive (status=running, last_event_at recent), I knew rough token consumption, but I never knew **what the agent was currently doing** without reading NDJSON. The promised `LastTool` field would change everything.
6. **Result summary IS surfaced via state.json after `done`.** It's a rich field — ma-dispatch's was a full markdown report. **But MCP `get_run` doesn't return it.** That's the easiest 5-line patch in this whole feedback file: include `result_summary` in `RunView.Agents[i]`. The agent's whole done-message is sitting in state.json being ignored by the public surface.
7. **`failed` doesn't say why.** recipes-cleanup's `state.json.teams[X].last_error = "hard timeout after 30 minutes"` is the answer — but `mcp__orchestra__get_run` returned `status: failed` with zero context. Reinforces the top fix.
8. **`signal_completion(done)` despite blocked outcome was bad agent behavior, not orchestra's fault.** ma-dispatch did the implementation, hit a credentials blocker, and decided to ship `done` with a result_summary that explains the blocker. That's a reasonable judgment call but it pollutes the success surface. If the agent had signaled `blocked`, the summary table would have been clearer. **Recipe-layer agent templates need to be explicit about when to use `blocked`** vs `done`.
9. **Hard timeout mechanic works.** recipes-cleanup correctly got reaped at the 30-minute mark (configurable per-team). Without it, the agent would have spun forever. **But the timeout reason isn't reported clearly** — `last_error: "hard timeout after 30 minutes"` is in state.json but `signal_status` is empty and `status` is just `failed`.
10. **Inter-agent messaging was never exercised.** Both teams worked in parallel and independently; neither sent a `send_message` to the other. A future dogfood with deps + handoff would be needed to test that path. Note that for *this* run, send_message would have errored on the MA backend anyway — fixing that was Team A's whole job.

### What broke that we wouldn't have noticed without a real run

- **MA sandbox has no GH credentials.** Universal blocker for any "ship a PR" recipe. Documented in §12.1 of the v3 design doc.
- **MA sandbox doesn't preinstall `gh`.** Per-team apt-get tax on every dogfood-shaped run.
- **Agent didn't auto-bail on repeated `gh auth status` failing.** Wasted ~10 minutes of session time after the first auth failure was clearly indicative.
- **Run-level "failed" with no team breakdown in `get_run`.** Had to read state.json to know recipes-cleanup specifically timed out.

### Tier breakdown (per kickoff prompt's mapping)

- **ma-dispatch:** `BLOCKED` — implementation done, no PR. signal_completion was misleadingly `done` but the result_summary makes the actual state clear.
- **recipes-cleanup:** `BLOCKED` — never produced output worth saving; just timed out.
- **Run overall:** `BLOCKED` — neither PR landed; substantive work captured only in ma-dispatch's `result_summary`.

### Net token cost
~30,800 output tokens, ~5.8M cache_read tokens, plus ~37 minutes of MA compute time. For a run that produced zero shipped PRs, that's expensive feedback — but the feedback is the deliverable and the doc you're reading is the output worth keeping.

## If we ship just one thing

**Add `RunView.Teams[i].LastError` derived from the most recent `session.error` event in the team's NDJSON log.** Surface it in `get_run` and as part of the per-team object in `list_runs`. One field, ~20 lines of code, eliminates roughly 70% of the "what is happening / what failed" UX pain we hit in this session. Every layer of the failure stack (`pending` → `failed: retries exhausted` → root cause) collapses to "here's the last error message the API sent." Without this, the chat-side LLM has to know to `cat .orchestra/logs/<team>.ndjson | grep error` to debug — and on a hosted MCP setup, it usually can't see those files at all.

Second-priority pair, also small: liveness check on registered pid in `get_run` (#1 in TL;DR), and `~1-2s wait + state-init check` in the `run` handler (#2 in TL;DR). Together those three shifts move every failure surface from "polled forever in confusion" to "fail-fast with the actual reason." The cumulative cost is one afternoon. The leverage is enormous — every operator using the MCP surface benefits on every run.
