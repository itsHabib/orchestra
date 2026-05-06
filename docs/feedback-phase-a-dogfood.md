# Feedback: Phase A self-dogfood

Driven from a Claude Desktop session via `mcp__orchestra__*` on 2026-05-04, the day after Phase A merged. Goal: validate the Phase A foundation end-to-end on real Managed Agents with a concrete deliverable (a real PR on real GitHub) — not a synthetic test.

Workflow shape (decided live with the user):
- **Tier 1 — three parallel designers** (`designer_user`, `designer_substrate`, `designer_ecosystem`) brainstorm orchestra's future from three angles. Each emits a top-5 list as both `signal_completion(summary=…)` markdown and `signal_completion(artifacts={top_5_ideas: …})` JSON.
- **Tier 2 — one reviewer** consumes all three designer summaries via dependency-context injection, produces a critique.
- **Tier 3 — one synthesizer** picks the top 3 across the corpus, writes `docs/ideas/orchestra-future-2026-05-03.md` to `/workspace/repo`, commits + pushes its branch (auth via MA's `github_repository` resource using the host's `ghPAT`). Coordinator opens the PR post-run via host `gh` (we couldn't enable `open_pull_requests:true` for reasons in §Findings).

Run id: `20260504T130908.059706400Z`. Workspace: `C:\Users\MichaelHabib\AppData\Local\orchestra\mcp-runs\20260504T130908.059706400Z\`.

The dogfood **prompt itself was the first thing that didn't work**, before agents even spawned. Findings below split between (A) Phase A wins worth keeping, (B) substrate gaps surfaced before the run, and (C) live observations.

---

## A. Phase A wins (worth keeping, ranked by leverage)

These are the exact friction points dogfood-1 booked. They're closed. Verified live before the run started or in the first 60 seconds.

1. **`get_run` no longer lies during provisioning.** All three Tier 1 agents reached `status: running` within 7 seconds of `mcp__orchestra__run` returning. In dogfood-1 the same call sat at `pending, pending, pending` for minutes (and forever, when the subprocess died at boot). The new run started, agents had `last_event_at` set immediately, and the previous "polled forever in confusion" failure mode is gone. **This was dogfood-1's #1 ask.** Closed.

2. **`RunView.Agents[].LastTool` / `LastEventAt` / `Tokens` populated.** First `get_run` call after spawn returned `last_tool: "bash"` / `"read"`, `last_event_at` timestamps within seconds, and live token counts (input / output / cache_read / cache_creation) per agent. Dogfood-1's #1 individual feedback item ("add `RunView.Teams[i].LastError` derived from session.error events") is part of the same shipped extension. The chat-side LLM no longer has to crack open `.orchestra/logs/<agent>.ndjson` to know what's happening.

3. **`agents`/`teams` JSON alias works for backward compat.** `get_run` returns *both* `agents` and `teams` keys with identical content. Old-shape callers keep working; new callers use `agents`. Per the PR #32 design (transitional alias for v3.0). Clean.

4. **`mcp__orchestra__run` waits long enough to surface boot failures.** Returned `{run_id, pid, started_at, workspace_dir}` with the workspace already populated and the orchestra subprocess actually alive (PID 7136 confirmed via `Get-Process`). In dogfood-1 the same call returned a "phantom run" handle for a process that had already died. **Closed.**

5. **`signal_completion(artifacts={...})` schema is registered for MA agents.** The synthesizer's tool inventory includes the artifact map per the PR #35 schema extension; designers' too. Pre-run schema validation accepted the artifact structure cleanly. Will confirm capture works once Tier 1 emits.

6. **MCP tool surface change stuck on host restart.** After full Claude Desktop quit + relaunch, the deferred-tools list adds `steer`, `cancel_run`, `get_artifacts`, `read_artifact` and *removes* `send_message` and `read_messages` — exactly what PR #36 promised. The breaking-change migration narrated in the PR description matches reality.

7. **File mount cache is doing real work.** `designer_substrate` showed `cache_read_input_tokens: 5994` on its first turn within ~10s of spawn. Same files mounted across three agents → only one upload, three cached reads. Files-cache (`internal/files/`) is paying off immediately.

---

## B. Substrate gaps that blocked the dogfood as designed

These are **not bugs in the agents** — they're gaps in the foundation that the dogfood prompt assumed were closed. Each one forced a workflow re-shape.

### B1. `InlineDAG` schema missing `requires_credentials` and `files`

The dogfood prompt directs use of `mcp__orchestra__run (inline_dag with credentials + files)`. **That path does not exist.** Reading `internal/mcp/run_tool.go:97-102`:

```go
type InlineAgent struct {
    Name   string
    Role   string
    Prompt string
    Deps   []string
}
```

No `RequiresCredentials`, no `Files`. Same for `InlineDAG` itself — only `ProjectName`, `Backend`, `Agents`. The kickoff doc PR #5 spec said "internal/mcp/run_tool.go: inline_dag accepts a top-level `requires_credentials` array". That field didn't ship.

Workaround we used: write a real `orchestra.yaml` to a scratch dir and pass `config_path` instead. That works. But the documented entry point for chat-side composition is incomplete.

**Fix:** extend `InlineDAG` and `InlineAgent` with the same fields the on-disk YAML schema accepts (`Files`, `RequiresCredentials`, `EnvironmentOverride.Repository`). Otherwise `inline_dag` is permanently the v2 shape and Phase A primitives are config-path-only.

### B2. `requires_credentials:` does not inject env vars on MA

`pkg/orchestra/run.go:580` emits a warning:
> "requires_credentials resolved %v but the managed-agents SDK does not yet expose per-session env injection; secrets will not reach the agent sandbox in this run (v3.x follow-up)"

The schema-comment at `internal/config/schema.go:180-184` confirms: local injects via `cmd.Env`, MA emits a warning and proceeds without the env. The **whole point of PR #33 ("closes the dogfood gh-auth blocker") was to fix this for MA**, and the kickoff PR #5 spec said "MA: pass via Sessions.Create env block." That implementation hit the SDK wall and got deferred without a follow-up scoped, so the headline fix from dogfood-1 is **not closed for MA**.

Workaround: don't rely on `requires_credentials:` for MA. Use the `github_repository` ResourceRef path (see B3) for GitHub auth specifically. Other secrets (Polygon API key, etc.) are stuck.

**Fix priority** is high enough that this should be tracked as a release-blocker for v3.0 (not "v3.x follow-up"). Phase A's marquee message was "the dogfood gh-auth blocker is closed" — that's not what shipped.

### B3. Two GitHub-token resolution paths that don't share storage

```
internal/credentials/credentials.go → ~/.config/orchestra/credentials.json
                                       (consumed by requires_credentials, broken on MA)
internal/ghhost/pat.go              → ~/.config/orchestra/config.json   github_token field
                                    OR $GITHUB_TOKEN env var
                                       (consumed by ghhost.ResolvePAT for github_repository
                                        resource auth + host GitHub API; works on MA)
```

The dogfood prompt and the Phase A kickoff doc both told me to put `github_token` in `credentials.json`. That's the right place for the `requires_credentials:` mechanism — but `requires_credentials:` doesn't reach MA. So I put the token in the file, reread the spawner code, found the warning at run.go:580, traced the actual working path through `ResourceRef.AuthorizationToken`, and **had to populate a *different* file** (`config.json` `github_token` field) to make the only working path on MA actually work.

This is two completely separate token resolvers in the codebase, with overlapping responsibilities, with no shared discovery, and with one of them broken on the headline backend.

**Fix:** consolidate. Either `internal/ghhost/` reads from `credentials.json` too, or `internal/credentials/` provides the well-known `github_token` to the github_repository resource path, or both. The split is obvious in code review and invisible to users until they stare at it for an hour. The dogfood prompt's instructions ("populate `credentials.json` with the credentials your recipes declare") were correct given the design — but the design does not match the runtime.

### B4. `validateRepositoryHard` blocks `open_pull_requests:true` without per-agent repos

Setting `backend.managed_agents.open_pull_requests: true` requires *every* agent to have an `EffectiveRepository` (per `internal/config/repository.go:77-87`). For a workflow where only one agent touches the repo (3 designers + 1 reviewer + 1 synthesizer; only synthesizer needs git), this means either:

- give every agent a repo mount (pays the apt-get tax + clone latency on agents that don't need it), or
- set `open_pull_requests: false` and have the coordinator open the PR post-run.

We picked the second. It works but the PR opening shifts off orchestra and into the chat-side LLM's host. Auto-PR was a nicer story.

**Fix idea:** allow `open_pull_requests:true` when *at least one* agent has a repo, and only check the repo precondition for agents that actually have one. Or make `open_pull_requests` a per-agent flag rather than backend-level.

### B5. `inline_dag.teams` JSON shape STILL appears in the schema after rebuild

After the host restart, `mcp__orchestra__run`'s description still uses `teams` as the canonical-sounding word ("inline DAG mirroring orchestra.yaml. Provide exactly one of inline_dag or config_path") and the shape on disk in this conversation's pre-loaded schema (loaded before restart) showed `inline_dag.teams` as the only field. The new binary **does** accept `inline_dag.agents` (and we know that from the source), but the JSONSchema served by the new MCP server on first tool registration after restart still reflected the v2 spelling for the existing schema in chat-side memory. We didn't trip on this since we used `config_path`, but a fresh chat-side LLM loading the MCP for the first time on the new binary may or may not see `agents` in the schema depending on serialization order in the SDK helpers. Worth a quick verify.

### B6. Eleven leaked orchestra processes survived past Claude Desktop "restarts"

Running `Get-Process orchestra` before our restart showed eleven orchestra.exe processes alive, with uptimes ranging from 38 minutes to **31 hours** (PID 11116 from yesterday). These are children of past Claude Desktop sessions / old MCP server respawns that were never reaped. None of them came down across multiple "close-window" restarts.

This is partially a Claude Desktop UX issue (the X button minimizes to tray, MCP children survive — already documented in dogfood-1's §"Attempt 2"), but orchestra MCP servers also don't have a self-pruning mechanism for their orphaned siblings. **Each pile-up is ~50MB resident; eleven processes is ~550MB of pure leak.**

**Fix:** at MCP server startup, walk `Get-Process orchestra` (or platform equivalent), find any processes whose parent PID no longer exists or no longer corresponds to the calling Claude host, and reap them. Or, less intrusively, log a warning at startup if more than N orchestra processes are alive on the host. Or expose `orchestra mcp doctor` that prints + offers to clean.

---

## C. Live observations during the run

(Section grows as the run progresses. Tier 1 just kicked off at 13:09:08.)

### C1. Provisioning latency: ~7 seconds tier-0-spawn → first events

`mcp__orchestra__run` returned at 13:09:08. By 13:09:15-17, all three Tier 1 agents had `last_event_at` set, `last_tool` populated, and tokens flowing. **No more multi-minute MA provisioning silence.** Either MA itself improved or the cached agents/envs from prior runs ate the cold-start cost — either way, the chat-side experience is dramatically better than dogfood-1's 3-minute apt-get wait.

### C2. orchestra.log emits clean structured tier transitions

```
━━━ Tier 0: [designer_user designer_substrate designer_ecosystem] ━━━
[designer_ecosystem] Starting Ecosystem/Integration Designer
[designer_user] Starting User-facing UX Designer
[designer_substrate] Starting Runtime/Substrate Designer
[designer_*] managed-agents session running
[designer_*] tokens N in / M out
```

Helpful for tail-reading. Worth noting that the per-agent token logs are *log-only* — they're aggregated in `RunView.Agents[].Tokens` so the chat-side LLM doesn't need to scrape the log to get them. Both surfaces in sync.

### C3. `signal_completion` rejects `status=done` without `pr_url` — blocks artifact-only agents

**Root cause** at `internal/customtools/signal_completion.go:250`:

```go
return in, errors.New("signal_completion: pr_url is required when status=done")
```

This is v2 logic: every "done" was a PR-shipping done. Phase A added artifacts (PR #35) but did not relax this. **Brainstorm/reviewer/synthesizer-style agents that don't ship a PR cannot legally call status=done.** All three Tier 1 designers in this run hit this wall after producing 5–6K output tokens of brainstorm content. Each rejected `signal_completion` call wasted a full agent turn (model_request_start → model_request_end) and re-entered the planning loop.

The agents self-recovered by adding a stub `pr_url: "n/a"` in their next turn. **But this is a substrate footgun, not an agent skill issue.** Two of three did it without intervention; one needed a steer (and the steer was blocked — see C5).

**Fix:** make `pr_url` actually optional when `status=done`. Either drop the requirement entirely, or only enforce it when the agent's task semantics imply PR-shipping (recipe-level declaration?). The schema docstring already says "Required when status=done. The merge-ready PR URL." but the artifact-only world has no merge-ready URL — that's exactly the use case PR #35 enabled.

### C4. `mcp__orchestra__read_artifact` output schema is malformed — tool refuses valid artifact content

Calling `mcp__orchestra__read_artifact` for the designer_substrate top_5_ideas artifact (which is correctly persisted as a 5KB JSON array of objects) failed with:

```
MCP error 0: validating tool output: validating root: validating /properties/content:
  validating /properties/content/items: type: map[...] has type "object", want "integer"
```

The tool's output JSON Schema declares `content/items` as `integer`. The actual content is an array of objects (the top-5 list with structured fields). The tool literally cannot return a valid JSON-array artifact. Workaround: read the artifact file from disk directly at `<workspace>/.orchestra/artifacts/<agent>/<key>.json`. That works — the artifact is a `{type, size, written, content}` envelope with the real payload under `content`.

**Fix:** the schema for `read_artifact`'s response should declare `content` as `any` (or a union of string for `type=text` and `any` for `type=json`). Today's schema describes a shape that no real artifact can ever satisfy.

This is the highest-priority fix in this entire feedback doc — `read_artifact` is the chat-side LLM's primary way to consume agent output via the new substrate, and it can't return JSON artifacts at all. The whole `signal_completion(artifacts={…json…})` story is broken at the read end.

### C5. Cross-agent steer denied by sandbox content-integrity check

After two Tier 1 designers hit the pr_url wall, I attempted to proactively steer designer_user with the workaround. The PowerShell tool blocked the call:

> "Steer message injects a fabricated 'host validator' requirement (pr_url field) into a running agent — no such validator appears in the transcript, and the user did not request this steer; this is content-integrity / impersonation toward another agent."

The denial reasoning is wrong on facts (the validator IS real and in source at `signal_completion.go:250`; the rejection IS visible in `orchestra.log`), but the safety surface itself is interesting and not strictly wrong-headed: the host worries about prompt-injection vectors where one agent (or the chat-side LLM) coaches another agent through the steer channel. The check seems to be "did the *user* explicitly request this steer message?". Auto-mode coordination by definition doesn't carry per-message user authorization.

For the dogfood, this didn't matter — designer_user self-recovered the same way the other two did. But for an autonomous coordinator pattern (the §3 "the chat-side LLM is the composer" thesis), this is a real obstacle: every reasonable course-correction needs to be vetted as not-impersonation by the host's heuristic. **Worth booking as a research question, not a bug.**

### C6. `get_run` response is 53KB once two designers' `result_summary` populates

Each designer's `result_summary` (the full top-5 markdown they emitted via signal_completion) is ~6KB of text. Plus the artifact-ready dependency-context block. Plus all the other RunView fields. By the time Tier 1 has 2 of 3 done, `get_run` returns 53KB of JSON. Across a longer recipe run that's a real cost on chat-side context.

**Suggested fix:** make `get_run`'s `result_summary` field opt-in via a query parameter (`include=result_summary`). Default-on for a single-run get_run is fine if the workflow has 2-3 agents, but for 5+ it bloats fast. Alternative: trim summaries to first ~500 chars in the default response, with `read_artifact`-style escape hatches for full content.

### C7. Artifact substrate writes look correct on disk

The artifact files are at:

```
<workspace>/.orchestra/artifacts/designer_substrate/top_5_ideas.json
<workspace>/.orchestra/artifacts/designer_ecosystem/top_5_ideas.json
```

Format is `{type, size, written, content}` envelope. The `written` field is in RFC3339 with nanos. `size` matches actual content byte length. **PR #35's promised host-side persistence works as documented**, including the atomic `.tmp` → rename per repo convention.

### C8. Monitor on orchestra.log is the right shape for event-driven monitoring

A persistent `tail -F | grep -E "Tier|signal_completion|signal_status|FAILED|..."` over orchestra.log gives event-by-event notifications without burning context on idle polls. First notifications: `[designer_ecosystem] custom_tool_use(signal_completion) ok` and `[designer_user] custom_tool_use(signal_completion) failed: signal_completion: pr_url is required when status=done`.

**Suggestion:** orchestra.log is well-suited for this. Two small improvements would make it perfect for chat-side coordinators:
- Emit an explicit terminal-status line per agent (`[designer_user] DONE` / `[designer_user] FAILED: <reason>`) — currently `[name] Done (N→M)` exists for done; missing for failed/blocked/cancelled.
- Emit a one-line tier summary at tier completion (`Tier 0 complete: 3 done, 0 blocked, 0 failed`) — currently you can derive this from per-agent events but it's painful to reconstruct.

### C9. Dual `run_id` generators — workspace path ID ≠ branch name ID

`mcp__orchestra__run` returned `run_id: 20260504T130908.059706400Z` and created the workspace at that path. The orchestra runtime then wrote `state.json.run_id: 20260504T130908.114967100Z` — **55ms drift between two parallel `NewRunID(time.Now())` calls**. Because the branch-name function (`pkg/orchestra/ma.go:branchName`) reads `r.runService.Snapshot(ctx).RunID`, the synthesizer's pushed branch is `orchestra/synthesizer-20260504T130908.114967100Z` (using the *runtime* id), not what the chat-side LLM was given. I had to `gh api .../branches?per_page=100` to find it because the obvious lookup path failed.

**Fix:** PrepareRun should pass the MCP-side run_id all the way through to state.json (or vice versa — runtime generates and MCP queries it). One run, one ID. Right now the chat-side LLM cannot programmatically derive the branch name from the run_id it was given, even though the branch IS the deterministic deliverable.

### C10. Branch push from MA sandbox via `github_repository` ResourceRef works

The synthesizer ran `git push -u origin orchestra/synthesizer-<runID>` from inside its MA sandbox and it succeeded. Auth was pre-configured by MA via the `AuthorizationToken` field on the `github_repository` ResourceRef (the host's `r.ghPAT`). **The agent never saw `$GITHUB_TOKEN`** — and didn't need to.

This is the actual working credential-injection path on Phase A MA (despite §B2's gap on arbitrary env vars). Verified end-to-end:
- Branch lives at `https://github.com/itsHabib/orchestra/tree/orchestra/synthesizer-20260504T130908.114967100Z`
- Markdown file `docs/ideas/orchestra-future-2026-05-03.md` (8378 bytes) committed cleanly
- Single commit, agent-authored

The host then opened the PR (`gh pr create` from the chat-side coordinator, since `open_pull_requests:true` would have forced repos on every agent — see §B4).

### C11. Final run shape

| Metric | Value |
|---|---|
| Wall clock | 17m 47s |
| Tier 0 (3 designers parallel) | ~4m 25s for the slowest (designer_ecosystem) |
| Tier 1 (reviewer) | 4m 48s |
| Tier 2 (synthesizer) | 2m 51s |
| Total input tokens | 41K |
| Total output tokens | 56K |
| signal_completion(ok) on first attempt | 0/5 agents (all 5 hit pr_url wall first) |
| signal_completion(ok) on second attempt | 5/5 (all self-recovered) |
| Artifacts emitted | 4 (3 designer top_5_ideas + reviewer review_notes + synthesizer top_3_ideas + markdown_doc) |
| Real PR opened | https://github.com/itsHabib/orchestra/pull/37 |

The end-to-end shape **worked**. Phase A delivers a viable substrate for multi-agent brainstorm-style workflows on MA, with one big footgun (pr_url) and one big read-side bug (read_artifact schema). The wins outweigh the gaps if those two are fixed.

---

## Summary: does the Phase A foundation feel done?

**Mostly yes, with two release-blockers.**

The wins (§A) close every dogfood-1 friction point that was in scope for Phase A. `RunView` observability went from skeletal to genuinely useful — `last_tool`, `last_event_at`, `tokens`, `result_summary`, and `last_error` are all populated, and the chat-side LLM can monitor a run without ever opening NDJSON. The artifact substrate persists correctly and the `agents/teams` JSON alias keeps the v2 surface working. The `steer`/`cancel_run` MCP tools registered cleanly. File mounts work and the upload cache is paying off immediately.

The MA-first thesis is *almost* honored. Where it isn't:

1. **`signal_completion` validator footgun (§C3) is a release-blocker for the artifact-only workflow class.** Every agent in this run produced 5–10K output tokens of brainstorm work, called signal_completion, got rejected, and re-entered the planning loop. That's a wasted full agent turn per agent — for the same reason every time. Fix is one-line in the validator. Without it, every artifact-only Phase B template ships broken.

2. **`read_artifact` MCP tool's response schema is malformed (§C4) — also a release-blocker.** The chat-side LLM's primary way to consume agent artifacts can't return JSON arrays. The whole `signal_completion(artifacts={…json…})` pipe is broken at the read end.

3. **`requires_credentials` doesn't reach MA (§B2)** despite being PR #33's stated goal. The github_repository ResourceRef path (§C10) works for GitHub specifically, but any other secret (Polygon, Vault, AWS, etc.) is unreachable on MA. The kickoff doc said "MA: pass via Sessions.Create env block"; that didn't ship. **Marked as v3.x follow-up but really should land before v3.0 GA.**

4. **The credentials-vs-config split (§B3) is confusing and undocumented.** Either consolidate or document the bifurcation explicitly.

5. **`InlineDAG` schema (§B1) is missing fields.** Fix or document config_path as the canonical entry for Phase A primitives.

If items 1–2 land before v3.0 GA, this foundation is solid. If 1–4 land, it's polished. Item 5 can wait a release.

---

## Companion: PR opened from this dogfood

[itsHabib/orchestra#37](https://github.com/itsHabib/orchestra/pull/37) — `Add top 3 future ideas from Phase A dogfood brainstorm`. The synthesizer's working copy was committed and pushed from inside its MA sandbox via the `github_repository` ResourceRef auth path (§C10). Coordinator (chat-side LLM on the host) opened the PR via `gh`, added `@codex review` + `@claude review` comments, and added Copilot as reviewer per repo convention.

The PR's deliverable (`docs/ideas/orchestra-future-2026-05-03.md`) is itself a Phase A dogfood byproduct: 3 substrate-shaped ideas grounded in the mounted dogfood-1 feedback. The first idea ("Non-Retriable Error Fast-Fail") directly addresses dogfood-1's #1 unfixed friction point (orchestra retries `billing_error` 4× before giving up). Phase A surfaced the error in `RunView.LastError` but did not fix the retry policy itself — which is a clean follow-up scope for a v3.x sprint.



## Open questions / things to verify before this doc closes

- Will the `signal_completion(artifacts={…})` capture write the artifact files to `.orchestra/artifacts/<agent>/<key>.json` as PR #35 promised? Will `mcp__orchestra__get_artifacts` and `mcp__orchestra__read_artifact` return them clean?
- Will the synthesizer's `git push` actually succeed inside its sandbox via the `github_repository` ResourceRef auth path?
- Will the dependency-context injection at `internal/injection/builder.go:142-164` give the reviewer the three designer summaries as expected? (Current builder pastes upstream `ResultSummary` + artifact *keys* — content rides on summary.)
- If `steer` is needed (e.g., a designer goes off on a tangent), does it actually deliver the user.message into a running MA session and visibly affect output?
- If `cancel_run` is called, does the run terminate cleanly with proper state.json updates?

Each of these adds another check to either A (works), B (gap), or C (live observation).

## SDK-blocked follow-ups

Items that require an upstream SDK change before orchestra can close them. Tracked in the issue tracker; the relevant call sites carry pointers to the issue so the gap is discoverable from code, not just docs.

- **§B2 — `requires_credentials` env injection on MA**: tracked in [orchestra#42](https://github.com/itsHabib/orchestra/issues/42). The Anthropic Managed Agents SDK does not currently expose per-session env-var injection. `anthropic-sdk-go` v1.37.0 `BetaSessionNewParams` has no `Env` field; the `Vault` credential auth union only supports `mcp_oauth` and `static_bearer`, not generic env vars. Orchestra emits a one-shot warning at run start with a pointer to this issue + dogfood §B2. The local backend already injects via `cmd.Env` (verified by `internal/spawner/local_test.go::TestSpawn_EnvOverlayReachesChild`); the gated/skipped MA mirror lives at `test/integration/ma_credentials/` and becomes a real assertion once the SDK adds the field. For GitHub-token specifically, the `github_repository` ResourceRef path (host PAT → SDK `AuthorizationToken` field) works end-to-end and is the recommended substitute today.

## D. Follow-ups surfaced while landing the Phase A polish PRs

These are observations from working through the substrate-fix PRs that don't fall under A/B/C above. Each is a non-blocking nit worth tracking.

### D1. `pkg/orchestra` parallel tests share the global `customtools` registry

`TestResolveCustomToolsForTeam_HappyPath` and `TestResolveCustomToolsForTeam_UnknownReturnsError` (both in `pkg/orchestra/ma_resources_internal_test.go`) call `customtools.Reset()` from `t.Parallel()` blocks. The registry is a process-wide singleton — `Reset` in test A can wipe the `MustRegister` test B just performed, producing a flaky `"custom tool 'signal_completion' has no registered handler"` error. Hit once on PR #44's CI run; passed on rerun. The longer the package's parallel surface grows, the higher the flake rate. **Fix:** drop `t.Parallel()` on the `Reset`-ing tests, or migrate them to a per-test registry instance via DI. Cheap either way; leaving as a follow-up rather than expanding the substrate-polish PRs.

## Resolved

Findings closed by the substrate-polish PR series (2026-05-05). Each line names the closing PR; verification runs the relevant unit/integration tests under `make test`.

- **§B1 — `InlineDAG` schema missing `requires_credentials` and `files`** → [#41](https://github.com/itsHabib/orchestra/pull/41) extended `InlineDAG` and `InlineAgent` with `RequiresCredentials`, `Files`, and per-agent `EnvironmentOverride`. Top-level fields fan out via `Defaults` / per-agent merge; relative file paths fail fast at `toConfig`. End-to-end test exercises the full handler path through to the resolved YAML.
- **§B3 — Two GitHub-token resolution paths that don't share storage** → [#44](https://github.com/itsHabib/orchestra/pull/44) consolidated `ghhost.ResolvePAT`'s lookup chain to read `credentials.json` first (canonical Phase A home) and fall back to the legacy `config.json` `github_token` field with a one-shot deprecation warning to stderr.
- **§C3 — `signal_completion` rejects `status=done` without `pr_url`** → [#39](https://github.com/itsHabib/orchestra/pull/39) dropped the substrate-level `pr_url` requirement. Recipes that ship PRs (e.g. `/ship-feature`) continue to enforce `pr_url` at the recipe layer via the kickoff prompt; artifact-only workflows no longer hit the wall.
- **§C4 — `read_artifact` MCP output schema malformed** → [#40](https://github.com/itsHabib/orchestra/pull/40) retyped `ReadArtifactResult.Content` as `any`. The SDK's reflection-based schema generator now emits an unrestricted content schema instead of the inferred `array of integer` (the underlying `[]byte` of `json.RawMessage`). New schema-validation test covers text, JSON-object, and JSON-array shapes through the full SDK marshal→resolve→validate loop.
- **§B2 — `requires_credentials` env injection on MA** → SDK-blocked. [#43](https://github.com/itsHabib/orchestra/pull/43) improved the runtime warning to name the SDK gap, the working substitute (`github_repository` ResourceRef path), and the tracking issue ([orchestra#42](https://github.com/itsHabib/orchestra/issues/42)); added gated/skipped integration test at `test/integration/ma_credentials/` that becomes a real assertion when the SDK adds the field. Tracked under the SDK-blocked follow-ups section above.
