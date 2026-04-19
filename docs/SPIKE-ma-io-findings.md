# Spike findings: Managed Agents I/O model

Status: **Complete (Q1/Q2/Q7 resolved; Q3 resolved from docs)**
Date run: 2026-04-18
Harness: [`docs/spike-harness/`](./spike-harness/) against `anthropic-sdk-go v1.37.0`
Evidence: [`docs/spike-output/ma-io/`](./spike-output/ma-io/) — raw request/response JSON per test. Retained for verifiability; safe to delete at any point (the harness can regenerate it, and the findings in this doc stand on their own).

---

## TL;DR

The design's §9.6 artifact flow — "team A writes to `/workspace/out/`, orchestra lists files via `Files.List(scope_id=session_id)`, downloads them, remounts on team B" — **does not work.** The Files API has no session-scoped view of container-written files. The `scope_id` parameter in the SDK is not a valid query field on `GET /v1/files`. There is no `/v1/sessions/{id}/files` endpoint. The `session.resources` collection is input-only.

The correct artifact model is inverted: **artifacts flow via the session's `github_repository` resource, not the Files API.** Agents are told to commit + push to a feature branch; orchestra reads the resulting branch/commit SHA through the GitHub API and mounts the same repo on downstream sessions checked out to that branch. The Files API is still useful for small binary inputs, but it is not the primary artifact medium.

The originally-planned vault probe (Q7) turned out to be unnecessary: the platform docs settle it. **Vaults are MCP-auth-only** — every credential binds to an `mcp_server_url` and is injected at the Anthropic MCP gateway, never into the container. GitHub-push auth instead flows through `resources[].authorization_token` on the `github_repository` session resource (a raw PAT, write-only at the API layer, rotatable via `Sessions.Resources.Update`; GitHub repos are cached across sessions that reuse the same URL). See §Q7 below for quotes + sources. No open gates remain.

---

## Q1 — File visibility via `Files.List(scope_id=session_id)`

**Answer: (b) NO.** Container writes are invisible to the Files API.

**Evidence** — [t1/events-raw.json](./spike-output/ma-io/t1/events-raw.json):

1. Session `sesn_011CaC2GV6nzZVw66ZR3rNiw` ran `echo 'hello from spike T1' > /workspace/hello.txt` via the bash tool. Tool result: `(no output)` — the write succeeded. Session went to `idle` with `stop_reason: end_turn`.
2. `GET /v1/files?scope_id=sesn_011CaC2GV6nzZVw66ZR3rNiw` → `400 Bad Request` — *"unknown field scope_id"*. The SDK's `BetaFileListParams.ScopeID` sends this query param, but the server rejects it.
3. `GET /v1/files` with no filter → `{"data":[],"has_more":false}`. No files visible at the account level either.
4. `GET /v1/sessions/{id}/files` → `404 Not Found`. No session-scoped files endpoint exists.
5. `GET /v1/sessions/{id}/resources` → `{"data":[]}`. Resources are strictly **inputs** mounted at session creation; agent-produced files do not appear here.

Conclusion: files written by the agent inside the container stay in the container. There is no first-party "export" path exposed to the Files API.

---

## Q2 — How does the agent publish?

**Answer: neither currently-tested path works out of the box.** Both need a credential the container doesn't have.

**Evidence** — [t3/events-raw.json](./spike-output/ma-io/t3/events-raw.json), [t4/events-raw.json](./spike-output/ma-io/t4/events-raw.json), [probe/events-raw.json](./spike-output/ma-io/probe/events-raw.json):

1. `anthropic` CLI is **not preinstalled** in the default cloud container. T3: `which anthropic || echo "anthropic-cli not installed"` → `anthropic-cli not installed`. Installable via `pip install anthropic` (Python + pip are preinstalled), but only resolves half the problem.
2. `ANTHROPIC_API_KEY` is **not exposed** to the container env. T4: the agent wrote `/workspace/t4.txt`, then reported `STATUS: unset`. Environment-key probe showed `PATH, HOME, PWD, PYTHONUNBUFFERED, NVM_DIR, NODE_EXTRA_CA_CERTS, SSL_CERT_FILE, ...` — no Anthropic credentials, no GitHub credentials.

