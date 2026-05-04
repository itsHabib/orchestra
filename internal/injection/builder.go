package injection

import (
	"fmt"
	"strings"

	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/workspace"
)

// Capabilities carries optional rendering toggles for sections that are not
// implicit in BuildPrompt's other arguments. Today it only signals
// repository-backed artifact delivery for managed-agents teams; local-backend
// callers pass the zero value.
type Capabilities struct {
	ArtifactPublish *ArtifactPublishSpec
}

// ArtifactPublishSpec instructs the team to commit and push to a specific
// branch and lists any upstream branches mounted read-only under the session.
type ArtifactPublishSpec struct {
	MountPath      string
	BranchName     string
	UpstreamMounts []UpstreamMount
}

// UpstreamMount is a single upstream team's branch made available read-only.
type UpstreamMount struct {
	TeamName  string
	MountPath string
	Branch    string
}

// BuildPrompt constructs the full prompt for a team's claude -p session.
// tierPeers is the list of all team names in the same tier (including
// self); pass nil for single-team spawns. caps carries optional toggles
// like ArtifactPublish; pass Capabilities{} for the local backend.
//
// The cross-team file message bus was removed in v3 phase A. The
// chat-side LLM (or downstream Phase B recipes) now drives inter-team
// composition via [steer] and signal_completion(artifacts={...}); the
// per-agent prompt no longer mentions an inbox.
func BuildPrompt(team *config.Agent, projectName string, state *workspace.State, cfg *config.Config, tierPeers []string, caps Capabilities) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are: %s\n", team.Lead.Role)
	fmt.Fprintf(&b, "Project: %s\n", projectName)

	writeTechnicalContext(&b, team.Context)
	writeTasks(&b, team.Tasks)
	writeTierPeers(&b, team, cfg, tierPeers)
	writeTeamMembers(&b, team)
	writeDependencyContext(&b, team, state, cfg)
	writeArtifactPublish(&b, caps.ArtifactPublish)
	writeInstructions(&b, team.HasMembers())

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

func writeTierPeers(b *strings.Builder, team *config.Agent, cfg *config.Config, tierPeers []string) {
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
		pt := cfg.AgentByName(p)
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

func writeTeamMembers(b *strings.Builder, team *config.Agent) {
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

func writeDependencyContext(b *strings.Builder, team *config.Agent, state *workspace.State, cfg *config.Config) {
	if len(team.DependsOn) == 0 || state == nil {
		return
	}

	b.WriteString("## Context from Previous Teams\n")
	for _, depName := range team.DependsOn {
		ts, ok := state.Agents[depName]
		if !ok || ts.Status != "done" {
			continue
		}
		depRole := depName
		if depTeam := cfg.AgentByName(depName); depTeam != nil {
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

func writeArtifactPublish(b *strings.Builder, spec *ArtifactPublishSpec) {
	if spec == nil || spec.BranchName == "" || spec.MountPath == "" {
		return
	}
	b.WriteString("## Artifact delivery\n")
	fmt.Fprintf(b, "Your working copy of the repo is at `%s`.\n\n", spec.MountPath)
	b.WriteString("When your task is complete:\n")
	fmt.Fprintf(b, "  1. Commit your changes on a new branch named `%s`.\n", spec.BranchName)
	b.WriteString("  2. Push the branch to origin.\n")
	b.WriteString("  3. Do NOT open a pull request; do NOT merge.\n\n")
	if len(spec.UpstreamMounts) > 0 {
		b.WriteString("Your upstream dependencies are mounted read-only at:\n")
		for _, m := range spec.UpstreamMounts {
			fmt.Fprintf(b, "  - %s: `%s` (branch `%s`)\n", m.TeamName, m.MountPath, m.Branch)
		}
		b.WriteString("\n")
	}
}

func writeInstructions(b *strings.Builder, hasMembers bool) {
	b.WriteString("## Instructions\n")
	if hasMembers {
		b.WriteString(`1. Use TeamCreate to create your team and assign tasks to teammates based on
   their focus areas. Give each teammate a detailed prompt — include technical
   context, specific tasks with verify commands, and relevant upstream results.
   They cannot see your conversation, so the prompt is ALL they get.
2. As results come back, run each task's verify command yourself to confirm
3. If a verify fails, give the teammate specific feedback and have them fix it
4. When all tasks pass verification, provide your summary
`)
		return
	}
	b.WriteString(`Work through your tasks in order. After completing each task, run its
verify command to confirm it works. When all tasks are done, provide a
brief summary of what you accomplished and list all files created/modified.
`)
}
