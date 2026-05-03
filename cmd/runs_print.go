package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/itsHabib/orchestra/internal/store"
)

func printRunRows(records []runRecord, limit int, now time.Time) {
	if len(records) == 0 {
		fmt.Println("No runs found.")
		return
	}
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	fmt.Printf("%-32s  %-18s  %-10s  %-7s  %-8s  %-10s  %s\n", "RUN ID", "PROJECT", "STATUS", "AGENTS", "COST", "DURATION", "STARTED")
	for _, record := range records {
		fmt.Printf("%-32s  %-18s  %-10s  %-7s  %-8s  %-10s  %s\n",
			runIDLabel(record),
			compactCell(record.state.Project, 18),
			aggregateRunStatus(record.state),
			teamCountLabel(record.state),
			formatCost(totalRunCost(record.state)),
			formatDuration(runDuration(record, now)),
			formatRunTime(record.startedAt()),
		)
	}
}

func runIDLabel(record runRecord) string {
	if record.active {
		return compactCell(record.id+" (active)", 32)
	}
	return compactCell(record.id, 32)
}

func printRunDetail(record runRecord, now time.Time) {
	state := record.state
	fmt.Printf("Run:      %s\n", record.id)
	fmt.Printf("Project:  %s\n", state.Project)
	fmt.Printf("Status:   %s\n", aggregateRunStatus(state))
	fmt.Printf("Backend:  %s\n", firstDisplay(state.Backend, "local"))
	fmt.Printf("Started:  %s\n", formatRunTime(record.startedAt()))
	fmt.Printf("Duration: %s\n", formatDuration(runDuration(record, now)))
	fmt.Printf("Cost:     %s\n", formatCost(totalRunCost(state)))
	fmt.Printf("Path:     %s\n", record.dir)
	if state.EnvironmentID != "" {
		fmt.Printf("Env ID:   %s\n", state.EnvironmentID)
	}
	fmt.Println()
	fmt.Printf("%-18s  %-4s  %-10s  %-8s  %-10s  %-28s  %-7s  %s\n", "AGENT", "TIER", "STATUS", "COST", "DURATION", "AGENT ID", "VERSION", "SESSION ID")
	for _, name := range sortedRunTeamNames(state) {
		ts := state.Agents[name]
		fmt.Printf("%-18s  %-4s  %-10s  %-8s  %-10s  %-28s  %-7s  %s\n",
			compactCell(name, 18),
			formatTier(ts.Tier),
			firstDisplay(ts.Status, "pending"),
			formatCost(ts.CostUSD),
			formatDuration(teamDuration(&ts, now)),
			firstDisplay(ts.AgentID, "-"),
			formatAgentVersion(ts.AgentVersion),
			firstDisplay(ts.SessionID, "-"),
		)
		if ts.LastError != "" {
			fmt.Printf("  error: %s\n", compactError(errors.New(ts.LastError)))
		}
		printRepositoryArtifacts(ts.RepositoryArtifacts)
	}
}

func printRepositoryArtifacts(artifacts []store.RepositoryArtifact) {
	for _, a := range artifacts {
		extras := "branch " + a.Branch + " (" + shortSHA(a.CommitSHA) + ")"
		if a.PullRequestURL != "" {
			extras += " pr " + a.PullRequestURL
		}
		fmt.Printf("  artifact: %s\n", extras)
	}
}

func shortSHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}

func sortedRunTeamNames(state *store.RunState) []string {
	names := make([]string, 0, len(state.Agents))
	for name := range state.Agents {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		ti := state.Agents[names[i]].Tier
		tj := state.Agents[names[j]].Tier
		if tierSortValue(ti) != tierSortValue(tj) {
			return tierSortValue(ti) < tierSortValue(tj)
		}
		return names[i] < names[j]
	})
	return names
}

func tierSortValue(tier *int) int {
	if tier == nil {
		return 1 << 30
	}
	return *tier
}

