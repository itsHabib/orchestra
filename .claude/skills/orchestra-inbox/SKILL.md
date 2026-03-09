---
name: orchestra-inbox
description: Check orchestra message inboxes for messages from teams, coordinator, or human. Use when the user wants to see what messages have been sent during an orchestra run.
argument-hint: "[inbox-name] — e.g., coordinator, human, data-engine, all"
user_invocable: true
---

# /orchestra-inbox -- Check Orchestra Inboxes

Check message inboxes in the orchestra messages directory.

## Messages directory

Default path: `.orchestra/messages/` (relative to current working directory).
If the user specifies a different path, use that instead.

## Argument Parsing

- If `<user_argument>` is provided, use it as the inbox to check:
  - `coordinator` → `1-coordinator`
  - `human` → `0-human`
  - `all` → check every inbox
  - A team name like `data-engine` → find the matching `N-<name>` folder
- If NO argument is provided, list all available inboxes and ask the user which one(s) to check.

## Steps

1. Locate the messages directory (see above).

2. List all participant folders:
   ```bash
   ls -d $MESSAGES/*/
   ```

3. If no argument was provided, present the available inboxes and ask which to check:
   ```
   Available inboxes:
     0-human
     1-coordinator
     2-data-engine
     3-full-stack
     ...
     shared (artifacts)

   Which inbox to check? (or "all")
   ```
   Wait for the user's response before proceeding.

4. For the selected inbox(es), list all `.json` files:
   ```bash
   ls $MESSAGES/<folder>/inbox/*.json 2>/dev/null
   ```

5. If no messages, report "No messages in `<folder>` inbox."

6. If messages exist, read each one and present a summary table:

   | # | From | Type | Subject | Priority | Read |
   |---|------|------|---------|----------|------|
   | 1 | 2-data-engine | blocking-issue | Missing dep | high | no |
   | 2 | 3-full-stack | status-update | API done | normal | yes |

7. For any **unread** messages, show the full content below the table.

8. Also check `$MESSAGES/shared/` and list any shared artifacts (interface contracts, schemas).

## Output Format

Keep it concise. The summary table is the primary output. Only expand unread messages.
If the user asks about a specific message, read and display it in full.
When checking "all", group messages by inbox with a header per inbox.
