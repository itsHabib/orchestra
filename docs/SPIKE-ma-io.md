# Spike: Managed Agents I/O model

Status: **Planned**
Owner: @itsHabib
Blocks: [DESIGN-v2.md](./DESIGN-v2.md) §9.6 (artifact flow), §5 D5, and implementation chapters P1.1.5/P1.4+. P1.1-P1.3 are spike-independent and may proceed in parallel.
Estimated effort: **1 day** (one operator with API access)
Last updated: 2026-04-17

> The v2 design depends on assumptions about how files flow in and out of Managed Agents sessions. Those assumptions aren't confirmed by the public MA docs. This spike answers them with concrete tests before we commit implementation effort.

---

## 1. Why this spike blocks design

Orchestra is a DAG orchestrator for multi-step agent workflows. Under the current `claude -p` backend, the agent operates directly on the user's filesystem — the repo IS the working directory, changes are immediately visible, and cross-team sharing is free because every team sees the same disk.

Under the new `managed_agents` backend, every session runs in an isolated cloud container. The MA docs explicitly state sessions do not share filesystems. The design doc (§9.6) sketches a flow using the Files API to move produced files between sessions, but that flow rests on three unverified assumptions:

1. Files the agent writes inside its container are automatically discoverable via `Beta.Files.List(scope_id=session_id)`.
2. The `agent_toolset_20260401` pre-built toolset includes whatever primitives the agent needs to produce and (if needed) publish deliverables.
3. The MA `github_repository` session resource is sufficient for repo checkout/update/PR workflows, or environments come with the tools orchestra needs to clone/push from a git repo if we fall back to a raw git-sync model.

If any of these is wrong, §9.6 needs to change — possibly significantly, including reopening the "no custom tools" non-goal.

The spike's job is to turn these assumptions into facts so the design is built on verified behavior, not optimistic reading of docs.

---

## 2. Questions this spike must answer

### Q1. File visibility: does `Files.List(scope_id=session_id)` return files the *agent wrote inside the container*?

Scenario: create an MA session, have the agent write `/workspace/out/foo.yaml` via the Bash tool, then call `Files.List(scope_id=session.id)`.

Possible answers:
- **(a)** Yes — the file appears with its own `file_id` and can be downloaded via `Files.Download`. §9.6 stands as written.
- **(b)** No — Files API only tracks files uploaded via `POST /v1/files` (client-side). Container writes are invisible to the Files API. §9.6 must change.
- **(c)** Partial — some paths are tracked (e.g. an `/output/` mount), others aren't. Needs a convention.

### Q2. Publishing pattern: if Q1 is (b) or (c), how does the agent get its output into the Files API?

Scenarios to test, in order of preference:
- **Bash + Anthropic CLI**: pass `ANTHROPIC_API_KEY` into the environment, have the agent run `anthropic files upload /workspace/out/foo.yaml` via Bash. No custom tool; just a built-in.
- **Bash + curl**: same as above but with `curl -X POST https://api.anthropic.com/v1/files ...`. Fallback if the Anthropic CLI isn't available in the container.
- **Built-in tool**: does `agent_toolset_20260401` include a `publish_file` or similar primitive? Check the tools docs.
- **Custom orchestra tool** (last resort): reopens the "no custom tools" non-goal. Only pursue if all above fail.

### Q3. Repo I/O: how does an existing codebase get into a session, and changes get out?

This is the broader question underneath Q1/Q2. For orchestra's real use case (agents working on actual repos), the "deliverable" is often a patch or branch push, not a single file.

Scenarios to test:
- **Native GitHub repository resource (preferred first)**: create the session with a `resources` entry `{type: github_repository, url, mount_path, authorization_token}`. Agent reads the mounted repo, changes a file, commits/pushes to a feature branch, and optionally opens a PR through GitHub MCP if available. Downstream team starts a second session with the same repo resource checked out to the branch and confirms the change is visible.
- **Raw git-sync fallback**: environment uses preinstalled `git` (or adds packages only if testing disproves that), GitHub credentials are supplied by the least-persistent safe mechanism, and the agent manually clones into `/workspace/`, commits, and pushes to a feature branch. Orchestra on the host polls for the branch / opens a PR. Downstream team clones the same branch instead of the original.
- **Files API bulk mount**: orchestra uploads every file in the repo as a resource at session start. Session-produced files come back via Q1's mechanism. Likely too slow / rate-limited for realistic repos — test the breaking point.
- **Hybrid**: small inputs as Files API resources; large codebases via `github_repository` resource or raw git-sync. Decide the cutoff and fallback rules.

