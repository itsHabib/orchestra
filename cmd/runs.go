package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/itsHabib/orchestra/internal/machost"
	"github.com/itsHabib/orchestra/pkg/spawner"
	"github.com/itsHabib/orchestra/pkg/store"
	"github.com/itsHabib/orchestra/pkg/store/filestore"
	"github.com/spf13/cobra"
)

const defaultRunsListLimit = 20

var (
	runsWorkspaceFlag string
	runsListLimit     int
	runsPruneApply    bool
	runsPruneOlder    time.Duration
	runsReconcile     bool
)

var runsCmd = &cobra.Command{
	Use:   "runs",
	Short: "Inspect workflow runs",
}

var runsLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List recent workflow runs",
	Run: func(_ *cobra.Command, _ []string) {
		records, err := loadRunRecords(runsWorkspaceFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runs ls: %v\n", err)
			os.Exit(1)
		}
		printRunRows(records, runsListLimit, time.Now().UTC())
	},
}

var runsShowCmd = &cobra.Command{
	Use:   "show <run-id>",
	Short: "Show teams and backend resources for a workflow run",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		records, err := loadRunRecords(runsWorkspaceFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runs show: %v\n", err)
			os.Exit(1)
		}
		record, ok := findRunRecord(records, args[0])
		if !ok {
			fmt.Fprintf(os.Stderr, "runs show: run %q not found\n", args[0])
			os.Exit(1)
		}
		printRunDetail(record, time.Now().UTC())
	},
}

var runsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Prune stale cached resources associated with workflow runs",
	Run: func(cmd *cobra.Command, _ []string) {
		if err := runRunsPrune(cmd.Context()); err != nil {
			fmt.Fprintf(os.Stderr, "runs prune: %v\n", err)
			os.Exit(1)
		}
	},
}

func init() {
	runsCmd.PersistentFlags().StringVar(&runsWorkspaceFlag, "workspace", workspaceDir, "Path to workspace directory")

	runsLsCmd.Flags().IntVar(&runsListLimit, "limit", defaultRunsListLimit, "Maximum runs to list; 0 lists all")

	runsPruneCmd.Flags().BoolVar(&runsPruneApply, "apply", false, "Delete stale cache records")
	runsPruneCmd.Flags().DurationVar(&runsPruneOlder, "older-than", defaultAgentPruneAge, "Consider cache records stale after this idle duration")
	runsPruneCmd.Flags().BoolVar(&runsReconcile, "reconcile", false, "Also list orchestra-tagged agents not referenced by any known run")

	runsCmd.AddCommand(runsLsCmd)
	runsCmd.AddCommand(runsShowCmd)
	runsCmd.AddCommand(runsPruneCmd)
}

type runRecord struct {
	id         string
	active     bool
	dir        string
	state      *store.RunState
	modifiedAt time.Time
}

type runAgentRefs struct {
	allAgentIDs       map[string]struct{}
	protectedAgentIDs map[string]struct{}
}

func loadRunRecords(workspace string) ([]runRecord, error) {
	if workspace == "" {
		workspace = workspaceDir
	}
	var records []runRecord

	active, err := readRunStateFile(filepath.Join(workspace, "state.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		records = append(records, runRecord{
			id:         runIDForState(active.state, "active"),
			active:     true,
			dir:        workspace,
			state:      active.state,
			modifiedAt: active.modifiedAt,
		})
	}

	archiveDir := filepath.Join(workspace, "archive")
	entries, err := os.ReadDir(archiveDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(archiveDir, entry.Name())
			archived, err := readRunStateFile(filepath.Join(dir, "state.json"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			records = append(records, runRecord{
				id:         runIDForState(archived.state, entry.Name()),
				dir:        dir,
				state:      archived.state,
				modifiedAt: archived.modifiedAt,
			})
		}
	}

	sortRunRecords(records)
	return records, nil
}

type runStateFile struct {
	state      *store.RunState
	modifiedAt time.Time
}

func readRunStateFile(path string) (*runStateFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state store.RunState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if state.Teams == nil {
		state.Teams = make(map[string]store.TeamState)
	}
	return &runStateFile{state: &state, modifiedAt: info.ModTime().UTC()}, nil
}

func runIDForState(state *store.RunState, fallback string) string {
	if state != nil && state.RunID != "" {
		return state.RunID
	}
	return fallback
}

