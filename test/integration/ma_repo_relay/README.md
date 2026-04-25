# Managed Agents Repo Relay Fixture

Opt-in integration fixture for P1.5 — repo-backed artifact flow. Two teams under the managed-agents backend exercise the full loop: tier-0 edits a file and pushes a branch; tier-1 mounts that branch read-only, appends, and pushes its own branch.

This fixture is **not** part of `go test ./...`. It is invoked manually against a scratch GitHub repository and is skipped by default.

## Prerequisites

- A scratch GitHub repository you control (will receive throwaway branches).
- A personal access token with `repo` scope (private) or `public_repo` (public).
- `ANTHROPIC_API_KEY` with managed-agents access.

## Run

```bash
ORCHESTRA_TEST_REPO_URL=https://github.com/your-user/orchestra-scratch \
GITHUB_TOKEN=ghp_... \
ANTHROPIC_API_KEY=sk-ant-... \
ORCHESTRA_MA_INTEGRATION=1 \
go run . run test/integration/ma_repo_relay/orchestra.yaml
```

(Edit the `repository.url` in `orchestra.yaml` to point at your scratch repo, or template it via your invoker.)

## Expected post-run checks

- `.orchestra/state.json` has `teams.team-a.status == "done"` and `teams.team-b.status == "done"`.
- Both teams have a non-empty `repository_artifacts[]` entry with `branch`, `commit_sha`, and `base_sha`.
- `team-b`'s `commit_sha` differs from `team-a`'s, and both branches exist on the remote.
- `team-b`'s commit message body / diff includes content building on team-a's change.

## Cleanup

Delete the two branches when done:

```bash
gh api -X DELETE "repos/your-user/orchestra-scratch/git/refs/heads/orchestra/team-a-<run-id>"
gh api -X DELETE "repos/your-user/orchestra-scratch/git/refs/heads/orchestra/team-b-<run-id>"
```
