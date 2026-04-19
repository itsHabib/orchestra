package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/itsHabib/orchestra/internal/machost"
	"github.com/itsHabib/orchestra/pkg/store"
	"github.com/itsHabib/orchestra/pkg/store/filestore"
	"github.com/spf13/cobra"
)

const (
	defaultAgentPruneAge = 30 * 24 * time.Hour
	agentStatusWorkers   = 5
	agentListPageLimit   = 100
	agentMaxListPages    = 10
)

var (
	agentsWorkspaceFlag string
	agentsPruneApply    bool
	agentsPruneOlder    time.Duration
	agentsReconcile     bool
)

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Manage the user-scoped Managed Agents cache",
}

var agentsLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List cached Managed Agents",
	Run: func(cmd *cobra.Command, _ []string) {
		ctx := cmd.Context()
		rows, err := loadAgentRows(ctx, agentsWorkspaceFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agents ls: %v\n", err)
			os.Exit(1)
		}
		printAgentRows(rows)
	},
}

var agentsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Prune stale Managed Agents cache records",
	Run: func(cmd *cobra.Command, _ []string) {
		ctx := cmd.Context()
		if err := runAgentsPrune(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "agents prune: %v\n", err)
			os.Exit(1)
		}
	},
}

func init() {
	agentsCmd.PersistentFlags().StringVar(&agentsWorkspaceFlag, "workspace", workspaceDir, "Path to workspace directory")

	agentsPruneCmd.Flags().BoolVar(&agentsPruneApply, "apply", false, "Delete stale cache records")
	agentsPruneCmd.Flags().DurationVar(&agentsPruneOlder, "older-than", defaultAgentPruneAge, "Prune records not used within this duration")
	agentsPruneCmd.Flags().BoolVar(&agentsReconcile, "reconcile", false, "Also list orchestra-tagged agents missing from the cache")

	agentsCmd.AddCommand(agentsLsCmd)
	agentsCmd.AddCommand(agentsPruneCmd)
}

type agentRow struct {
	record store.AgentRecord
	status string
	err    error
}

type orphanAgent struct {
	key     string
	agentID string
	version int64
	status  string
}

func loadAgentRows(ctx context.Context, workspace string) ([]agentRow, error) {
	st := filestore.New(workspace)
	records, err := st.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	client, err := machost.NewClient()
	if err != nil {
		return nil, err
	}
	return annotateAgentRows(ctx, &client, records), nil
}

func annotateAgentRows(ctx context.Context, client *anthropic.Client, records []store.AgentRecord) []agentRow {
	rows := make([]agentRow, len(records))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := min(agentStatusWorkers, max(1, len(records)))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				rec := records[idx]
				status, err := getAgentStatus(ctx, client, rec.AgentID)
				rows[idx] = agentRow{record: rec, status: status, err: err}
			}
		}()
	}
	for i := range records {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return rows
}

func getAgentStatus(ctx context.Context, client *anthropic.Client, agentID string) (string, error) {
	agent, err := client.Beta.Agents.Get(ctx, agentID, anthropic.BetaAgentGetParams{})
	switch {
	case isAnthropicStatus(err, http.StatusNotFound):
		return "missing", nil
	case err != nil:
		return "unreachable", err
	case !agent.ArchivedAt.IsZero():
		return "archived", nil
	default:
		return "active", nil
	}
}

func printAgentRows(rows []agentRow) {
	fmt.Printf("%-32s  %-28s  %-7s  %-16s  %s\n", "KEY", "AGENT ID", "VERSION", "LAST USED", "MA STATUS")
	for i := range rows {
		row := &rows[i]
		status := row.status
		if row.err != nil {
			status = status + " (" + compactError(row.err) + ")"
		}
		fmt.Printf("%-32s  %-28s  %-7d  %-16s  %s\n",
			row.record.Key,
			row.record.AgentID,
			row.record.Version,
			formatCacheTime(row.record.LastUsed),
			status,
		)
	}
}

func runAgentsPrune(ctx context.Context) error {
	st := filestore.New(agentsWorkspaceFlag)
	records, err := st.ListAgents(ctx)
	if err != nil {
		return err
	}
	client, err := machost.NewClient()
	if err != nil {
		return err
	}
	rows := annotateAgentRows(ctx, &client, records)
	now := time.Now().UTC()
	stale := staleAgentRows(rows, now, agentsPruneOlder)

	if err := printOrApplyStaleAgents(ctx, st, stale, now); err != nil {
		return err
	}

	if agentsReconcile {
		orphaned, err := listOrphanAgents(ctx, &client, records)
		if err != nil {
			return err
		}
		printOrphanAgents(orphaned)
	}
	return nil
}

func printOrApplyStaleAgents(ctx context.Context, st store.Store, stale []agentRow, now time.Time) error {
	if len(stale) == 0 {
		fmt.Println("No stale cached agents.")
		return nil
	}

	action := "Would delete"
	if agentsPruneApply {
		action = "Deleted"
	}
	for i := range stale {
		row := &stale[i]
		reason := staleReason(row, now, agentsPruneOlder)
		if agentsPruneApply {
			if err := st.DeleteAgent(ctx, row.record.Key); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		fmt.Printf("%s %s (%s)\n", action, row.record.Key, reason)
	}
	if !agentsPruneApply {
		fmt.Println("Dry run only. Re-run with --apply to delete these cache records.")
	}
	return nil
}

func staleAgentRows(rows []agentRow, now time.Time, olderThan time.Duration) []agentRow {
	var out []agentRow
	for i := range rows {
		if staleReason(&rows[i], now, olderThan) != "" {
			out = append(out, rows[i])
		}
	}
	return out
}

func staleReason(row *agentRow, now time.Time, olderThan time.Duration) string {
	switch row.status {
	case "missing":
		return "MA 404"
	case "archived":
		return "archived on MA"
	}
	if olderThan > 0 && (row.record.LastUsed.IsZero() || row.record.LastUsed.Before(now.Add(-olderThan))) {
		return "last used older than " + olderThan.String()
	}
	return ""
}

func listOrphanAgents(ctx context.Context, client *anthropic.Client, records []store.AgentRecord) ([]orphanAgent, error) {
	cached := make(map[string]string, len(records))
	for i := range records {
		rec := &records[i]
		cached[rec.Key] = rec.AgentID
	}

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
			key, ok := agentCacheKeyFromMetadata(agent.Metadata)
			if !ok {
				continue
			}
			if cached[key] == agent.ID {
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
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

func printOrphanAgents(orphaned []orphanAgent) {
	if len(orphaned) == 0 {
		fmt.Println("No orchestra-tagged MA agents are missing from the cache.")
		return
	}
	fmt.Println("Orchestra-tagged MA agents missing from cache:")
	fmt.Printf("%-32s  %-28s  %-7s  %s\n", "KEY", "AGENT ID", "VERSION", "MA STATUS")
	for _, agent := range orphaned {
		fmt.Printf("%-32s  %-28s  %-7d  %s\n", agent.key, agent.agentID, agent.version, agent.status)
	}
}

func agentCacheKeyFromMetadata(metadata map[string]string) (string, bool) {
	if metadata["orchestra_version"] != "v2" {
		return "", false
	}
	project := metadata["orchestra_project"]
	role := metadata["orchestra_role"]
	if project == "" || role == "" {
		return "", false
	}
	return project + "__" + role, true
}

func isAnthropicStatus(err error, code int) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

func formatCacheTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func compactError(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(msg) <= 80 {
		return msg
	}
	return msg[:77] + "..."
}