func sortRunRecords(records []runRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		iStarted := records[i].startedAt()
		jStarted := records[j].startedAt()
		if !iStarted.Equal(jStarted) {
			return iStarted.After(jStarted)
		}
		if records[i].active != records[j].active {
			return records[i].active
		}
		return records[i].id > records[j].id
	})
}

func (r runRecord) startedAt() time.Time {
	if r.state != nil && !r.state.StartedAt.IsZero() {
		return r.state.StartedAt.UTC()
	}
	return r.modifiedAt.UTC()
}

func findRunRecord(records []runRecord, id string) (runRecord, bool) {
	for _, record := range records {
		if record.id == id || id == "active" && record.active {
			return record, true
		}
	}
	return runRecord{}, false
}

func printRunRows(records []runRecord, limit int, now time.Time) {
	if len(records) == 0 {
		fmt.Println("No runs found.")
		return
	}
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	fmt.Printf("%-32s  %-18s  %-10s  %-7s  %-8s  %-10s  %s\n", "RUN ID", "PROJECT", "STATUS", "TEAMS", "COST", "DURATION", "STARTED")
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
	fmt.Printf("%-18s  %-4s  %-10s  %-8s  %-10s  %-28s  %-7s  %s\n", "TEAM", "TIER", "STATUS", "COST", "DURATION", "AGENT ID", "VERSION", "SESSION ID")
	for _, name := range sortedRunTeamNames(state) {
		ts := state.Teams[name]
		fmt.Printf("%-18s  %-4s  %-10s  %-8s  %-10s  %-28s  %-7s  %s\n",
			compactCell(name, 18),
			formatTier(ts.Tier),
			firstDisplay(ts.Status, "pending"),
			formatCost(ts.CostUSD),
			formatDuration(teamDuration(ts, now)),
			firstDisplay(ts.AgentID, "-"),
			formatAgentVersion(ts.AgentVersion),
			firstDisplay(ts.SessionID, "-"),
		)
		if ts.LastError != "" {
			fmt.Printf("  error: %s\n", compactError(errors.New(ts.LastError)))
		}
	}
}

