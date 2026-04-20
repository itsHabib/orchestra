package cmd

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/itsHabib/orchestra/internal/machost"
	"github.com/itsHabib/orchestra/pkg/spawner"
	"github.com/itsHabib/orchestra/pkg/store/filestore"
)

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
	if err := pruneAndPrintStale(ctx, st, stale, now, runsPruneOlder, runsPruneApply, "No stale cached agents eligible for workflow prune.", "cache record "); err != nil {
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
		protect := record.active && !isRunTerminal(record)
		if !protect && olderThan > 0 {
			started := record.startedAt()
			protect = started.IsZero() || started.After(cutoff)
		}
		for name := range record.state.Teams {
			agentID := record.state.Teams[name].AgentID
			if agentID == "" {
				continue
			}
			refs.allAgentIDs[agentID] = struct{}{}
			if protect {
				refs.protectedAgentIDs[agentID] = struct{}{}
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
