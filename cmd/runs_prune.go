package cmd

import (
	"context"
	"fmt"
	"time"

	agentservice "github.com/itsHabib/orchestra/internal/agents"
)

func runRunsPrune(ctx context.Context) error {
	runRecords, err := loadRunRecords(runsWorkspaceFlag)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	refs := collectRunAgentRefs(runRecords, now, runsPruneOlder)

	st := newAgentStore(runsWorkspaceFlag)
	records, err := st.ListAgents(ctx)
	if err != nil {
		return err
	}
	if len(records) == 0 && !runsReconcile {
		fmt.Println("No stale cached agents eligible for workflow prune.")
		return nil
	}

	svc, err := newAgentServiceFromStore(st)
	if err != nil {
		return err
	}
	report, err := svc.Prune(ctx, agentservice.PruneOpts{
		Apply:   runsPruneApply,
		MaxAge:  runsPruneOlder,
		Protect: protectRunAgentRefs(refs),
		Now:     now,
	})
	if err != nil {
		return err
	}
	printStaleAgentReport(report, runsPruneApply, "No stale cached agents eligible for workflow prune.", "cache record ")
	if runsReconcile {
		orphaned, err := svc.Orphans(ctx, excludeRunAgentRefs(refs))
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

func protectRunAgentRefs(refs runAgentRefs) func(key, agentID string) bool {
	return func(_ string, agentID string) bool {
		_, protected := refs.protectedAgentIDs[agentID]
		return protected
	}
}

func excludeRunAgentRefs(refs runAgentRefs) func(key, agentID string) bool {
	return func(_ string, agentID string) bool {
		_, referenced := refs.allAgentIDs[agentID]
		return referenced
	}
}

func printRunOrphanAgents(orphaned []orphanAgent) {
	if len(orphaned) == 0 {
		fmt.Println("No orchestra-tagged MA agents are missing from known workflow runs.")
		return
	}
	fmt.Println("Orchestra-tagged MA agents not referenced by known workflow runs:")
	fmt.Printf("%-32s  %-28s  %-7s  %s\n", "KEY", "AGENT ID", "VERSION", "MA STATUS")
	for _, agent := range orphaned {
		fmt.Printf("%-32s  %-28s  %-7d  %s\n", agent.Key, agent.AgentID, agent.Version, agent.Status)
	}
}