### Q4. Environment capabilities

Less pivotal but needed to finalize config shape:
- Confirm the documented preinstalled `git` is present and usable; only require `packages.apt: [git]` if testing disproves the docs.
- Does the `agent_toolset_20260401` give the agent reliable git tool access, or does it go through Bash?
- Does `github_repository` resource mounting work for private repos, branch checkout, and downstream sessions without manual clone steps?
- Does GitHub MCP give enough write/PR capability for the default implementation, or should orchestra treat it as optional and rely on git CLI pushes?
- What's pre-installed? The overview page mentioned a "Container reference" — fetch and catalog.
- Can vaults actually hold a GitHub PAT for MCP auth? If not, use `authorization_token` on `github_repository` resources and document how orchestra sources it without writing it to `orchestra.yaml`.

---

## 3. Methodology

A single Go or Python script that exercises each scenario. Each test produces a pass/fail + a captured `Files.List` response (or equivalent) for the spike report.

Prerequisites:
- An Anthropic API key with MA access (beta header `managed-agents-2026-04-01`).
- Go SDK or Python SDK installed.
- A throwaway GitHub repo + PAT for the repo tests.

Test matrix:

| Test | Setup | Action | Verify |
| --- | --- | --- | --- |
| T1 | Fresh session, minimal env | Send `user.message`: "Write `/workspace/hello.txt` with the text 'hello'" | `Files.List(scope_id=session.id)`. Record response. |
| T2 | Same as T1 | After T1, `Files.Download` any returned file IDs | File content matches what the agent wrote. |
| T3 | Session with env containing `packages.apt: [anthropic-cli]` or similar; ANTHROPIC_API_KEY passed via env | "Write foo.yaml, then upload via the anthropic CLI" | File appears in `Files.List`. |
| T4 | Same session as T3 | Same test via `curl` instead of CLI | File appears in `Files.List`. |
| T5 | Fresh session with `github_repository` resource mounted at `/workspace/repo` | "Read a known file, make a change, commit and push to branch spike-test" | Branch appears on GitHub; raw session response does not echo the token. |
| T6 | New session with same `github_repository` resource checked out to branch spike-test | "Read the change from the branch" | Agent confirms the change. |
| T7 | Fresh env with raw git fallback + GitHub token, no repo resource | "Clone https://github.com/x/y, make a change, push to branch spike-raw-git" | Branch appears on GitHub; record whether extra packages were needed. |
| T8 | Moderate-size repo (~500 files) | Upload entire repo as Files API resources, create session with them mounted | Measure: latency to create, total bytes, 100-file cap behavior, and whether it works at all. |

Each test's raw request/response captured in `docs/spike-output/ma-io/<test-id>.json`.

---

## 4. Expected deliverables

When the spike completes, produce:

1. **`docs/SPIKE-ma-io-findings.md`** — one-page answers to Q1–Q4 with evidence (captured request/response snippets, references to specific MA doc sections if they clarified during testing).
2. **Proposed amendments to `DESIGN-v2.md` §9.6 and §5 D5** — written as a diff or a "changes required" section. No code yet; design amendments only.
3. **Recommendation on repo I/O model** — `github_repository` resource vs. raw git-sync vs. Files API vs. hybrid, with the cutoff criteria if hybrid and the exact `state.json` fields needed for branch/commit/PR artifacts.
4. **Updated §14 open questions** — close the ones resolved, open any new ones surfaced.

---

## 5. Out of scope

- Performance benchmarking beyond the one size-ceiling data point in T7.
- Cost measurement — covered separately in design doc §14.4.
- Multi-agent / thread behavior — orthogonal to I/O.
- Windows-specific environment behavior — MA containers are Linux.

---

## 6. Success criteria

Spike is done when all four deliverables in §4 are produced AND the design doc author (@itsHabib) has reviewed the findings and decided whether §9.6 stands, needs amendment, or needs a rewrite.

If Q1 answer is (c) "partial" or Q3 has surprises that invalidate both the `github_repository` resource model and the raw git-sync fallback, the spike may recommend a second round. That's expected — better to spend a second day spiking than build Phase 1 on bad assumptions.
