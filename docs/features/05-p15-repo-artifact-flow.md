# Feature: P1.5 — Repo-backed artifact flow

Status: **Proposed — deferred until after P1.6 text-only**
Owner: @itsHabib
Depends on: [p14-ma-session-lifecycle.md](./p14-ma-session-lifecycle.md) (shipped), [p13-registry-cache.md](./p13-registry-cache.md) (shipped), [06-p16-multi-team-text-only.md](./06-p16-multi-team-text-only.md) (proposed — lands first)
Relates to: [DESIGN-v2.md](../DESIGN-v2.md) §5 D5, §6, §9.6, §10.2, §13 phase P1.5, §14 Q8.
Target: multi-team DAG runs under `backend: managed_agents` where team A's deliverable is a branch push and team B reads it. Layered on top of the text-only multi-team DAG shipped by P1.6.

**Ordering note (2026-04-20).** DESIGN-v2 §13 sequenced P1.5 before P1.6. That ordering has been reversed: [06-p16-multi-team-text-only.md](./06-p16-multi-team-text-only.md) proves multi-team DAG under MA using text-only deliverables first. This chapter layers GitHub-backed artifact flow on top as additive capability for teams whose deliverable is code. Text-only teams keep using the summary path. DESIGN-v2 amendments to reflect the reordering are pending.

---

## 1. Overview

`p14-ma-session-lifecycle.md` landed a single-team MA fixture: start a session, stream events, persist a text summary. The spawner already knows how to attach a `github_repository` resource to a session (`pkg/spawner/managed_agents_session.go:919` — `toSessionResources`), but nothing in the engine constructs one, nothing passes a PAT, nothing resolves a pushed branch after the session ends, and nothing wires an upstream team's branch into a downstream team's checkout.

P1.5 closes that loop. It adds:

1. Config schema for `backend.managed_agents.repository` (`url`, `mount_path`) plus per-team overrides.
2. Host-side GitHub PAT sourcing (`GITHUB_TOKEN` env → `~/.config/orchestra/config.json` → actionable error), mirroring the existing `ANTHROPIC_API_KEY` pattern (`internal/machost/client.go:40`).
3. `cmd/run_ma.go` wiring that builds the team's `ResourceRef` list at `StartSession` time — repo URL + PAT + checkout derived from upstream dependency state.
4. Post-session branch resolution: on `session.status_idle` with `stop_reason: end_turn`, call `GET /repos/{owner}/{repo}/branches/{branch}` and record `repository_artifacts[]` in `state.json`. No prose parsing — GitHub is the source of truth.
5. Downstream teams mount every upstream's pushed branch via multiple `github_repository` resources at `/workspace/upstream/<team-name>/`.
6. Prompt builder extension: MA-backend teams get a deterministic commit/push suffix instructing them to push `orchestra/<team>-<run-id>`. Local-backend prompts are byte-identical.
7. Optional host-side PR creation behind `backend.managed_agents.open_pull_requests: true`. Default off.

No Files API calls. The spike settled that (`docs/SPIKE-ma-io-findings.md` §Q1). The Files API remains available for small host-side input uploads in future chapters; P1.5 does not need it.

---

## 2. Requirements

**Functional.**

