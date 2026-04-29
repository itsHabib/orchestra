# MCP block / unblock acceptance smoke

Drives the §12.3 acceptance from `docs/DESIGN-ship-feature-workflow.md` end
to end against live Managed Agents: ship a deliberately ambiguous design
doc, watch the team call `signal_completion(blocked)`, send a disambiguating
`user.message` via the `unblock` MCP tool, and assert the team eventually
calls `signal_completion(done)` with a real PR URL.

This is the load-bearing piece of the workflow that the P3 MCP smoke
explicitly deferred — without it, the unblock story is unverified end to
end against MA. P4-lean is scoped to this single acceptance; the multi-doc
happy-path fixture (§12.1 / §12.2) ships in a follow-up.

## Run

The smoke is opt-in. Both gates must be set:

- `ORCHESTRA_MA_INTEGRATION=1` — enables every live-MA integration test.
- `ORCHESTRA_MCP_TEST_REPO_URL` — `https://github.com/<owner>/<repo>` of a
  sandbox repository the agent is allowed to clone, push to, and open a
  PR against.
- `ANTHROPIC_API_KEY` (or a populated `<user-config-dir>/orchestra/config.json`)
  — used by the spawned `orchestra run` subprocess.
- `gh` CLI authenticated against the sandbox repo (`gh auth status`) —
  needed for the agent's PR work inside `/ship-feature`.

Unlike `test/integration/mcp_smoke`, the doc path is **not** an env var.
The fixture lives at a pinned path inside the sandbox so the test
verifies a known-ambiguous spec end to end:

```
orchestra-ma/docs/test-fixtures/ambiguous-mystery-flag.md
```

The sandbox layout scopes per-backend fixtures under their own top-level
directory (`orchestra-ma/` here) so future tests against other backends
don't collide. Before running, ensure the file exists at that exact path
on your sandbox's default branch — the canonical content lives in this
orchestra repo at `docs/test-fixtures/ambiguous-mystery-flag.md`; copy
it verbatim into `orchestra-ma/docs/test-fixtures/ambiguous-mystery-flag.md`
in your sandbox.

```bash
ORCHESTRA_MA_INTEGRATION=1 \
ORCHESTRA_MCP_TEST_REPO_URL=https://github.com/me/sandbox \
  go test ./test/integration/mcp_unblock_smoke -count=1 -v -timeout 35m
```

## What it verifies

The test exercises the full §12.3 chain:

1. `ship_design_docs([fixture-path], repo_url=<sandbox>)` spawns the run.
2. `get_status` polled every 30 s — the team must reach
   `signal_status="blocked"` (the agent reads the doc, decides it's
   ambiguous, calls `signal_completion(blocked)`).
3. `unblock(run_id, team, "make it a --debug bool that enables debug
   logging")` lands a `user.message` on the still-alive session and clears
   the recorded blocked signal so the eventual done-signal isn't dropped
   by the customtools idempotency gate.
4. `get_status` polled every 30 s — the team must reach
   `signal_status="done"` with a non-empty `signal_pr_url`.

Total runtime is capped at 30 minutes (matches `mcp_smoke`'s cap). Per-team
default is 90 minutes; the cap fires inside that to keep the smoke from
burning a full team budget on a single ambiguous doc. If a single
disambiguation legitimately needs longer, surface to the kickoff before
raising the cap rather than just bumping the constant.

## Architecture findings the test surfaces

- **MA closes the session on `signal_completion(blocked)`.** If
  `SteerableSessionID` rejects unblock with `is "done"` / `is "failed"`,
  the team transitioned away from `running` after the blocked signal
  fired. That violates §12.3 — surface to the kickoff before patching.
- **Unblock returns OK but the agent doesn't act on the user.message.**
  If `signal_status` stays `"blocked"` past the deadline after a
  successful unblock, either the agent is ignoring steering or the
  signal-clear-on-unblock wiring regressed. Surface to the kickoff.

Both paths produce explicit `t.Fatalf` messages prefixed with
`§12.3 ARCHITECTURE FINDING` so the diagnostic survives the test runner.

## What this smoke does NOT cover

- The multi-doc happy-path fixture (§12.1 / §12.2) — separate "P4
  expanded" PR if §12.3 holds.
- Concurrent `ship_design_docs` calls.
- Repo-URL inference / repo-relative path validation beyond what
  `recipes.ShipDesignDocs` already does.
- The HTTP transport.
