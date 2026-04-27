# ship-feature P2 smoke

Live-MA smoke test for the P2 wiring: skill-registration cache → managed
agent spec resolution → signal_completion dispatch → state + notifications.

The test is opt-in: it runs only when `ORCHESTRA_MA_INTEGRATION=1` and an
Anthropic API key is reachable (`ANTHROPIC_API_KEY` env var or
`<user-config-dir>/orchestra/config.json:api_key`).

It does **not** run the full /ship-feature workflow (PR + reviews + CI) —
that is P4. This smoke verifies that an MA agent created with the
`ship-feature` skill attached and the `signal_completion` custom tool
attached can call the tool, that the engine dispatches the call, and that
state.json + notifications.ndjson are updated.

## Prerequisites

1. `orchestra skills upload ship-feature` has been run (the skill is
   registered with Anthropic and cached locally).
2. `ANTHROPIC_API_KEY` is set in the environment.

## Run

```
ORCHESTRA_MA_INTEGRATION=1 go test -count=1 -v ./test/integration/ship_feature_smoke/...
```
