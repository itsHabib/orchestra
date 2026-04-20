package injection

import (
	"fmt"
	"strings"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/workspace"
)

// BuildPrompt constructs the full prompt for a team's claude -p session.
// tierPeers is the list of all team names in the same tier (including self); pass nil for single-team spawns.
// inboxFolder is this team's message bus folder name (e.g., "2-data-engine"); empty disables messaging.
// messagesPath is the base path to the messages directory.
func BuildPrompt(team *config.Team, projectName string, state *workspace.State, cfg *config.Config, tierPeers []string, inboxFolder, messagesPath string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are: %s\n", team.Lead.Role)
	fmt.Fprintf(&b, "Project: %s\n", projectName)

	writeTechnicalContext(&b, team.Context)
	writeTasks(&b, team.Tasks)
	writeTierPeers(&b, team, cfg, tierPeers)
	writeTeamMembers(&b, team)
	writeDependencyContext(&b, team, state, cfg)
	writeMessageBus(&b, inboxFolder, messagesPath, cfg.Defaults.InboxPollInterval)
	writeInstructions(&b, team.HasMembers(), inboxFolder != "" && messagesPath != "")

	return b.String()
}

func writeTechnicalContext(b *strings.Builder, context string) {
	if context == "" {
		return
	}
	b.WriteString("\n## Technical Context\n")
	b.WriteString(context)
	if !strings.HasSuffix(context, "\n") {
		b.WriteString("\n")
	}
}

func writeTasks(b *strings.Builder, tasks []config.Task) {
	b.WriteString("\n## Your Tasks\n")
	for _, task := range tasks {
		fmt.Fprintf(b, "### Task: %s\n", task.Summary)
		if task.Details != "" {
			b.WriteString(task.Details)
			b.WriteString("\n")
		}
		if len(task.Deliverables) > 0 {
			fmt.Fprintf(b, "Expected deliverables: %s\n", strings.Join(task.Deliverables, ", "))
		}
		if task.Verify != "" {
			fmt.Fprintf(b, "Verify: `%s`\n", task.Verify)
		}
		b.WriteString("\n")
	}
}

func writeTierPeers(b *strings.Builder, team *config.Team, cfg *config.Config, tierPeers []string) {
	peers := peersExcept(tierPeers, team.Name)
	if len(peers) == 0 {
		return
	}

	b.WriteString("## Parallel Teams (Your Tier)\n")
	b.WriteString("These teams are running alongside you in the same tier. Coordinate your work\n")
	b.WriteString("to avoid conflicts — don't modify files they own, and design compatible interfaces.\n")
	b.WriteString("If you need to share interface contracts or coordination notes with peers,\n")
	b.WriteString("write them to .orchestra/shared/ so other teams can reference them.\n\n")
	for _, p := range peers {
		pt := cfg.TeamByName(p)
		if pt == nil {
			continue
		}
		var summaries []string
		for _, task := range pt.Tasks {
			summaries = append(summaries, task.Summary)
		}
		fmt.Fprintf(b, "- %s (%s): %s\n", p, pt.Lead.Role, strings.Join(summaries, ", "))
	}
	b.WriteString("\n")
}

func peersExcept(tierPeers []string, teamName string) []string {
	var peers []string
	for _, p := range tierPeers {
		if p != teamName {
			peers = append(peers, p)
		}
	}
	return peers
}

func writeTeamMembers(b *strings.Builder, team *config.Team) {
	if !team.HasMembers() {
		return
	}

	fmt.Fprintf(b, "## Your Team\n")
	fmt.Fprintf(b, "You have %d teammates. Assign each teammate 2-6 tasks from the list above\n", len(team.Members))
	b.WriteString("based on their focus area. Each teammate's spawn prompt MUST include:\n")
	b.WriteString("1. The full Technical Context above (they don't inherit your conversation)\n")
	b.WriteString("2. Their specific assigned tasks with details, deliverables, and verify commands\n")
	b.WriteString("3. Any relevant results from previous teams\n\n")
	b.WriteString("Teammates:\n")
	for _, m := range team.Members {
		fmt.Fprintf(b, "- %s: %s\n", m.Role, m.Focus)
	}
	b.WriteString("\n")
}

func writeDependencyContext(b *strings.Builder, team *config.Team, state *workspace.State, cfg *config.Config) {
	if len(team.DependsOn) == 0 || state == nil {
		return
	}

	b.WriteString("## Context from Previous Teams\n")
	for _, depName := range team.DependsOn {
		ts, ok := state.Teams[depName]
		if !ok || ts.Status != "done" {
			continue
		}
		depRole := depName
		if depTeam := cfg.TeamByName(depName); depTeam != nil {
			depRole = depTeam.Lead.Role
		}
		fmt.Fprintf(b, "### %s (%s) — Completed\n", depName, depRole)
		fmt.Fprintf(b, "Summary: %s\n", ts.ResultSummary)
		if len(ts.Artifacts) > 0 {
			fmt.Fprintf(b, "Artifacts: %s\n", strings.Join(ts.Artifacts, ", "))
		}
		b.WriteString("\n")
	}
}

