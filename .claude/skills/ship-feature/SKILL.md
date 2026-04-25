---
name: ship-feature
description: Take a design doc through to a PR with reviews requested. Implements in a worktree, opens the PR, iterates locally on CI until green, then schedules a remote agent to push-notify when reviews land.
argument-hint: "[design-doc-path] — defaults to the most recently committed design doc on main"
user_invocable: true
---

# /ship-feature -- Design Doc → PR → Reviews

Drive a single piece of work end-to-end: pick up a design doc, implement it in an isolated worktree, open a PR with reviews requested from `@claude` / `@codex` / Copilot, baby-sit CI in the active session, then hand off to a scheduled remote agent that pings you when all three reviews are in.

## Arguments

Parse `<user_argument>` as: `[design-doc-path]`

- `design-doc-path` (optional): path to a markdown design doc. If omitted, auto-discover the most recent design doc committed on `main`.

Store as `$DOC`.

## Steps

### 1. Resolve the design doc

If `$DOC` was given, verify the file exists. If it doesn't, abort with a clear message — do nothing else.

If `$DOC` was NOT given, auto-discover from `main`:

```bash
git log main --diff-filter=A --name-only --pretty=format: -- 'docs/features/*.md' 'docs/prompts/*.md' \
  | awk 'NF' | head -1
```

Show the discovered path and ask the user to confirm via `AskUserQuestion`:
- "Use this doc" (Recommended)
- "Cancel"

Cancel → exit cleanly. Otherwise read the doc into `$DOC_CONTENT` and derive `$SLUG` from the filename (strip extension, replace any non-alphanumeric with `-`, e.g. `09-foo.md` → `09-foo`).

If a remote branch `feature/$SLUG` already exists (`git ls-remote --heads origin "feature/$SLUG"`), append `-2`, `-3`, etc. until you find a free name. Final value goes in `$BRANCH`.

### 2. Implement the doc in an isolated worktree

Use the `Agent` tool with `isolation: "worktree"` and `subagent_type: "general-purpose"`. The runtime auto-creates a worktree off HEAD and returns the path + branch when the agent finishes.

Prompt template for the subagent:

```
You are implementing a design doc end-to-end in an isolated worktree.

Design doc path: <$DOC>
Branch to create: <$BRANCH>

--- DESIGN DOC CONTENT ---
<$DOC_CONTENT>
--- END DESIGN DOC ---

Repo conventions (from .claude/CLAUDE.md):
- All file writes use atomic pattern: write .tmp then os.Rename
- All packages live under internal/, NOT pkg/
- Tests use real binary + mock claude script — no mocks/interfaces for the spawner
- Do NOT force-push

Your job:
1. Create branch `<$BRANCH>` off the current HEAD in this worktree.
2. Implement the design doc in full. Read the existing code; reuse existing utilities where they fit.
3. Run `make vet` and `make test` and make them pass before committing.
4. Commit in logical chunks with descriptive messages.
5. Do NOT push. Do NOT open a PR. The calling session will do that.

When done, return: a one-paragraph summary of what you built, plus a list of the files you touched.
```

Capture the worktree path returned by the Agent tool as `$WT`. The branch is `$BRANCH`.

If the Agent reports failure or makes no commits, abort: do NOT push, do NOT open a PR. Print a clear failure summary including the worktree path so the user can inspect it manually.

### 3. Push and open the PR

From the worktree, push and open the PR:

```bash
git -C "$WT" push -u origin "$BRANCH"
```

Build a PR title from the doc's H1 (or filename if there's no H1). Build the body from the doc's first paragraph plus a test plan. Then:

```bash
PR_URL=$(gh pr create \
  --base main --head "$BRANCH" \
  --title "<derived title>" \
  --body "$(cat <<'EOF'
## Summary
<2-3 bullet points distilled from the design doc>

## Design doc
<relative link to $DOC on main>

## Test plan
- [ ] `make vet`
- [ ] `make test`
- [ ] <doc-specific verification steps>

Generated with /ship-feature
EOF
)")
PR_NUMBER=$(echo "$PR_URL" | grep -o '[0-9]*$')
```

Capture both `$PR_URL` and `$PR_NUMBER`.

### 4. Fan out review requests

Three commands, in this exact shape — `@claude` and `@codex` as separate comments, Copilot as a requested reviewer. Do NOT comment `@copilot` (it pushes commits).