- F1. `backend.managed_agents.repository` parses from YAML: `{url, mount_path}`. `mount_path` defaults to `/workspace/repo`. Missing under `managed_agents` is an error the first time a team without `environment_override.repository: none` tries to run. (See §5.5 for the per-team override.)
- F2. GitHub PAT resolution function `internal/ghhost.ResolvePAT()` returns the token, tried in order: `GITHUB_TOKEN` env var → `~/.config/orchestra/config.json` `github_token` field → error whose message names both sources. Never logged, never written to state.
- F3. `cmd/run_ma.go` constructs one `ResourceRef{Type: "github_repository"}` per tier-0 team with `Checkout: &RepoCheckout{Type: "branch", Name: "main"}` (or the repo's default branch; see Q3).
- F4. For non-tier-0 teams, one `ResourceRef` per upstream dependency: same repo URL, distinct `MountPath` of `/workspace/upstream/<upstream-team-name>/`, `Checkout.Name` = upstream's recorded pushed branch. In addition, the team gets its own working-copy mount at `mount_path` checked out to the repo default branch for its own branch work.
- F5. After a team's session reaches `session.status_idle` with `StopReason.Type == "end_turn"`, orchestra resolves `GET /repos/{owner}/{repo}/branches/orchestra/<team>-<run-id>` via `internal/ghhost.Client`. On 200, records a `RepositoryArtifact{URL, Branch, BaseSHA, CommitSHA, PullRequestURL: nil}` entry in `store.TeamState.RepositoryArtifacts`. On 404 or `CommitSHA == BaseSHA`, marks the team `failed` with `LastError: "no branch pushed"`.
- F6. Prompt builder adds an "artifact publish" section to MA-backend team leads only, naming the expected branch (`orchestra/<team>-<run-id>`) and the forbidden actions (no PR, no merge). `backend: local` prompts are unchanged. Gated by a new `Capabilities.ArtifactPublish` field populated by the caller.
- F7. When `backend.managed_agents.open_pull_requests: true`, orchestra opens a PR via `POST /repos/{owner}/{repo}/pulls` after the branch is recorded. On success, populates `RepositoryArtifact.PullRequestURL`. On failure, logs a warning but does not fail the team (the push is the actual deliverable; the PR is a convenience).
- F8. `store.TeamState` gains `RepositoryArtifacts []RepositoryArtifact` alongside the existing token/cost/status fields. Persisted in the same atomic-write flow.
- F9. `cmd/runs show <run-id>` renders repository artifacts in the per-team section (one row per artifact with branch + short-sha + optional PR URL). `cmd/runs ls` is unchanged.

**Non-functional.**

- NF1. **PAT is never persisted.** Lives in `os.Getenv` / in-memory `ResolvePAT()` return value / `ResourceRef.AuthorizationToken` only. Does not appear in `state.json`, archived runs, NDJSON logs, or CLI output. Log redaction test required.
- NF2. **GitHub API client is swappable for tests.** `ghhost.Client` is a concrete type taking a minimal `httpDoer` interface so unit tests hit `httptest.Server`, not github.com.
- NF3. **Rate limits are a non-concern.** Authenticated GitHub API gives 5000 requests/hour; the flow is one `GET branches/...` per completed team plus one optional `POST pulls` per completed team. A 50-team run is ~100 calls — 2% of budget.
- NF4. **Resume is idempotent.** Re-reading a branch after an orchestra crash re-records the same `RepositoryArtifact` (same `{branch, base_sha, commit_sha}`). A PR already open is detected via `GET /repos/{owner}/{repo}/pulls?head=<owner>:<branch>&state=open` before posting; existing PR's URL is recorded, no duplicate created.
- NF5. **No changes to the local backend.** `cmd/run.go` (the `claude -p` path) imports nothing from `ghhost`. The Capabilities struct in the prompt builder defaults to `ArtifactPublish: ""` for local, producing today's prompt output.
- NF6. **Token redaction in error messages.** A PAT accidentally included in a log line or panic trace is redacted before display. `ghhost.Client` wraps HTTP errors through a scrubber that replaces the header value with `***`.

---

## 3. Scope

### In-scope

- New package `internal/ghhost/` with `Client`, `ResolvePAT`, `ParseRepoURL`, `RepositoryArtifact`-construction helpers, `httpDoer` interface, scrubber.
- Config schema additions: `backend.managed_agents.repository`, `backend.managed_agents.open_pull_requests`, optional per-team `environment_override.repository`.
- `store.TeamState.RepositoryArtifacts` + `store.RepositoryArtifact` types (persisted).
- `store.RunState.RunID` participation — used to compose the branch name `orchestra/<team>-<run-id>`. (The field already exists from p14.)
- `cmd/run_ma.go`: build `ResourceRef` list per team from config + upstream artifacts; call `ghhost.ResolveBranch` after `end_turn`; call `ghhost.OpenPullRequest` when flag is on.
- `internal/injection/builder.go`: `Capabilities.ArtifactPublish` field, new prompt section, threaded through `cmd/run_ma.go`.
- `docs/features/p14-ma-session-lifecycle.md` deviation note updated: branch handling moved here.
- An `examples/ma_repo_relay/` fixture: two teams, tier-0 team edits `README.md`, pushes branch; tier-1 team reads the branch, appends a line, pushes its own branch.
- Integration test (opt-in, hits real GitHub): mirror the fixture, runs against a scratch repo declared by env vars. Skipped unless `ORCHESTRA_TEST_REPO_URL` and `ORCHESTRA_TEST_REPO_PAT` are set.

### Out-of-scope

- **Files API usage.** Spike-resolved: no first-class artifact view. Small host-side uploads for textual inputs remain possible via `ResourceRef{Type: "file"}` but nothing in P1.5 exercises that path.
- **Multi-repo runs.** `backend.managed_agents.repository` is a single repo. Per-team override can point elsewhere, but cross-repo dependency resolution (team A → repo X, team B pulls from repo X) is a stretch case; defer until a fixture demands it.
- **Non-GitHub hosts.** Managed Agents supports `github_repository` specifically; the SDK has no generic git resource. Non-GitHub support would mean an entirely different substrate, not a variation of this design.
- **Per-commit PR updates.** Orchestra pushes one branch per team, opens a PR once. If the team re-runs (via resume after a failure), the existing PR stays open and its URL re-records.
- **PR merge policy.** Orchestra opens the PR; it does not merge, approve, or set auto-merge. That's a human-facing operational decision.
- **Agent-side MCP-driven PR creation.** DESIGN-v2 §9.6 lists it as an opt-in path. Out of scope for P1.5 — the host-side path is simpler and covers the stated need.
- **Skipping Git history between upstream and downstream.** Downstream teams mount the upstream's branch; they do not see a merge preview or diff. If we need a "just the diff" view later, it's a separate feature.
- **`orchestra runs rm --remote`.** Deleting remote branches post-run is not P1.5's job. If the user wants branch GC, it's a separate command.

---

## 4. Data model / API

### 4.1 New package `internal/ghhost`

```go
// Package ghhost is the host-side GitHub API client. Used by the engine
// to resolve pushed branches after MA sessions end, and optionally to
// open pull requests. Not exposed to agent containers.
package ghhost

import (
    "context"
    "net/http"
)

// Client is the orchestra-internal GitHub API client. Narrow by design:
// only the endpoints P1.5 needs.
type Client struct {
    http  httpDoer
    token string
    base  string // "https://api.github.com" by default; overridden for tests
}

type httpDoer interface {
    Do(*http.Request) (*http.Response, error)
}

// New constructs a Client. token must be a personal access token with
// repo (private) or public_repo (public) scope.
func New(token string, opts ...Option) *Client

// ResolvePAT returns the GitHub PAT. Tries GITHUB_TOKEN env var, then
// ~/.config/orchestra/config.json `github_token`. Never logged.
func ResolvePAT() (string, error)

// ParseRepoURL splits a GitHub URL into owner and repo. Accepts the
// canonical https URL form; rejects ssh URLs and forks with extra path
// components.
func ParseRepoURL(url string) (owner, repo string, err error)

// GetBranch fetches a branch. Returns ErrBranchNotFound on 404.
func (c *Client) GetBranch(ctx context.Context, owner, repo, branch string) (*Branch, error)

// OpenPullRequest opens a PR. Returns ErrPullRequestExists with the
// existing URL when the head:base pair already has an open PR.
func (c *Client) OpenPullRequest(ctx context.Context, req OpenPRRequest) (*PullRequest, error)

type Branch struct {
    Name      string // "orchestra/team-a-20260420-..."
    CommitSHA string // branch head
    BaseSHA   string // the merge-base against the repo default branch, via GET /repos/.../compare/...
}

type OpenPRRequest struct {
    Owner, Repo    string
    Head, Base     string // "orchestra/team-a-...", "main"
    Title, Body    string
}

type PullRequest struct {
    URL    string
    Number int
}
```

### 4.2 Config schema additions

```go
// In internal/config/schema.go.

type Backend struct {
    Kind          string                `yaml:"kind,omitempty"`
    ManagedAgents *ManagedAgentsBackend `yaml:"managed_agents,omitempty"`
}

type ManagedAgentsBackend struct {
    Repository       *RepositorySpec `yaml:"repository,omitempty"`
    OpenPullRequests bool            `yaml:"open_pull_requests,omitempty"`
}

type RepositorySpec struct {
    URL       string `yaml:"url"`
    MountPath string `yaml:"mount_path,omitempty"` // defaults to /workspace/repo
}

// Team override.
type EnvironmentOverride struct {
    Repository *RepositorySpec `yaml:"repository,omitempty"`
}
```

`Validate()` additions:

- Under `backend: managed_agents`, if any team has no inherited `repository` and `open_pull_requests` is true, error: "PR opening requires a repository."
- Under `backend: managed_agents`, if `repository.URL` is set but doesn't parse via `ghhost.ParseRepoURL`, error.
- Warning (not error): if any team's `depends_on` names a team whose own repository override differs — cross-repo upstreaming is out of scope for P1.5.

### 4.3 Store types

```go
// In pkg/store/run_state.go.

type TeamState struct {
    // ... existing fields ...
    RepositoryArtifacts []RepositoryArtifact `json:"repository_artifacts,omitempty"`
}

type RepositoryArtifact struct {
    URL            string `json:"url"`
    Branch         string `json:"branch"`
    BaseSHA        string `json:"base_sha"`
    CommitSHA      string `json:"commit_sha"`
    PullRequestURL string `json:"pull_request_url,omitempty"`
    ResolvedAt     time.Time `json:"resolved_at"`
}
```

State writes go through the existing `store.UpdateTeamState(team, mutator)` funnel. No new mutex, no new atomic-write path.

### 4.4 Prompt builder

```go
// In internal/injection/builder.go.

type Capabilities struct {
    HasFileBus      bool
    HasMembers      bool
    ArtifactPublish *ArtifactPublishSpec // nil == none
}

type ArtifactPublishSpec struct {
    MountPath   string // "/workspace/repo"
    BranchName  string // "orchestra/<team>-<run-id>"
    UpstreamMounts []UpstreamMount // empty for tier-0
}

type UpstreamMount struct {
    TeamName  string // "planner"
    MountPath string // "/workspace/upstream/planner"
    Branch    string // "orchestra/planner-<run-id>"
}
```

Local-backend callers pass `Capabilities{HasFileBus: true, HasMembers: t.HasMembers(), ArtifactPublish: nil}`. MA-backend callers pass `{false, false, &ArtifactPublishSpec{...}}`.

The prompt gains one new section, rendered only when `ArtifactPublish != nil`:

```
## Artifact delivery

Your working copy of the repo is at {MountPath}.

When your task is complete:
  1. Commit your changes on a new branch named {BranchName}.
  2. Push the branch to origin.
  3. Do NOT open a pull request; do NOT merge.

{if UpstreamMounts}
Your upstream dependencies are mounted read-only at:
  - {Team}: {MountPath}  (branch {Branch})
  ...
{end}
```

(Actual rendering in Go; pseudocode above.)

---

## 5. Engineering decisions

### 5.1 Branch name is deterministic from `{team, run_id}`

Name format: `orchestra/<team-name>-<run-id>`. Deterministic from state orchestra already has. Consequences:

- Branch resolution is a single GET, not a scan or list.
- Collision with a human-created branch of the same name is very unlikely (run IDs include a UTC timestamp + random suffix; see `internal/run/service.go` generator). If it happens, the symptom is the team "appearing to succeed" on prior content. Acceptable — treat as a user error, not a design flaw.
- Agents are told the name in the prompt and must obey it. Agents that push to a different branch are marked `failed: no branch pushed`. No fuzzy match, no "nearby branch" detection.

Alternative considered: derive name from the commit SHA only (no branch) and look up via `GET /repos/.../commits?author=<bot>&since=<run-start>`. Rejected — depends on agent identity, flaky with force-pushes, and provides no preview surface for humans.

### 5.2 Branch resolution is post-hoc, not parsed from prose

The agent writes prose describing what it did. That prose is unreliable (agents hallucinate commits, mis-state SHAs). GitHub's branch endpoint is authoritative. Orchestra parses **zero** agent text for branch/SHA information.

The prompt instruction "report the branch name and final commit SHA in your last message" exists in DESIGN-v2 §9.6 — keep it as a human-readable courtesy but do not consume it. The event translator records the prose summary in `TeamState.ResultSummary`; the artifact structure comes from GitHub.

### 5.3 Host-side PR creation, not agent-side

Two paths (DESIGN-v2 §9.6): orchestra calls the GitHub API, or the agent uses the GitHub MCP. P1.5 implements only the first. Reasons:

1. Atomic with run completion — PR opens exactly when orchestra decides to open it, not when the agent guesses it's done.
2. No agent permission grant needed. The agent only pushes (via the `authorization_token` on the `github_repository` resource, which is write-only). No vault, no MCP, no `permission_policy: always_allow` for PR-creation tools.
3. Simpler resume: on re-run, orchestra queries `GET /repos/.../pulls?head=...&state=open` and records the existing URL instead of trying to detect "did the agent re-open?"

Agent-side via MCP remains possible in a future chapter; it is not a critical path.

### 5.4 Multi-upstream mounts = multiple `ResourceRef`

MA accepts an array of session resources. A team with two upstreams (A, B) gets three `github_repository` entries:

| MountPath | Branch |
|---|---|
| `/workspace/repo` | repo default branch (team's own working copy) |
| `/workspace/upstream/a` | A's pushed branch |
| `/workspace/upstream/b` | B's pushed branch |

Each `ResourceRef` carries the same PAT. The container gets three checkouts; the team's working copy is the one it's expected to push from. Alternative considered: a single repo mount that the session's agent `git fetch`es upstream branches into. Rejected — requires the agent to understand fan-in geometry; multiple mounts make it declarative.

**Open:** does MA allow multiple `github_repository` resources backed by the same URL? Spike didn't test fan-in; confirm during implementation. If it errors, the fallback is git-submodule-style or a single mount + `git fetch` at session start.

### 5.5 Per-team `environment_override.repository`

Most teams share the project-level repo. Some teams (e.g. a docs team working on a different repo) need to override. The override field is a full `RepositorySpec`; it replaces the project-level value for that team, not a shallow merge. Same PAT resolution — one `GITHUB_TOKEN` for the orchestra process, used for all repos it touches.

Cross-repo dependencies (team in repo X depends on team in repo Y) are explicitly out of scope (§3). If the override names a different repo, orchestra treats the team as self-contained — its upstream's branch in repo Y is not mounted. The validator emits a warning in this case.

### 5.6 `RepositoryArtifacts` is a slice, not a singleton

Today a team pushes one branch per run. Future cases (multiple artifacts in one session, or resume-then-retry creating a fresh branch) argue for a slice. Slice cost is tiny; singleton would force a schema change later.

### 5.7 GitHub client is internal

`internal/ghhost/`. Consistent with the repo-wide policy (see [02-pkg-to-internal.md](./02-pkg-to-internal.md)): everything lives under `internal/` until SDK extraction is real work.

### 5.8 PAT in `ResourceRef` is passed through; lifetime equals StartSession

`ResourceRef.AuthorizationToken` is populated at `cmd/run_ma.go:runTeamMA` immediately before calling `StartSession`. It is not stored in any long-lived struct on the orchestra side, not passed through the run-lifecycle service, not written to state. MA stores it server-side (rotation via `Sessions.Resources.Update` is available but not used in P1.5).

---

## 6. Trade-offs

| Decision | Alternatives | Why this one |
|---|---|---|
| Deterministic branch name from `{team, run_id}` | (a) Agent-chosen branch name reported in prose. (b) Orchestra passes the name via env var and expects exact match. | (a) is what cortex tried with unreliable results. (b) is what we do; passing via env var is a future convenience but adds no value before then. |
| Branch head via GitHub API, never prose | Parse the SHA out of the final agent.message. | Agents hallucinate SHAs. Authoritative source is GitHub. |
| Host-side PR opening | Agent opens PR via GitHub MCP. | No MCP permission grant, no vault, no `always_allow` override, atomic with orchestra's completion signal. |
| Multiple `github_repository` resources for fan-in | Single mount + agent `git fetch`. | Declarative; the agent doesn't need to know about fan-in topology. |
| PAT lifetime = session | Store PAT in state, refresh per team. | State persistence is a leak vector. Resolving fresh each run is cheap. |
| `RepositoryArtifacts []` not singleton | Singleton now, migrate later. | Schema change later is more expensive than slice cost now. |
| `internal/ghhost/` not `pkg/github/` | Public library surface now. | No external consumer; narrow domain-specific client; matches "services/glue internal, SDK primitives public" convention. |
| Single-repo per project by default | Multi-repo first-class. | Most runs have one repo. Override exists for exceptions. Cross-repo dependency resolution is a stretch case deferred until real demand. |
| `open_pull_requests` is a project-level flag | Per-team PR policy. | Policy-at-team granularity has no real user. Project-level toggle is sufficient. |
| Strict branch name match ("no branch pushed" on mismatch) | Fuzzy-match on `orchestra/<team>-*`. | Fuzzy matching rewards agents for disregarding instructions. Fail loud. |

---

## 7. Testing

### 7.1 Unit tests — `internal/ghhost/`

All tests use `httptest.Server`.

- `ResolvePAT`: env set → returns env; env unset, config present → returns config; neither → actionable error.
- `ParseRepoURL`: canonical https → `(owner, repo, nil)`; ssh URL → error; path-only URL → error; trailing `.git` stripped.
- `GetBranch`: 200 with body → populated `Branch`; 404 → `ErrBranchNotFound`; 401 → error with scrubbed token.
- `OpenPullRequest`: 201 → populated `PullRequest`; 422 with "a pull request already exists" → detect, fetch existing, return `ErrPullRequestExists` with URL.
- Token scrubber: inject a PAT into an error path; assert it doesn't appear in the returned error's `.Error()` or in the logged request.

### 7.2 Unit tests — config parse + validate

- YAML with `backend: managed_agents` + `repository: {url: ..., mount_path: ...}` → parses, defaults applied.
- Missing `repository` under MA with any team + `open_pull_requests: true` → validation error.
- `environment_override.repository` on a team → shadow-replaces project value for that team only.
- Cross-repo `depends_on` warning fires.

### 7.3 Unit tests — `cmd/run_ma.go` wiring

- Tier-0 team: `ResourceRef` list has exactly one entry, mount `/workspace/repo`, branch `main`.
- Tier-1 team with two upstreams: three entries, correct mount paths, correct branches from `TeamState.RepositoryArtifacts`.
- Post-session resolve on `end_turn`: fake `ghhost.Client` returns a branch → `RepositoryArtifact` appended, team marked `done`.
- Post-session resolve on `end_turn` + `ErrBranchNotFound`: team marked `failed` with expected message.
- `open_pull_requests: true` + happy path: PR URL populated.
- `open_pull_requests: true` + PR already exists: URL populated, no duplicate call.

### 7.4 Integration test — `test/integration/ma_repo_relay/`

Two teams. Opt-in via env vars.

- Team A prompt: "Edit README.md, commit, push branch `orchestra/a-<run-id>`."
- Team B (depends_on A) prompt: "Read `/workspace/upstream/a/README.md`, append a line, commit, push branch `orchestra/b-<run-id>`."
- Assert: both teams `done`; `state.json` has both `RepositoryArtifacts` with distinct SHAs; team B's commit content includes team A's change.
- Cleanup: delete both branches via GitHub API on test teardown.

### 7.5 Prompt golden test

- MA-backend rendering with fake `ArtifactPublishSpec{MountPath, BranchName, UpstreamMounts}`: byte-compared against a golden file.
- Local-backend rendering (existing): byte-identical to today's golden.

### 7.6 What we explicitly do not test

- Real Managed Agents behavior with multiple `github_repository` resources per session. That is the spike-adjacent open question (§5.4). If MA rejects fan-in at the API level, the integration test will surface it; `go test -short` skips the integration test.
- GitHub API rate-limiting. 5000/hr ceiling; we will not exercise it in test.

---

## 8. Rollout

Three PRs, each independently mergeable.

**PR 1 — `internal/ghhost/` + config schema + store schema.**

- New package `internal/ghhost/` with `Client`, `ResolvePAT`, `ParseRepoURL`, `GetBranch`, `OpenPullRequest`, scrubber.
- Config: `ManagedAgentsBackend`, `RepositorySpec`, `EnvironmentOverride.Repository`. Validation rules.
- Store: `RepositoryArtifact`, `TeamState.RepositoryArtifacts`.
- Unit tests per §7.1, §7.2.
- No engine wiring yet — the pieces land inert.

**PR 2 — Engine wiring in `cmd/run_ma.go` + prompt builder extension.**

- `runTeamMA` builds `ResourceRef` list from config + upstream `TeamState.RepositoryArtifacts`.
- On `end_turn`, calls `ghhost.GetBranch`, records artifact, optionally calls `ghhost.OpenPullRequest`.
- `internal/injection/builder.go` gains `Capabilities.ArtifactPublish`; MA caller populates, local caller passes nil.
- Unit tests per §7.3, §7.5.
- Single-team MA fixture (from p14) must still pass.

**PR 3 — Multi-team fixture + integration test + docs.**

- `test/integration/ma_repo_relay/` fixture, opt-in test.
- `docs/features/p14-ma-session-lifecycle.md` deviation note updated (branch handling moved here).
- README gains an MA-backend example section.
- `cmd/runs show` renders repository artifacts.

**Rollback.** PR 1 is pure add. PR 2 touches `cmd/run_ma.go` in a way that's flag-gated (MA teams without `repository` fail fast at validate); reverting restores p14 single-team behavior. PR 3 is docs + tests only.

---

## 9. Observability & error handling

- DEBUG `slog` on every `ghhost` call with elapsed-ms + HTTP status. Never log the token, never log the full request body for POSTs.
- PAT-bearing `http.Request` objects are constructed inside `Client.Do`-wrapper; the token is not available to callers inside `ghhost`.
- `ErrBranchNotFound`, `ErrPullRequestExists`, `ErrPATMissing` as exported sentinels. Callers `errors.Is` them.
- Team `LastError` strings for failure modes:
  - No branch pushed: `"no branch pushed: orchestra/<team>-<run-id>"`
  - GitHub unreachable: `"github branch lookup failed: <scrubbed error>"`
  - PR open failed: warning-only, does not become `LastError`.

---

## 10. Open questions

1. **Does MA accept multiple `github_repository` resources on one session?** (§5.4) Must confirm during PR 2 implementation. If it rejects, fallback is a single mount + session-start `git fetch` seeded by orchestra via a setup message or `environment.packages`.
2. **Default branch detection.** Should orchestra call `GET /repos/{owner}/{repo}` to resolve the repo default branch, or assume `main`? Proposed: assume `main`; if the repo uses `master` or something else, the user declares it via `backend.managed_agents.repository.default_branch: master`. Adds one optional field.
3. **`base_sha` meaning.** When resolving team A's `orchestra/a-<run-id>` branch, `base_sha` should be the merge-base against the repo default branch at run start. That requires capturing the default branch head at `Begin()` time. Alternatively, `base_sha` is just "the commit the branch diverged from" as reported by GitHub's `GET /compare` endpoint. Lean toward the compare-endpoint approach; no state needed.
4. **PR body content.** Should orchestra populate the PR body with a summary of team tasks, or leave it blank? Lean blank — humans can edit. A one-line attribution footer (`Opened by orchestra run <run-id>`) is useful for audit.
5. **What happens if team A's branch exists from a prior aborted run?** Deterministic branch name means a re-run under the same run_id collides. Proposed: run_id has a timestamp; collisions are effectively impossible. Don't add collision-avoidance logic.
6. **Where does `ResolvePAT` live in the dependency tree?** It's a singleton concern (one token per orchestra process). Option A: resolve at `cmd/root.go` startup, thread through. Option B: resolve lazily at first `ghhost.Client` construction. Lean A — one explicit "GitHub not configured" error at startup beats a deferred failure mid-run.
7. **Prompt section wording — is `Do NOT open a pull request` strong enough?** Agents with aggressive tool-using tendencies have been observed opening PRs anyway. If we observe this under MA, add a runtime check: on `end_turn`, query `GET /repos/.../pulls?head=...&state=open`; if one exists and `open_pull_requests: false`, log a warning and close it or leave it. Deferred pending real-world data.
8. **Does `orchestra resume` need any P1.5-specific logic?** P1.8 is its own chapter, but resume would want to re-run branch resolution for teams that ended between session-idle and artifact-record. The idempotency property (§NF4) means re-running `GetBranch` is safe; P1.8 just needs to know to do it. Flagged here, implemented there.

---

## 11. Acceptance criteria

- [ ] `internal/ghhost/` package exists with `Client`, `ResolvePAT`, `ParseRepoURL`, `GetBranch`, `OpenPullRequest`, unit tests.
- [ ] `grep -rn "GITHUB_TOKEN\|github_token" pkg/ internal/` shows PAT only in `internal/ghhost/`.
- [ ] `state.json` under a multi-team MA run contains `repository_artifacts[]` with `{url, branch, base_sha, commit_sha, resolved_at}` per team.
- [ ] `cmd/runs show <run-id>` renders repository artifacts (branch + short-SHA, optional PR URL) per team.
- [ ] MA-backend prompt includes artifact-publish instructions naming `orchestra/<team>-<run-id>`; local-backend prompt byte-unchanged.
- [ ] `test/integration/ma_repo_relay/` passes against a scratch GitHub repo when `ORCHESTRA_TEST_REPO_URL` + `ORCHESTRA_TEST_REPO_PAT` are set; skipped otherwise.
- [ ] `make test && make vet && make lint` green.
- [ ] p14 single-team fixture still passes unchanged.
- [ ] README documents the MA repo-relay example.
- [ ] Follow-ups tracked (not shipped): P1.6 multi-team DAG stress test, P1.7 validation warnings, P1.8 resume.