Without credentials, the agent cannot `curl` the Files API, cannot use the Anthropic CLI, and cannot `git push` anywhere. **The load-bearing question becomes: does `vault_ids` on session creation surface those credentials as env vars, or via a different mechanism?** The Claude UI exposes a credentials vault — that is the intended primitive. A one-off confirmation test is cheap and deferred to a follow-up session (task #8).

**Provisional answer assuming vaults work as advertised:** the agent publishes by running `curl` directly against the Files API (if an Anthropic key is vaulted) or — more commonly — by running `git push` (if a GitHub PAT is vaulted). The design should not require the `anthropic` CLI; `curl` is already present.

---

## Q3 — Repo I/O

**Not live-tested this pass** (requires a throwaway repo + PAT, not provided). But the shape of the answer is constrained by what we now know:

- **Container environment is rich.** [probe/events-raw.json](./spike-output/ma-io/probe/events-raw.json) shows `git 2.43.0` preinstalled at `/usr/bin/git`. Python, pip, node, npm, Java (via `JAVA_HOME`), Rust (via `RUSTUP_HOME`), Ruby (`RBENV_ROOT`), Playwright browsers (`PLAYWRIGHT_BROWSERS_PATH`), bun (`BUN_INSTALL`) are all present. No extra `packages.apt: [git]` is needed — the design can drop that line from §12.2.
- **/workspace is empty and writable.** `total 8` (just `.` and `..`), owned by root. Any repo mount ends up somewhere under `/workspace/`.
- **Networking must allow github.com** if the agent is to push. The spike env was created with `allow_package_managers: true` + `allowed_hosts: ["api.anthropic.com", "github.com", "objects.githubusercontent.com", "codeload.github.com"]` and both the package installs and would-be git operations had the network access they need.
- **The `github_repository` session resource** is not observable without actually using it, but given Q1's result, it is the only plausible primary artifact medium. The alternative — "Files API as artifact bus" — is dead.

The recommended repo I/O model pending follow-up verification:

1. **Default: `github_repository` resource.** Every team that produces code-shaped artifacts gets a `github_repository` resource mounted at `/workspace/repo`. Credentials come from a vault. Agent reads, changes, commits, pushes to a feature branch. Orchestra reads the branch name and commit SHA from the GitHub API (not the agent's final prose) and stores them as `repository_artifacts` in `state.json`. Downstream teams get the same resource checked out to the new branch.
2. **Small inputs: Files API.** The Files API remains useful for host-side uploads of small binary inputs (spec files, reference datasets) that don't belong in a repo. Mount them as `resources: [{type: "file", file_id: ...}]`.
3. **No fallback to raw git-sync as the default.** Only used if `github_repository` resources fail for some edge case surfaced in the vault follow-up.

---

## Q4 — Environment capabilities

**Catalogued** — see [probe/events-raw.json](./spike-output/ma-io/probe/events-raw.json).

- **OS:** Linux 4.4.0 (gVisor sandbox — kernel reports `runsc`). `IS_SANDBOX=...` env var confirms.
- **Shell, user:** bash, root.
- **Preinstalled:** git 2.43.0, python/python3, pip, node 22, npm, java, rust, ruby, playwright.
- **Not preinstalled:** anthropic CLI, gh CLI. `pip install anthropic` should work if needed, but **prefer `curl` in the prompt** — fewer moving parts, nothing to install.
- **TLS roots** surface via `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`. Consistent across Python/Node/requests.
- **Credential surface** (probe evidence): no secrets in the environment by name. Vaults (via `vault_ids`) are the documented injection path and must carry this load.

---

## Q7 — Vault credential mechanics

**Answer: Not applicable — vaults are MCP-auth-only.** No live probe needed; the platform docs resolve this.

**Evidence — [vaults docs](https://platform.claude.com/docs/en/managed-agents/vaults):**

> "Each credential binds to a single `mcp_server_url`. When the agent connects to an MCP server at session runtime, the API matches the server URL against active credentials on the referenced vault and injects the token."

Only two auth types exist, `mcp_oauth` and `static_bearer`, both with mandatory `mcp_server_url`. There is no env-var or filesystem mechanism; the token is injected at the Anthropic MCP gateway, outside the container.

**The actual `git push` credential path** is `resources[].authorization_token` on each `github_repository` resource. From the [GitHub docs](https://platform.claude.com/docs/en/managed-agents/github):

> "The `resources[].authorization_token` authenticates the repository clone operation and is not echoed in API responses."

Properties worth knowing:

- **Write-only at the API layer.** Never returned in responses.
- **Rotatable mid-session** via `POST /v1/sessions/{id}/resources/{resource_id}` — useful for long-lived sessions spanning PAT-rotation windows.
- **Repos are cached across sessions that reuse the same URL** → fan-out-heavy tiers start faster on warm caches.
- **Scopes:** `repo` for private repos or `public_repo` for public; same scope covers issue + PR creation if the agent drives them.
- **Two GitHub layers are independent.** The `github_repository` resource (filesystem clone + push) and the GitHub MCP server at `https://api.githubcopilot.com/mcp/` (PR-creation tools) are separate. Orchestra can use just the resource (for the default host-side PR-open path) or both (if agents open PRs themselves via the MCP).

**Consequential facts from adjacent docs:**

- **MCP toolsets default to `permission_policy: always_ask`** ([permission-policies docs](https://platform.claude.com/docs/en/managed-agents/permission-policies)). If orchestra ever opts into GitHub-MCP-driven PR creation, it must explicitly set `always_allow` on the `mcp_toolset` entry, otherwise every tool call parks the session at `stop_reason: requires_action` awaiting `user.tool_confirmation`.
- **Invalid MCP credentials don't fail session creation** — a `session.error` event is emitted and retries happen on the next `idle → running` transition ([mcp-connector docs](https://platform.claude.com/docs/en/managed-agents/mcp-connector)). Orchestra needs to watch for this event type in the engine loop.

**Design impact (folded in; see DESIGN-v2.md amendments):**

- §9.6 step 1: auth source flipped from `vault_ids` → `resources[].authorization_token`.
- §12.2: `vault_ids` dropped from required-under-`managed_agents` to optional; new `github_token` source priority (env → orchestra config → error) added.
- §13 P1.0.5: killed as a live-probe chapter; marked resolved-from-docs.
- §14 Q7: marked resolved.
- §14 Q8: clarified that MCP-path PR creation needs the `always_allow` override.

---

## Proposed amendments to DESIGN-v2.md

### §9.6 — Artifact flow (rewrite, not patch)

Replace the existing section with:

> Under the MA backend, cross-session artifact flow is mediated by **GitHub**, not the Files API. The MA Files API has no session-scoped view of files written inside a container (verified by spike T1/T3/T4; see `SPIKE-ma-io-findings.md` Q1).
>
> **Default flow (repo-backed deliverable):**
>
> 1. Each team is given a `github_repository` session resource pointing at the project's repo (or a scoped fork), mounted at `/workspace/repo`, checked out to the branch declared by the team's upstream dependencies (or `main` for tier-0 teams). Auth comes from a vault whose ID is declared in `backend.managed_agents.vault_ids`.
> 2. The team's prompt includes, as a standard suffix: *"Your working copy of the repo is at `/workspace/repo`. Commit your changes on a new branch named `orchestra/<team>-<run-id>` and push. Do not open a PR. Report the branch name and the final commit SHA in your last message."*
> 3. On `session.status_idle + stop_reason: end_turn`, orchestra resolves the branch head via the GitHub API (`GET /repos/{owner}/{repo}/branches/{branch}`), not by parsing the agent's prose. If the branch doesn't exist or has no new commits, the team is marked `failed` with `last_error: "no branch pushed"`.
> 4. Orchestra records `{url, branch, commit_sha, base_sha}` as a `repository_artifacts[]` entry on that team's state. Optionally opens a PR via the GitHub API (behind a config flag — not on by default).
> 5. Downstream teams that depend on A start sessions with a `github_repository` resource checked out to A's recorded branch. Artifacts from multiple upstreams become multiple mount points (`/workspace/upstream/<team-a>`, `/workspace/upstream/<team-b>`, ...).
>
> **Secondary flow (small inputs, no repo):** the host-side orchestrator uploads small input files via the Files API and attaches them as `resources: [{type: "file", file_id: ...}]` on the session at creation. Produced text summaries continue to be captured from the final `agent.message` and inlined into downstream prompts. This covers cases like "team A writes a one-page plan.md that team B should read" — no repo needed.
>
> **What's gone:** `ListProducedFiles` via `Beta.Files.List(scope_id=session_id)`, `DownloadFile` to `.orchestra/results/<team>/<filename>`, the max_artifact_mb knob. The MA backend does not download produced files to the host. Agent-produced state lives in git. If a team genuinely needs to publish a non-repo artifact (a rendered PDF, a compiled binary), it does so by **committing the artifact to the repo**, with the same branch/SHA tracking as code.

### §5 D5 — replace wording

> **D5** — **Artifacts flow through GitHub, not the Files API.** The orchestrator resolves branch heads via the GitHub API on team completion and records `repository_artifacts` in `state.json`. Downstream sessions mount the same `github_repository` resource checked out to the upstream's branch. The Files API is used only for host-side input uploads.

### §10.2 `state.json` schema

Remove the `artifacts[]` field. Keep `repository_artifacts[]`. Add `base_sha` alongside `commit_sha` for diff context:

```json
"repository_artifacts": [
  {
    "url": "https://github.com/org/repo",
    "branch": "orchestra/backend-20260417",
    "base_sha": "5b7e...",
    "commit_sha": "abc123...",
    "pull_request_url": null
  }
]
```

`max_artifact_mb`, `defaults.archive_keep.results` — can be dropped. `.orchestra/results/<team>/` stops being a file sink and becomes summary-only (`summary.md` from the last `agent.message`).

### §12.2 config schema

- Drop the comment *"git is preinstalled in current cloud containers; add only extra packages"* — replace with spike-confirmed statement: *"git 2.43.0 is preinstalled; do not list it in `packages.apt`."*
- Promote `vault_ids` from an optional field to effectively-required under `managed_agents`: update the example to show a vault holding a GitHub PAT, with a doc-comment note that without it repo pushes fail.
- Replace `max_artifact_mb` with nothing — no file download path exists to cap.

### §13 phase ordering

- P1.5 ("Files API integration") is **deleted**. Its work absorbs into a renamed P1.5 — *"Repo-backed artifact flow"* — which wires `github_repository` resources, branch-head resolution via GitHub API, and downstream session remount.
- **~~New P1.0.5 (prerequisite): vault behavior confirmation.~~** **Resolved from docs (see §Q7 above); no live probe needed.** The `github_repository.authorization_token` path is the design's default, not a fallback.

### §14 open questions

- **Q1 (Files.List semantics):** **resolved** — files.list does not see container writes. See this doc's §Q1.
- **Q2 (artifact size filter):** **resolved as moot** — there is no file-download path to cap.
- **Q3 (rate limits at scale):** still open; measurement deferred to P1.6 as before, but the numbers change — each team is now 1 create (env/agent cached) + 1 session + 1 GitHub API call, not 1 session + N file downloads.
- **Q4 (cost delta vs `claude -p`):** unchanged.
- **Q5 (prompt builder scope):** narrows — the "artifact publish" prompt section is now a fixed git-commit-and-push instruction, not a branching "upload via X" instruction. Simpler than the design assumed.
- **Q6 (repo artifact source of truth):** **resolved** — GitHub API is the source of truth, branch + commit SHA + base SHA.
- **~~New Q7 (vault mechanics):~~** **Resolved from docs — vaults are MCP-auth-only, every credential binds to an `mcp_server_url`, nothing surfaces inside the container.** `github_repository.authorization_token` is the actual git-push credential path. See §Q7 above.
- **New Q8 (PR creation):** who opens PRs — orchestra host-side via the GitHub API, or the agent itself? Recommended: orchestra, so PR creation is atomic with run completion and can be gated by a config flag.

---

## Recommended next steps

1. **~~Vault confirmation spike.~~** Resolved from docs — see §Q7 above. No live probe needed.
2. **Amend DESIGN-v2.md §9.6 / §5 D5 / §10.2 / §12.2 / §13 / §14** with the diffs above. This should be a single follow-up commit.
3. **Unblock P1.4.** With the pivoted §9.6, P1.4 becomes "StartSession + Events + Send + watchdog" with no Files API work. P1.5 ("Repo-backed artifact flow") is the new critical path.
4. **Clean up.** The spike env and agent are live on the account:
   - `env_01KxKxiy8wMM2DWTmY3cFctg`
   - `agent_011CaC22o3uPVJh9xyqrU8C3`
   Run `spike teardown` when the vault follow-up is done.

---

## Raw evidence index

| File | Purpose |
| --- | --- |
| [`spike-output/ma-io/state.json`](./spike-output/ma-io/state.json) | Harness persistent state (env, agent, last session) |
| [`spike-output/ma-io/setup/environment.json`](./spike-output/ma-io/setup/environment.json) | Env creation response |
| [`spike-output/ma-io/setup/agent.json`](./spike-output/ma-io/setup/agent.json) | Agent creation response |
| [`spike-output/ma-io/t1/`](./spike-output/ma-io/t1/) | Q1 evidence — container write + Files.List probes |
| [`spike-output/ma-io/t3/events-raw.json`](./spike-output/ma-io/t3/events-raw.json) | Q2 evidence — `anthropic` CLI absent |
| [`spike-output/ma-io/t4/events-raw.json`](./spike-output/ma-io/t4/events-raw.json) | Q2 evidence — `ANTHROPIC_API_KEY` absent |
| [`spike-output/ma-io/probe/events-raw.json`](./spike-output/ma-io/probe/events-raw.json) | Q4 evidence — full container capability cat |
