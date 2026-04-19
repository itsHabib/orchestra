---
name: orchestra-msg
description: Send a message to a team or the coordinator via the orchestra message bus. Use when the user wants to communicate with teams during an orchestra run — corrections, answers, decisions, or broadcasts.
argument-hint: "<recipient> <message> [--type question|correction|answer|gate|broadcast]"
user_invocable: true
---

# /orchestra-msg -- Send a Message via Orchestra Message Bus

Send a message from `0-human` to a team, the coordinator, or all teams.

## Messages directory

Default path: `.orchestra/messages/` (relative to current working directory).
If the user specifies a different path, use that instead.

## Argument Parsing

Parse the `<user_argument>` for:
- **recipient**: Required. Can be:
  - A team name (e.g., `data-engine`) — resolves to its indexed folder
  - `coordinator` → resolves to `1-coordinator`
  - `all` — broadcasts to every inbox
- **message**: Required. The rest of the argument text after the recipient.
- **--type**: Optional flag. Defaults to `correction`. Valid types:
  `question`, `answer`, `correction`, `gate`, `broadcast`, `status-update`, `interface-contract`

Examples:
- `/orchestra-msg data-engine use int64 for score fields`
- `/orchestra-msg coordinator prioritize API contract --type question`
- `/orchestra-msg all API v2 contract is in shared/ --type broadcast`

## Steps

1. Locate the messages directory (see above).

2. List the participant folders to discover all inboxes:
   ```bash
   ls -d $MESSAGES/*/
   ```

3. Resolve the recipient name to the correct folder:
   - If recipient is `coordinator` → `1-coordinator`
   - If recipient is `human` → `0-human`
   - If recipient matches a team name, find the matching `N-<name>` folder
   - If recipient is `all` → write to every folder except `0-human`

4. Generate the message ID: `<unix_ms>-0-human-<type>`
   Get the timestamp:
   ```bash
   date +%s%3N
   ```

5. Write the message JSON using atomic writes:
   ```bash
   cat > $MESSAGES/<folder>/inbox/<id>.json.tmp << 'MSGEOF'
   {
     "id": "<id>",
     "sender": "0-human",
     "recipient": "<folder>",
     "type": "<type>",
     "subject": "<first 60 chars of message>",
     "content": "<full message>",
     "timestamp": "<ISO8601>",
     "read": false
   }
   MSGEOF
   mv $MESSAGES/<folder>/inbox/<id>.json.tmp $MESSAGES/<folder>/inbox/<id>.json
   ```

6. For `all` (broadcast), repeat step 5 for every participant folder except `0-human`.

7. Confirm: "Message sent to `<folder>` (type: `<type>`)"

## Important
- Always use atomic writes (write `.tmp` then `mv`)
- The sender is always `0-human`
- Keep the subject short (first ~60 chars of the message content)
- For broadcasts, list all recipients in the confirmation
