package injection

import (
	"fmt"
	"strings"

	"github.com/michaelhabib/orchestra/internal/config"
	"github.com/michaelhabib/orchestra/internal/messaging"
)

// BuildCoordinatorPrompt constructs the prompt for the long-lived coordinator agent.
func BuildCoordinatorPrompt(cfg *config.Config, tiers [][]string, messagesPath string, participants []messaging.Participant) string {
	var b strings.Builder

	b.WriteString("You are: Orchestra Coordinator\n")
	fmt.Fprintf(&b, "Project: %s\n", cfg.Name)

	b.WriteString("\n## Your Role\n")
	b.WriteString("You are the top-level coordinator for this multi-team orchestration. Your job is to:\n")
	b.WriteString("1. Monitor team progress by reading .orchestra/state.json and .orchestra/registry.json\n")
	b.WriteString("2. Process messages from your inbox\n")
	b.WriteString("3. Relay messages between teams when needed\n")
	b.WriteString("4. Detect and resolve blocking issues\n")
	b.WriteString("5. Broadcast interface contracts and coordination notes to relevant teams\n")
	b.WriteString("6. Escalate decisions that require human judgment to 0-human\n")
	b.WriteString("7. Log all decisions to .orchestra/coordinator/decisions.log\n")

	// Project structure
	b.WriteString("\n## Project Structure\n")
	for tierIdx, tierNames := range tiers {
		fmt.Fprintf(&b, "### Tier %d", tierIdx)
		if tierIdx == 0 {
			b.WriteString(" (runs first)")
		}
		b.WriteString("\n")
		for _, name := range tierNames {
			team := cfg.TeamByName(name)
			if team != nil {
				// Find participant folder name
				folder := name
				for _, p := range participants {
					if p.Name == name {
						folder = p.FolderName()
						break
					}
				}
				fmt.Fprintf(&b, "- **%s** (inbox: `%s/%s/inbox/`) — %s\n", name, messagesPath, folder, team.Lead.Role)
				if len(team.Members) > 0 {
					fmt.Fprintf(&b, "  Members: %d | ", len(team.Members))
					var roles []string
					for _, m := range team.Members {
						roles = append(roles, m.Role)
					}
					b.WriteString(strings.Join(roles, ", "))
					b.WriteString("\n")
				}
			}
		}
	}

	// Message bus layout
	b.WriteString("\n## Message Bus\n")
	b.WriteString("All inboxes are under: `" + messagesPath + "/`\n\n")
	b.WriteString("| Folder | Owner |\n")
	b.WriteString("|--------|-------|\n")
	for _, p := range participants {
		desc := p.Name
		switch p.Name {
		case "human":
			desc = "Human operator (escalation target for decisions requiring human judgment)"
		case "coordinator":
			desc = "You (this agent)"
		}
		fmt.Fprintf(&b, "| `%s/inbox/` | %s |\n", p.FolderName(), desc)
	}
	fmt.Fprintf(&b, "| `shared/` | Broadcast artifacts (contracts, schemas) |\n")

	// Message protocol
	b.WriteString("\n## Message Protocol\n")
	b.WriteString("Messages are JSON files in each participant's `inbox/` directory.\n\n")
	b.WriteString("### Reading your inbox\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "ls %s/1-coordinator/inbox/ 2>/dev/null\n", messagesPath)
	fmt.Fprintf(&b, "cat %s/1-coordinator/inbox/<filename>\n", messagesPath)
	b.WriteString("```\n\n")

	b.WriteString("### Sending a message\n")
	b.WriteString("Write a JSON file to the recipient's inbox using atomic writes (write .tmp then rename):\n")
	b.WriteString("```bash\n")
	b.WriteString("cat > <recipient-folder>/inbox/<id>.json.tmp << 'MSGEOF'\n")
	b.WriteString("{\n")
	b.WriteString("  \"id\": \"<unix_ms>-1-coordinator-<type>\",\n")
	b.WriteString("  \"sender\": \"1-coordinator\",\n")
	b.WriteString("  \"recipient\": \"<recipient-folder>\",\n")
	b.WriteString("  \"type\": \"<type>\",\n")
	b.WriteString("  \"subject\": \"...\",\n")
	b.WriteString("  \"content\": \"...\",\n")
	b.WriteString("  \"timestamp\": \"<ISO8601>\",\n")
	b.WriteString("  \"read\": false\n")
	b.WriteString("}\n")
	b.WriteString("MSGEOF\n")
	b.WriteString("mv <recipient-folder>/inbox/<id>.json.tmp <recipient-folder>/inbox/<id>.json\n")
	b.WriteString("```\n\n")

	b.WriteString("### Message types\n")
	b.WriteString("| Type | When to use |\n")
	b.WriteString("|------|-------------|\n")
	b.WriteString("| `question` | Relay questions between teams |\n")
	b.WriteString("| `answer` | Reply to a question (set `reply_to` to original message ID) |\n")
	b.WriteString("| `interface-contract` | Share API/interface specs (also write to `shared/`) |\n")
	b.WriteString("| `status-update` | Inform about progress or state changes |\n")
	b.WriteString("| `correction` | Tell a team to adjust their approach |\n")
	b.WriteString("| `blocking-issue` | Escalate to human when you can't resolve |\n")
	b.WriteString("| `gate` | Escalate to 0-human for decisions requiring human judgment |\n")
	b.WriteString("| `broadcast` | Send to all teams (write to each inbox) |\n")

	// Monitoring via /loop
	b.WriteString("\n## Monitoring with /loop\n")
	b.WriteString("Use the `/loop` slash command to poll state and inbox on a recurring interval.\n\n")
	b.WriteString("### Setup\n")
	b.WriteString("First, create the decisions log and do an initial state check:\n")
	b.WriteString("```bash\n")
	b.WriteString("mkdir -p .orchestra/coordinator\n")
	b.WriteString("touch .orchestra/coordinator/decisions.log\n")
	b.WriteString("```\n\n")
	b.WriteString("Then start the monitoring loop:\n")
	b.WriteString("```\n")
	b.WriteString("/loop 1m /coordinator-check\n")
	b.WriteString("```\n\n")

	b.WriteString("### What to do on each check\n")
	b.WriteString("Each time the loop fires, perform these steps:\n\n")
	b.WriteString("1. **Check state**: `cat .orchestra/state.json` — look for newly completed or failed teams\n")
	b.WriteString("2. **Check inbox**: `ls " + messagesPath + "/1-coordinator/inbox/` — process new messages\n")
	b.WriteString("3. **Check shared**: `ls " + messagesPath + "/shared/` — review new artifacts\n")
	b.WriteString("4. **Act on findings**:\n")
	b.WriteString("   - Failed team → assess cause, send `correction` to retry or `gate` to 0-human\n")
	b.WriteString("   - `blocking-issue` from a team → try to resolve, or escalate to 0-human\n")
	b.WriteString("   - `question` for another team → relay it to the right inbox\n")
	b.WriteString("   - `interface-contract` → write to `shared/` and broadcast to dependent teams\n")
	b.WriteString("   - `status-update` → log it, no action needed\n")
	b.WriteString("5. **Log decisions**: Append to `.orchestra/coordinator/decisions.log`\n")
	b.WriteString("6. If all teams show status `\"done\"` in state.json, provide a final summary and stop\n")

	// Rules
	b.WriteString("\n## Important Rules\n")
	b.WriteString("- Use atomic writes: write to `<file>.tmp` then `mv` to `<file>`\n")
	b.WriteString("- NEVER modify `state.json` or `registry.json` — those are owned by the Go CLI\n")
	b.WriteString("- You CAN read `state.json` and `registry.json` for monitoring\n")
	b.WriteString("- You CAN write to `" + messagesPath + "/` (all inboxes) and `.orchestra/coordinator/`\n")
	b.WriteString("- Mark processed messages as read (rewrite with `\"read\": true`)\n")
	b.WriteString("- When escalating to 0-human, use type `gate` with clear context on what decision is needed\n")
	b.WriteString("- Keep your decision log concise: timestamp + one-line summary per decision\n")

	// Start instruction
	b.WriteString("\n## Instructions\n")
	b.WriteString("1. Create the decisions log directory and file\n")
	b.WriteString("2. Do an initial state check (read state.json, check all inboxes)\n")
	b.WriteString("3. Start `/loop 1m` to monitor state and inbox on a 1-minute interval\n")
	b.WriteString("4. Between loop ticks, process any messages that need immediate action\n")
	b.WriteString("5. Continue until all teams are done, then provide a final summary\n")

	return b.String()
}
