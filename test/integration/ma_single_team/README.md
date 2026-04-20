# Managed Agents Single-Team Fixture

Opt-in smoke fixture for P1.4 managed-agents session lifecycle. It starts one MA session, asks for a text-only markdown deliverable, and expects Orchestra to persist the final agent message to `.orchestra/results/research-brief/summary.md`.

Run from the repository root with a real Managed Agents API key:

```bash
ORCHESTRA_MA_INTEGRATION=1 ANTHROPIC_API_KEY=... go run . run test/integration/ma_single_team/orchestra.yaml
```

Expected post-run checks:

- `.orchestra/state.json` has `teams.research-brief.status == "done"`.
- `teams.research-brief.agent_id`, `agent_version`, `session_id`, and `last_event_id` are populated.
- `.orchestra/results/research-brief/summary.md` exists and is non-empty.
- `.orchestra/logs/research-brief.ndjson` contains `session.status_running`, `agent.message`, and `session.status_idle`.
