---
name: orchestra-monitor
description: Monitor an orchestra run — team status, unread messages, costs, failures, and live activity. Designed to run with /loop for continuous monitoring as a human coordinator.
argument-hint: "[path-to-.orchestra] — e.g., ~/dev/my-project/.orchestra"
user_invocable: true
---

# /orchestra-monitor -- Orchestra Run Monitor

Single-pass status check for an active orchestra run. Designed to be called via `/loop 1m /orchestra-monitor <path>`.

## Workspace path

If `<user_argument>` is provided, use it as the path to the `.orchestra/` directory.
Otherwise default to `.orchestra/` in the current working directory.

Store the resolved path as `$WS` for the rest of this skill. Messages are at `$WS/messages/`.

## Steps

Perform ALL of the following checks in a single pass. Be concise — this runs every minute.

### 1. Team Status

Read `$WS/state.json` and `$WS/registry.json`. `state.json` uses the v2 run-state shape: top-level `project`, optional `backend`/`run_id`/`started_at`, and a `teams` map keyed by team name. Local runs still use `status: "done"` for completed teams.

Count teams by status and show a one-line summary:
```
Teams: 2/7 done | 2 running | 3 pending | 0 failed
```

If any team has status `"failed"`, flag it prominently:
```
!! FAILED: data-engine — check .orchestra/logs/data-engine.log
```

### 2. Cost & Duration

From `state.json`, sum `cost_usd` across all completed teams. From `registry.json`, compute wall clock for running teams (now - started_at).

```
Cost: $4.23 total | Running: full-stack (12m), security-caching (8m)
```

### 3. Live Activity (per running team)

For each running team, provide a SHORT activity snapshot. Use these methods (in order of preference):

**a) Files created/modified** — Run `find` on the project directory for files modified since the team started. Show counts and notable recent files:
```
  phoenix-ideation (4m): 3 files written (research-notes.md, brainstorm.md, architecture.md)
  titan-ideation (4m): 1 file written (research-notes.md), 2 teammates active
```

**b) Latest log line** — Extract the most recent meaningful assistant text from `$WS/logs/<team>.log`. Parse the JSON log for the last `"type":"assistant"` message with `"type":"text"` content. Show a truncated snippet (max 80 chars):
```
  hydra-ideation (4m): "Evaluating swarm consensus patterns for multi-agent coor..."
```

**c) Teammate count** — Check if the team has spawned subagents by looking for TeamCreate/SendMessage tool calls in the log:
```
  cipher-ideation (4m): 3 teammates spawned, waiting on results
```

Pick whichever signals are available. Show 1 line per running team. Skip teams with nothing interesting.

### 4. Deliverables Progress

For each running team, check what deliverable files exist vs what's expected. Use the orchestra.yaml `deliverables` or `verify` fields as a guide, or simply count files in the team's expected output directory.

```
Deliverables: phoenix 2/6, titan 4/6, hydra 1/6, nova 0/6, cipher 3/6
```

If deliverables aren't clearly defined or no files exist yet, omit this section.

### 5. Unread Messages

Check ALL inboxes under `$WS/messages/` for unread messages (JSON files where `"read": false`).

Focus on `0-human/inbox/` and `1-coordinator/inbox/` first (messages directed at you), then scan team inboxes.

If unread messages exist, list them:
```
Unread messages (3):
  -> 0-human: [gate] "Need approval for API key rotation" from 2-data-engine
  -> 1-coordinator: [blocking-issue] "Missing dep" from 3-full-stack
  -> 1-coordinator: [status-update] "API routes done" from 3-full-stack
```

If no unread messages: `No unread messages.`

### 6. Shared Artifacts

List any files in `.orchestra/messages/shared/`:
```
Shared artifacts: 2 (api-contract-v1.json, db-schema-v1.json)
```

If none: omit this section entirely.

## Output Format

Keep it tight but informative — this is a dashboard glance. Target ~8-15 lines per tick. Example of a full output:

```
[orchestra] cf-open-estimate — 14m elapsed
Teams: 2/7 done | 2 running | 3 pending | 0 failed
Cost: $4.23 | Running: full-stack (12m), security-caching (8m)

Activity:
  full-stack (12m): 5 files written (routes.go, handlers.go, main.go...), "Wiring up health endpoint..."
  security-caching (8m): 3 teammates spawned, 2 files written (ratelimit.go, safeguard.go)

Deliverables: full-stack 3/5, security-caching 2/4

Unread messages (1):
  -> 1-coordinator: [blocking-issue] "Missing dep" from 3-full-stack

Shared: api-contract-v1.json
```

Quiet tick (no messages, early in run):
```
[orchestra] Agent Hackathon — 4m elapsed
Teams: 0/11 done | 5 running | 6 pending | 0 failed
Cost: $0.00

Activity:
  phoenix-ideation (4m): 3 files written (research-notes.md, brainstorm.md, architecture.md)
  hydra-ideation (4m): 2 teammates active, "Designing swarm consensus protocol..."
  titan-ideation (4m): concept picked: SentinelAgent, 1 file written
  nova-ideation (4m): 3 teammates spawned, researching creative AI patterns
  cipher-ideation (4m): 2 files written (research-notes.md, threat-model.md)

No unread messages
```

## Important

- Do NOT read full message contents — just show sender, type, and subject
- If the user wants to read or respond to a message, tell them to use `/orchestra-inbox` or `/orchestra-msg`
- If a team has failed, suggest checking the log file at `$WS/logs/<team>.log`
- The Activity section is the most valuable part — make it count. Show what's actually happening, not just "running".
- When extracting log snippets, skip tool calls and system messages — only show assistant text that reveals intent or progress.