```bash
gh pr comment "$PR_NUMBER" --body "@claude"
gh pr comment "$PR_NUMBER" --body "@codex"
gh pr edit "$PR_NUMBER" --add-reviewer Copilot
```

If the Copilot reviewer add fails (handle differs per repo/org), retry with `copilot-pull-request-reviewer`. If both fail, print a one-liner asking the user to add Copilot manually and continue — don't block on this.

### 5. Iterate locally on CI until green

Invoke `/loop` in dynamic mode (no interval — model self-paces) with the body prompt below. The loop runs in the active session so the user can see each iteration and Ctrl-C if needed.

Loop body prompt:

```
Polling CI for PR #<$PR_NUMBER> in worktree <$WT>.

1. Run: gh pr checks <$PR_NUMBER> --json name,state,conclusion
2. Classify each check:
   - SUCCESS → counted as passing
   - IN_PROGRESS / QUEUED / PENDING → still running
   - FAILURE / CANCELLED / TIMED_OUT / ACTION_REQUIRED → failed

3. Decide:
   - All checks SUCCESS → DONE. Print "CI green ✓" and do NOT call ScheduleWakeup (this exits the loop).
   - Any failure → fetch logs with `gh run view <run-id> --log-failed`, fix the underlying issue in the worktree at <$WT>, commit with a descriptive message, push. Then ScheduleWakeup(~270s) to re-check.
   - Otherwise (still running) → ScheduleWakeup(~270s) to re-check.

Never use destructive git operations. Never force-push. Fix the actual bug — do not skip tests or hooks.
```

When the loop exits naturally (CI green), proceed to step 6.

### 6. Schedule the review-wait tail and exit

Create a one-time scheduled remote agent via the `mcp__scheduled-tasks__create_scheduled_task` tool. Cadence: every 20 minutes, hard cap at 6 hours. The scheduled agent has zero memory of this session, so the prompt must be fully self-contained:

```
PR #<$PR_NUMBER> at <$PR_URL> needs three reviews to land: @claude, @codex, and Copilot.

Each time you wake:

1. Fetch PR state:
     gh pr view <$PR_NUMBER> --json reviews,comments,reviewRequests

2. Determine which reviewers have responded:
   - @claude responded → a comment exists whose author login is "claude" or matches the claude bot app
   - @codex responded → a comment exists whose author login is "codex" or matches the codex bot app
   - Copilot responded → an entry exists in `reviews` whose author is Copilot (and Copilot is no longer in `reviewRequests`)

3. Branch on result:
   - All three responded → call PushNotification with title "PR #<$PR_NUMBER> — reviews ready" and body "All 3 reviews in on <$PR_URL>". Then delete this scheduled task. Done.
   - Some still pending AND total elapsed time < 6 hours → exit cleanly; the scheduler will wake you again in 20 minutes.
   - Some still pending AND total elapsed >= 6 hours → call PushNotification with title "PR #<$PR_NUMBER> — review timeout" and body listing which reviewers are still missing. Then delete this scheduled task.

Track elapsed time by the schedule's creation timestamp vs. now.

Do NOT push commits. Do NOT comment on the PR. This task only observes and notifies.
```

Use cron expression `*/20 * * * *` (every 20 min) and capture the task id as `$TASK_ID`.

After scheduling, print the final summary and exit:

```
--- /ship-feature complete ---
Doc:       <$DOC>
Branch:    <$BRANCH>
Worktree:  <$WT>
PR:        <$PR_URL>
CI:        green ✓
Reviews:   scheduled tail watching (task <$TASK_ID>) — push notification on completion

Nothing else for you to do until the notification fires.
```

## Important

- This skill is a **single bundled approval**: once the user invokes `/ship-feature`, run all six steps without re-prompting between them. The only allowed prompt is the one-key doc confirmation in step 1 when no path arg is given.
- Never force-push. Never skip hooks. If a CI fix would require either, stop the loop and surface it to the user instead.
- If the implementation agent in step 2 reports failure, abort BEFORE push. The worktree stays on disk for manual inspection — print its path.
- Copilot's reviewer handle differs by org. Don't block the workflow if `--add-reviewer` fails; print a one-liner and continue.
- The scheduled tail in step 6 must be self-contained — the remote agent has no context from this session.
- If CI never goes green, the user can Ctrl-C the loop. Step 6 is never reached, so no orphan scheduled task is created.
