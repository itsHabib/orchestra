# MCP server smoke test

Drives the orchestra MCP server end-to-end from a Go test client. Built and
run as a real subprocess (`orchestra mcp --transport stdio`) so the test
exercises the same path Claude Code attaches to.

## Run modes

The smoke runs in two phases. Both gates must be opted into.

### 1. Protocol smoke — recommended baseline

Set `ORCHESTRA_MA_INTEGRATION=1`. The test:

- builds the orchestra binary (`go build -o … ./cmd/orchestra`)
- spawns `orchestra mcp --transport stdio` as a child process
- connects an in-memory MCP client over stdio
- runs `Initialize`, `ListTools`, and `list_jobs` against the empty registry
- exits after asserting all four tools are advertised

This confirms the MCP wiring (mark3labs/mcp-go server, our tool definitions,
the binary entry point) is healthy. It does **not** spawn an `orchestra run`
subprocess and does **not** call the Anthropic API.

```bash
ORCHESTRA_MA_INTEGRATION=1 \
  go test ./test/integration/mcp_smoke -count=1 -v
```

### 2. Live ship-design-docs path — opt-in further

In addition to `ORCHESTRA_MA_INTEGRATION=1`, set:

- `ORCHESTRA_MCP_TEST_REPO_URL` — `https://github.com/<owner>/<repo>` of a
  sandbox repository the agent is allowed to clone and push to.
- `ORCHESTRA_MCP_TEST_DOC_PATH` — repo-relative path of a design doc the
  `/ship-feature` skill should drive to PR.
- `ANTHROPIC_API_KEY` (or a populated `<user-config-dir>/orchestra/config.json`)
  — used by the spawned `orchestra run` subprocess.
- A reachable `gh` CLI auth (`gh auth status`) for the agent's PR work.

The test then calls `ship_design_docs`, polls `get_status` every 30 seconds,
and asserts the run reaches `status: done` within 30 minutes. Failure or block
ends the test immediately. Unblock is not exercised by the smoke (P3-OOS).

```bash
ORCHESTRA_MA_INTEGRATION=1 \
ORCHESTRA_MCP_TEST_REPO_URL=https://github.com/me/sandbox \
ORCHESTRA_MCP_TEST_DOC_PATH=docs/feat-foo.md \
  go test ./test/integration/mcp_smoke -count=1 -v -timeout 35m
```

## What the smoke does NOT cover

- A managed GitHub fixture repo with prepared design docs (P4).
- Concurrent `ship_design_docs` calls (verified ad-hoc; not gated on).
- The HTTP transport (`--transport http`).
- `unblock` against a real blocked agent (P4).
