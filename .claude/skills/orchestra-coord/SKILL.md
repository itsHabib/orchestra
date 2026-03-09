---
name: orchestra-coord
description: Designate this session as the human coordinator for an orchestra run. Reads the project config, shows current status, starts a monitor loop, and primes the session to handle messages and interventions.
argument-hint: "<project-dir> [poll-interval] — e.g., ~/dev/ez-slots 2m"
user_invocable: true
---

# /orchestra-coord -- Become the Orchestra Coordinator

Bootstrap this Claude Code session as the human coordinator for an active (or about-to-start) orchestra run.

## Arguments

Parse `<user_argument>` as: `<project-dir> [poll-interval]`

- `project-dir` (required): path to the project root containing `orchestra.yaml` and `.orchestra/`
- `poll-interval` (optional): monitor frequency, defaults to `2m`. Supports `Nm` or `Ns` format.

Store as `$DIR` and `$INTERVAL`.

## Steps

### 1. Read the project config

Read `$DIR/orchestra.yaml`. Extract:
- Project name
- Team names, roles, and tier structure (from `depends_on`)
- Number of teams and tiers

Print a brief project summary:
```
Coordinating: ez-slots (8 teams, 5 tiers)
Workspace: ~/dev/ez-slots/.orchestra/
```

### 2. Check current state

If `$DIR/.orchestra/state.json` exists, read it and show current status:
```
Status: 2/8 done | 2 running | 4 pending | 0 failed
```

If no workspace exists yet:
```
No active run — start with: cd $DIR && orchestra run orchestra.yaml
```

### 3. Offer to start a monitor loop

Ask the user if they want a recurring monitor loop, and at what frequency:

```
Want me to start a monitor loop? (default: every 2m)
```

If they say yes (or give a frequency), use `/loop` to start it:
```
/loop $INTERVAL /orchestra-monitor $DIR/.orchestra
```

If they decline, skip it — they can always run `/orchestra-monitor` manually.

### 4. Print coordinator reference card

Print a compact reference of available commands:

```
--- Coordinator Mode Active ---
Monitor:  /orchestra-monitor $DIR/.orchestra  (looping every $INTERVAL)
Inbox:    /orchestra-inbox [team|coordinator|human|all]
Send msg: /orchestra-msg <team> <message>
Stop:     CronDelete <job-id>

Your role:
- Watch the monitor ticks for failures, stalls, or blocked teams
- Check /orchestra-inbox when you see unread messages
- Use /orchestra-msg to reply to teams or broadcast corrections
- Escalations to 0-human/inbox/ are directed at YOU — act on them
```

### 5. Prime the session context

After printing the reference card, tell the session:

```
I'm now coordinating this orchestra run. I'll watch the monitor ticks and alert you to:
- Failed teams (with log file paths)
- Unread messages that need human decisions (gate messages)
- Stalled teams (no file writes for 5+ minutes)
- Tier transitions (when a tier completes and the next one starts)

You can ask me anything about the run, or tell me to intervene with a team.
```

## Important

- Do NOT start the orchestra run itself — the user runs that in a separate terminal
- Do NOT read full log files unless asked — just monitor via the loop
- The monitor loop is the heartbeat — if it shows problems, surface them proactively
- When a gate message arrives in 0-human/inbox/, read it and present the decision to the user
- Keep responses concise during monitoring — this is a dashboard, not a novel
