# MCP server smoke test

Drives the orchestra MCP server end-to-end from a Go test client. Built and
run as a real subprocess (`orchestra mcp --transport stdio`) so the test
exercises the same path Claude Code attaches to.

## Run mode

Set `ORCHESTRA_MA_INTEGRATION=1`. The test:

- builds the orchestra binary (`go build -o … .`)
- spawns `orchestra mcp --transport stdio` as a child process
- connects an MCP client over stdio (modelcontextprotocol/go-sdk)
- runs `Initialize`, `ListTools`, `ListResources`, `ListResourceTemplates`, and `list_runs` against the empty registry
- exits after asserting every v1 generic tool and resource is advertised

This confirms the MCP wiring (official Go SDK, our tool definitions, the
binary entry point) is healthy. It does **not** spawn an `orchestra run`
subprocess and does **not** call the Anthropic API.

```bash
ORCHESTRA_MA_INTEGRATION=1 \
  go test ./test/integration/mcp_smoke -count=1 -v
```

## What the smoke does NOT cover

- A live `run` against managed agents (orthogonal — exercise the inline-DAG path against a real DAG manually with `claude mcp` for now).
- The HTTP transport (`--transport http`).
