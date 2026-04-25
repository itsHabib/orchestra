# Managed Agents Steering Fixture

Opt-in delivery test for `orchestra msg` against a live MA session. The single team is given a verbose-by-design task so the session stays `running` long enough for the harness to land a steering message before the run finishes.

The fixture verifies **delivery** — that the steering event reaches MA and gets echoed back through the stream. It does **not** verify the agent obeys the steering; that's model-variance noise.

## Run

The Make target wraps the test driver and the API key check:

```bash
ORCHESTRA_MA_INTEGRATION=1 ANTHROPIC_API_KEY=... make e2e-ma-steering
```

Cost: ≈ $0.05–0.10 per run.

## What the harness does

1. Builds the orchestra binary into a temp dir.
2. Starts `orchestra run test/integration/ma_steering/orchestra.yaml` in the background.
3. Polls `.orchestra/state.json` until `teams.intro.status == "running"` and `session_id` is populated.
4. Invokes `orchestra msg --team intro --message "<sentinel>"` against the same workspace.
5. Waits for `orchestra run` to finish.
6. Asserts:
   - the `orchestra msg` exit code was 0,
   - `.orchestra/logs/intro.ndjson` contains a `user.message` event with the sentinel text,
   - `state.Teams["intro"].LastEventID` advanced past the steering event.

## Manual quick-check

```bash
ORCHESTRA_MA_INTEGRATION=1 ANTHROPIC_API_KEY=... ./orchestra run test/integration/ma_steering/orchestra.yaml &
# In another shell:
./orchestra sessions ls
./orchestra msg --team intro --message "use the JSON store, not a database"
# Wait for the background run to finish; then:
jq '.events[] | select(.type=="user.message")' .orchestra/logs/intro.ndjson
```