func aggregateRunStatus(state *store.RunState) string {
	if state == nil || len(state.Agents) == 0 {
		return "unknown"
	}
	counts := make(map[string]int)
	for name := range state.Agents {
		ts := state.Agents[name]
		status := firstDisplay(ts.Status, "pending")
		counts[status]++
	}
	switch {
	case counts["failed"] > 0:
		return "failed"
	case counts["stalled"] > 0:
		return "stalled"
	case counts["running"] > 0 || counts["idle"] > 0 || counts["rescheduling"] > 0:
		return "running"
	case counts["pending"] > 0:
		return "pending"
	case counts["done"] == len(state.Agents):
		return "done"
	case counts["terminated"] == len(state.Agents):
		return "terminated"
	default:
		return "mixed"
	}
}

func isRunTerminal(record runRecord) bool {
	if record.state == nil || len(record.state.Agents) == 0 {
		return false
	}
	for name := range record.state.Agents {
		ts := record.state.Agents[name]
		switch firstDisplay(ts.Status, "pending") {
		case "done", "failed", "terminated", "canceled", "skipped":
		default:
			return false
		}
	}
	return true
}

func totalRunCost(state *store.RunState) float64 {
	var total float64
	if state == nil {
		return total
	}
	for name := range state.Agents {
		total += state.Agents[name].CostUSD
	}
	return total
}

func runDuration(record runRecord, now time.Time) time.Duration {
	state := record.state
	if state == nil {
		return 0
	}
	if !state.StartedAt.IsZero() {
		end := latestTeamEnd(state)
		switch {
		case record.active && (end.IsZero() || !isRunTerminal(record)):
			end = now
		case !record.active && end.IsZero():
			end = record.modifiedAt
		}
		if end.After(state.StartedAt) {
			return end.Sub(state.StartedAt).Round(time.Second)
		}
	}
	var maxDuration time.Duration
	for name := range state.Agents {
		dur := state.Agents[name].DurationMs
		if dur <= 0 {
			continue
		}
		d := time.Duration(dur) * time.Millisecond
		if d > maxDuration {
			maxDuration = d
		}
	}
	return maxDuration.Round(time.Second)
}

func latestTeamEnd(state *store.RunState) time.Time {
	var end time.Time
	for name := range state.Agents {
		ended := state.Agents[name].EndedAt
		if ended.After(end) {
			end = ended
		}
	}
	return end
}

func teamDuration(ts *store.AgentState, now time.Time) time.Duration {
	if ts.DurationMs > 0 {
		return (time.Duration(ts.DurationMs) * time.Millisecond).Round(time.Second)
	}
	if ts.Status == "running" && !ts.StartedAt.IsZero() {
		return now.Sub(ts.StartedAt).Round(time.Second)
	}
	if !ts.StartedAt.IsZero() && !ts.EndedAt.IsZero() && ts.EndedAt.After(ts.StartedAt) {
		return ts.EndedAt.Sub(ts.StartedAt).Round(time.Second)
	}
	return 0
}

func teamCountLabel(state *store.RunState) string {
	if state == nil {
		return "0"
	}
	done := 0
	for name := range state.Agents {
		if state.Agents[name].Status == "done" {
			done++
		}
	}
	return fmt.Sprintf("%d/%d", done, len(state.Agents))
}

func formatTier(tier *int) string {
	if tier == nil {
		return "-"
	}
	return strconv.Itoa(*tier)
}

func formatAgentVersion(version int) string {
	if version == 0 {
		return "-"
	}
	return strconv.Itoa(version)
}

func formatCost(cost float64) string {
	if cost <= 0 {
		return "-"
	}
	return fmt.Sprintf("$%.2f", cost)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d <= 0 {
		return "-"
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, mins)
	}
	return fmt.Sprintf("%dm%02ds", mins, secs)
}

func formatRunTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func compactCell(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 3 {
		return s[:width]
	}
	return s[:width-3] + "..."
}

func firstDisplay(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