func writeMessageBus(b *strings.Builder, inboxFolder, messagesPath, pollInterval string) {
	if inboxFolder == "" || messagesPath == "" {
		return
	}

	b.WriteString("## Message Bus (Cross-Team Communication)\n")
	fmt.Fprintf(b, "Your inbox: `%s/%s/inbox/`\n\n", messagesPath, inboxFolder)
	writeBootstrapInstructions(b, inboxFolder, messagesPath)
	writeInboxMonitoringInstructions(b, inboxFolder, messagesPath, pollInterval)
	writeSendingInstructions(b, inboxFolder, messagesPath)
	b.WriteString("### When to send messages\n")
	b.WriteString("- Need info from a parallel team → `question` to their inbox\n")
	b.WriteString("- Blocked on something → `blocking-issue` to `1-coordinator`\n")
	b.WriteString("- Defined an API or interface → `interface-contract` to `1-coordinator` (it will broadcast)\n")
	b.WriteString("- Major milestone → `status-update` to `1-coordinator`\n\n")
	b.WriteString("### Shared artifacts\n")
	fmt.Fprintf(b, "Check for shared interface contracts: `ls %s/shared/ 2>/dev/null`\n\n", messagesPath)
}

func writeBootstrapInstructions(b *strings.Builder, inboxFolder, messagesPath string) {
	b.WriteString("### Bootstrap messages\n")
	b.WriteString("Before starting work, check your inbox for `bootstrap` messages from the orchestrator.\n")
	b.WriteString("These contain results and context from teams that ran before you.\n")
	fmt.Fprintf(b, "```\nls %s/%s/inbox/ 2>/dev/null && for f in %s/%s/inbox/*.json; do cat \"$f\" 2>/dev/null; done\n```\n\n", messagesPath, inboxFolder, messagesPath, inboxFolder)
}

func writeInboxMonitoringInstructions(b *strings.Builder, inboxFolder, messagesPath, pollInterval string) {
	b.WriteString("### Inbox monitoring (team lead only — do NOT pass this to teammates)\n")
	fmt.Fprintf(b, "Start a `/loop` to check your inbox every %s. Do this early in your session:\n", pollInterval)
	b.WriteString("```\n")
	fmt.Fprintf(b, "/loop %s check inbox: ls %s/%s/inbox/ 2>/dev/null && for f in %s/%s/inbox/*.json; do cat \"$f\" 2>/dev/null; done\n", pollInterval, messagesPath, inboxFolder, messagesPath, inboxFolder)
	b.WriteString("```\n")
	b.WriteString("When a message arrives, read it and act accordingly — answer questions, adopt\n")
	b.WriteString("interface contracts, adjust work based on corrections, or acknowledge status updates.\n\n")
}

func writeSendingInstructions(b *strings.Builder, inboxFolder, messagesPath string) {
	b.WriteString("### Sending a message\n")
	b.WriteString("Write JSON to the recipient's inbox using atomic writes (write .tmp then mv):\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(b, "cat > %s/<recipient-folder>/inbox/<id>.json.tmp << 'MSGEOF'\n", messagesPath)
	b.WriteString("{\n")
	fmt.Fprintf(b, "  \"id\": \"<unix_ms>-%s-<type>\",\n", inboxFolder)
	fmt.Fprintf(b, "  \"sender\": \"%s\",\n", inboxFolder)
	b.WriteString("  \"recipient\": \"<recipient-folder>\",\n")
	b.WriteString("  \"type\": \"<question|answer|interface-contract|status-update|blocking-issue>\",\n")
	b.WriteString("  \"subject\": \"...\",\n")
	b.WriteString("  \"content\": \"...\",\n")
	b.WriteString("  \"timestamp\": \"<ISO8601>\",\n")
	b.WriteString("  \"read\": false\n")
	b.WriteString("}\n")
	b.WriteString("MSGEOF\n")
	fmt.Fprintf(b, "mv %s/<recipient-folder>/inbox/<id>.json.tmp %s/<recipient-folder>/inbox/<id>.json\n", messagesPath, messagesPath)
	b.WriteString("```\n\n")
}

func writeInstructions(b *strings.Builder, hasMembers, hasMessageBus bool) {
	b.WriteString("## Instructions\n")
	if hasMembers {
		if hasMessageBus {
			b.WriteString(`1. Start your /loop inbox monitor (see Message Bus section above)
2. Use TeamCreate to create your team and assign tasks to teammates based on
   their focus areas. Give each teammate a detailed prompt — include technical
   context, specific tasks with verify commands, and relevant upstream results.
   They cannot see your conversation, so the prompt is ALL they get.
   IMPORTANT: Do NOT include Message Bus, inbox, or /loop instructions in teammate
   prompts. Teammates must NOT poll the message bus. Only YOU (the team lead)
   communicate via the message bus.
3. When your inbox monitor finds messages relevant to a teammate's work (questions,
   corrections, interface contracts), relay that information to the teammate via
   SendMessage. You are the single point of contact between your team and the
   message bus.
4. As results come back, run each task's verify command yourself to confirm
5. If a verify fails, use SendMessage to give the teammate specific feedback and
   have them fix it
6. When all tasks pass verification, provide your summary
7. IMPORTANT: When you are completely done, cancel your /loop inbox monitor
   using CronDelete with the job ID from step 1. This allows your session to exit cleanly.
`)
			return
		}
		b.WriteString(`1. Use TeamCreate to create your team and assign tasks to teammates based on
   their focus areas. Give each teammate a detailed prompt — include technical
   context, specific tasks with verify commands, and relevant upstream results.
   They cannot see your conversation, so the prompt is ALL they get.
2. As results come back, run each task's verify command yourself to confirm
3. If a verify fails, give the teammate specific feedback and have them fix it
4. When all tasks pass verification, provide your summary
`)
	} else {
		b.WriteString(`Work through your tasks in order. After completing each task, run its
verify command to confirm it works. When all tasks are done, provide a
brief summary of what you accomplished and list all files created/modified.
`)
	}
}
