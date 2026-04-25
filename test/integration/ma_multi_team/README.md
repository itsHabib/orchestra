## Managed Agents Multi-Team Fixture (P1.6)

Opt-in smoke fixture for multi-team DAG orchestration under the managed-agents
backend. Two teams: a `planner` (tier 0) writes a three-bullet outline; an
`analyst` (tier 1, depends on planner) reads the outline via the dependency-
result injection path and expands each bullet into a paragraph. No GitHub. No
repo mounts. The deliverable that flows between tiers is the planner's final
`agent.message` text, persisted to `.orchestra/results/planner/summary.md` and
inlined into the analyst's prompt.

Run from the repository root with a real Managed Agents API key:

```bash
ORCHESTRA_MA_INTEGRATION=1 ANTHROPIC_API_KEY=... go run . run test/integration/ma_multi_team/orchestra.yaml
```

The `ORCHESTRA_MA_INTEGRATION=1` gate keeps this off the default `make test`
path. CI runs unit tests by default; this fixture runs only when invoked
explicitly (manual run or a separate workflow with secret-backed credentials).

### Expected post-run checks

- `.orchestra/state.json` has `teams.planner.status == "done"` and
  `teams.analyst.status == "done"`.
- Both teams have populated `agent_id`, `agent_version`, `session_id`, and
  `last_event_id`.
- `.orchestra/results/planner/summary.md` contains three bulleted lines.
- `.orchestra/results/analyst/summary.md` contains three `## `-prefixed
  paragraphs whose headers echo the planner's bullets — proof that the
  upstream summary reached the downstream prompt.
- `.orchestra/logs/planner.ndjson` and `analyst.ndjson` each contain
  `session.status_running`, `agent.message`, and `session.status_idle` with
  `stop_reason: end_turn`.

### Notes

- `defaults.ma_concurrent_sessions: 4` keeps the create-rate footprint small
  for a two-team fixture; the production default is 20.
- The fixture spends real API tokens on every run. Treat it as a smoke test,
  not a regression suite — invoke it when the multi-team MA path changes shape,
  not on every commit.
