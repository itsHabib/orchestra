package injection

import (
	"fmt"
	"strings"

	"github.com/michaelhabib/orchestra/internal/config"
	"github.com/michaelhabib/orchestra/internal/workspace"
)

// BuildPrompt constructs the full prompt for a team's claude -p session.
func BuildPrompt(team config.Team, projectName string, state *workspace.State, cfg *config.Config) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are: %s\n", team.Lead.Role)
	fmt.Fprintf(&b, "Project: %s\n", projectName)

	// Technical Context
	if team.Context != "" {
		b.WriteString("\n## Technical Context\n")
		b.WriteString(team.Context)
		if !strings.HasSuffix(team.Context, "\n") {
			b.WriteString("\n")
		}
	}

	// Tasks
	b.WriteString("\n## Your Tasks\n")
	for _, task := range team.Tasks {
		fmt.Fprintf(&b, "### Task: %s\n", task.Summary)
		if task.Details != "" {
			b.WriteString(task.Details)
			b.WriteString("\n")
		}
		if len(task.Deliverables) > 0 {
			fmt.Fprintf(&b, "Expected deliverables: %s\n", strings.Join(task.Deliverables, ", "))
		}
		if task.Verify != "" {
			fmt.Fprintf(&b, "Verify: `%s`\n", task.Verify)
		}
		b.WriteString("\n")
	}

	// Team members section (only for team leads)
	if team.HasMembers() {
		fmt.Fprintf(&b, "## Your Team\n")
		fmt.Fprintf(&b, "You have %d teammates. Assign each teammate 2-6 tasks from the list above\n", len(team.Members))
		b.WriteString("based on their focus area. Each teammate's spawn prompt MUST include:\n")
		b.WriteString("1. The full Technical Context above (they don't inherit your conversation)\n")
		b.WriteString("2. Their specific assigned tasks with details, deliverables, and verify commands\n")
		b.WriteString("3. Any relevant results from previous teams\n\n")
		b.WriteString("Teammates:\n")
		for _, m := range team.Members {
			fmt.Fprintf(&b, "- %s: %s\n", m.Role, m.Focus)
		}
		b.WriteString("\n")
	}

	// Context from previous teams
	if len(team.DependsOn) > 0 && state != nil {
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
			fmt.Fprintf(&b, "### %s (%s) — Completed\n", depName, depRole)
			fmt.Fprintf(&b, "Summary: %s\n", ts.ResultSummary)
			if len(ts.Artifacts) > 0 {
				fmt.Fprintf(&b, "Artifacts: %s\n", strings.Join(ts.Artifacts, ", "))
			}
			b.WriteString("\n")
		}
	}

	// Instructions
	b.WriteString("## Instructions\n")
	if team.HasMembers() {
		b.WriteString(`1. Use TeamCreate to create your team
2. Assign tasks to teammates based on their focus areas. Give each teammate
   a detailed spawn prompt — include technical context, specific tasks with
   verify commands, and relevant upstream results. They cannot see your
   conversation, so the prompt is ALL they get.
3. Spawn teammates in parallel using the Task tool
4. As results come back, run each task's verify command yourself to confirm
5. If a verify fails, send the teammate specific feedback and have them fix it
6. When all tasks pass verification, provide your summary
`)
	} else {
		b.WriteString(`Work through your tasks in order. After completing each task, run its
verify command to confirm it works. When all tasks are done, provide a
brief summary of what you accomplished and list all files created/modified.
`)
	}

	return b.String()
}