func sortedRunTeamNames(state *store.RunState) []string {
	names := make([]string, 0, len(state.Teams))
	for name := range state.Teams {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		ti := state.Teams[names[i]].Tier
		tj := state.Teams[names[j]].Tier
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
	if state == nil || len(state.Teams) == 0 {
		return "unknown"
	}
	counts := make(map[string]int)
	for _, ts := range state.Teams {
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
	case counts["done"] == len(state.Teams):
		return "done"
	case counts["terminated"] == len(state.Teams):
		return "terminated"
	default:
		return "mixed"
	}
}

func isRunTerminal(record runRecord) bool {
	if record.state == nil || len(record.state.Teams) == 0 {
		return false
	}
	for _, ts := range record.state.Teams {
		switch firstDisplay(ts.Status, "pending") {
		case "done", "failed", "terminated", "canceled", "cancelled", "skipped":
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
	for _, ts := range state.Teams {
		total += ts.CostUSD
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
		if end.IsZero() || !isRunTerminal(record) {
			end = now
		}
		if end.After(state.StartedAt) {
			return end.Sub(state.StartedAt).Round(time.Second)
		}
	}
	var maxDuration time.Duration
	for _, ts := range state.Teams {
		if ts.DurationMs <= 0 {
			continue
		}
		d := time.Duration(ts.DurationMs) * time.Millisecond
		if d > maxDuration {
			maxDuration = d
		}
	}
	return maxDuration.Round(time.Second)
}

func latestTeamEnd(state *store.RunState) time.Time {
	var end time.Time
	for _, ts := range state.Teams {
		if ts.EndedAt.After(end) {
			end = ts.EndedAt
		}
	}
	return end
}

func teamDuration(ts store.TeamState, now time.Time) time.Duration {
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
	for _, ts := range state.Teams {
		if ts.Status == "done" {
			done++
		}
	}
	return fmt.Sprintf("%d/%d", done, len(state.Teams))
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
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
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

func runRunsPrune(ctx context.Context) error {
	runRecords, err := loadRunRecords(runsWorkspaceFlag)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	refs := collectRunAgentRefs(runRecords, now, runsPruneOlder)

	st := filestore.New(runsWorkspaceFlag)
	records, err := st.ListAgents(ctx)
	if err != nil {
		return err
	}
	if len(records) == 0 && !runsReconcile {
		fmt.Println("No stale cached agents eligible for workflow prune.")
		return nil
	}

	var client anthropic.Client
	if len(records) > 0 || runsReconcile {
		client, err = machost.NewClient()
		if err != nil {
			return err
		}
	}

	var rows []agentRow
	if len(records) > 0 {
		rows = annotateAgentRows(ctx, &client, records)
	}
	stale := staleRunAgentRows(rows, refs, now, runsPruneOlder)
	if err := printOrApplyStaleRunAgents(ctx, st, stale, now); err != nil {
		return err
	}
	if runsReconcile {
		orphaned, err := listAgentsMissingFromRuns(ctx, &client, refs)
		if err != nil {
			return err
		}
		printRunOrphanAgents(orphaned)
	}
	return nil
}

func collectRunAgentRefs(records []runRecord, now time.Time, olderThan time.Duration) runAgentRefs {
	refs := runAgentRefs{
		allAgentIDs:       make(map[string]struct{}),
		protectedAgentIDs: make(map[string]struct{}),
	}
	cutoff := now.Add(-olderThan)
	for _, record := range records {
		protect := !isRunTerminal(record)
		if !protect && olderThan > 0 {
			started := record.startedAt()
			protect = started.IsZero() || started.After(cutoff)
		}
		for _, ts := range record.state.Teams {
			if ts.AgentID == "" {
				continue
			}
			refs.allAgentIDs[ts.AgentID] = struct{}{}
			if protect {
				refs.protectedAgentIDs[ts.AgentID] = struct{}{}
			}
		}
	}
	return refs
}

func staleRunAgentRows(rows []agentRow, refs runAgentRefs, now time.Time, olderThan time.Duration) []agentRow {
	var stale []agentRow
	for i := range rows {
		row := &rows[i]
		if _, protected := refs.protectedAgentIDs[row.record.AgentID]; protected {
			continue
		}
		if staleReason(row, now, olderThan) != "" {
			stale = append(stale, rows[i])
		}
	}
	return stale
}

func printOrApplyStaleRunAgents(ctx context.Context, st store.Store, stale []agentRow, now time.Time) error {
	if len(stale) == 0 {
		fmt.Println("No stale cached agents eligible for workflow prune.")
		return nil
	}

	action := "Would delete"
	if runsPruneApply {
		action = "Deleted"
	}
	for i := range stale {
		row := &stale[i]
		reason := staleReason(row, now, runsPruneOlder)
		if runsPruneApply {
			if err := st.DeleteAgent(ctx, row.record.Key); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		fmt.Printf("%s cache record %s (%s)\n", action, row.record.Key, reason)
	}
	if !runsPruneApply {
		fmt.Println("Dry run only. Re-run with --apply to delete these cache records.")
	}
	return nil
}

func listAgentsMissingFromRuns(
	ctx context.Context,
	client *anthropic.Client,
	refs runAgentRefs,
) ([]orphanAgent, error) {
	params := anthropic.BetaAgentListParams{
		Limit:           anthropic.Int(agentListPageLimit),
		IncludeArchived: anthropic.Bool(true),
	}
	var out []orphanAgent
	for pageNum := 0; pageNum < agentMaxListPages; pageNum++ {
		page, err := client.Beta.Agents.List(ctx, params)
		if err != nil {
			return nil, err
		}
		for i := range page.Data {
			agent := &page.Data[i]
			key, ok := spawner.AgentCacheKeyFromMetadata(agent.Metadata)
			if !ok {
				continue
			}
			if _, referenced := refs.allAgentIDs[agent.ID]; referenced {
				continue
			}
			status := "active"
			if !agent.ArchivedAt.IsZero() {
				status = "archived"
			}
			out = append(out, orphanAgent{
				key:     key,
				agentID: agent.ID,
				version: agent.Version,
				status:  status,
			})
		}
		if page.NextPage == "" {
			break
		}
		params.Page = param.NewOpt(page.NextPage)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key != out[j].key {
			return out[i].key < out[j].key
		}
		return out[i].agentID < out[j].agentID
	})
	return out, nil
}

func printRunOrphanAgents(orphaned []orphanAgent) {
	if len(orphaned) == 0 {
		fmt.Println("No orchestra-tagged MA agents are missing from known workflow runs.")
		return
	}
	fmt.Println("Orchestra-tagged MA agents not referenced by known workflow runs:")
	fmt.Printf("%-32s  %-28s  %-7s  %s\n", "KEY", "AGENT ID", "VERSION", "MA STATUS")
	for _, agent := range orphaned {
		fmt.Printf("%-32s  %-28s  %-7d  %s\n", agent.key, agent.agentID, agent.version, agent.status)
	}
}
