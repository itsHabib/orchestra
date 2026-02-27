package cmd

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/michaelhabib/orchestra/internal/config"
	"github.com/michaelhabib/orchestra/internal/dag"
	"github.com/michaelhabib/orchestra/internal/injection"
	olog "github.com/michaelhabib/orchestra/internal/log"
	"github.com/michaelhabib/orchestra/internal/spawner"
	"github.com/michaelhabib/orchestra/internal/workspace"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <config.yaml>",
	Short: "Full orchestration: init, DAG, spawn tiers, collect, summary",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		logger := olog.New()

		cfg, warnings, err := config.Load(args[0])
		if err != nil {
			logger.Error("Config error: %s", err)
			os.Exit(1)
		}
		for _, w := range warnings {
			logger.Warn("%s", w)
		}

		if err := runOrchestration(context.Background(), cfg, logger); err != nil {
			logger.Error("Orchestration failed: %s", err)
			os.Exit(1)
		}
	},
}

func runOrchestration(ctx context.Context, cfg *config.Config, logger *olog.Logger) error {
	wallStart := time.Now()

	// 1. Init workspace
	ws, err := workspace.Init(cfg)
	if err != nil {
		return fmt.Errorf("init workspace: %w", err)
	}
	logger.Success("Workspace initialized at %s", ws.Path)

	// 2. Build DAG
	tiers, err := dag.BuildTiers(cfg.Teams)
	if err != nil {
		return fmt.Errorf("building DAG: %w", err)
	}
	logger.Info("DAG: %d tiers", len(tiers))

	// 3. Execute tier by tier
	for tierIdx, tierNames := range tiers {
		logger.TierStart(tierIdx, tierNames)

		state, err := ws.ReadState()
		if err != nil {
			return fmt.Errorf("reading state: %w", err)
		}

		type tierResult struct {
			name string
			res  *workspace.TeamResult
			err  error
		}
		results := make(chan tierResult, len(tierNames))
		var wg sync.WaitGroup

		for _, name := range tierNames {
			wg.Add(1)
			go func(teamName string) {
				defer wg.Done()
				team := cfg.TeamByName(teamName)

				// Update registry → running
				ws.UpdateRegistryEntry(teamName, func(e *workspace.RegistryEntry) {
					e.Status = "running"
					e.StartedAt = time.Now()
				})

				logger.TeamMsg(teamName, "Starting %s", team.Lead.Role)

				// Compute peer names (other teams in this tier)
				var peers []string
				if len(tierNames) > 1 {
					peers = tierNames
				}

				prompt := injection.BuildPrompt(*team, cfg.Name, state, cfg, peers)
				logWriter, _ := ws.LogWriter(teamName)
				defer logWriter.Close()

				model := team.Lead.Model
				if model == "" {
					model = cfg.Defaults.Model
				}

				res, err := spawner.Spawn(ctx, spawner.SpawnOpts{
					TeamName:       teamName,
					Prompt:         prompt,
					Model:          model,
					MaxTurns:       cfg.Defaults.MaxTurns,
					PermissionMode: cfg.Defaults.PermissionMode,
					TimeoutMinutes: cfg.Defaults.TimeoutMinutes,
					LogWriter:      logWriter,
				ProgressFunc:   func(team, msg string) { logger.TeamMsg(team, msg) },
				})

				results <- tierResult{teamName, res, err}
			}(name)
		}

		wg.Wait()
		close(results)

		var failed []string
		for r := range results {
			if r.err != nil {
				failed = append(failed, r.name)
				logger.TeamMsg(r.name, "FAILED: %s", r.err)
				ws.UpdateTeamState(r.name, workspace.TeamState{Status: "failed"})
				ws.UpdateRegistryEntry(r.name, func(e *workspace.RegistryEntry) {
					e.Status = "failed"
					e.EndedAt = time.Now()
				})
				continue
			}

			logger.TeamMsg(r.name, "Done (cost: $%.2f, turns: %d)", r.res.CostUSD, r.res.NumTurns)

			ws.WriteResult(r.res)
			ws.UpdateTeamState(r.name, workspace.TeamState{
				Status:        "done",
				ResultSummary: r.res.Result,
				CostUSD:       r.res.CostUSD,
				DurationMs:    r.res.DurationMs,
			})
			ws.UpdateRegistryEntry(r.name, func(e *workspace.RegistryEntry) {
				e.Status = "done"
				e.SessionID = r.res.SessionID
				e.EndedAt = time.Now()
			})
		}

		if len(failed) > 0 {
			return fmt.Errorf("tier %d: teams failed: %v", tierIdx, failed)
		}
	}

	// 4. Print summary
	printSummary(ws, cfg, time.Since(wallStart))
	return nil
}

func printSummary(ws *workspace.Workspace, cfg *config.Config, wallClock time.Duration) {
	state, err := ws.ReadState()
	if err != nil {
		return
	}
	reg, err := ws.ReadRegistry()
	if err != nil {
		return
	}

	bold := color.New(color.Bold)
	fmt.Println()
	bold.Println("═══════════════════════════════════════════════════")
	bold.Printf("  Orchestra: %s — Complete\n", state.Project)
	bold.Println("═══════════════════════════════════════════════════")
	fmt.Println()

	fmt.Printf("  %-16s │ %-8s │ %-8s │ %-6s │ %-10s\n", "Team", "Status", "Cost", "Turns", "Duration")
	fmt.Printf("  ────────────────┼──────────┼──────────┼────────┼────────────\n")

	var totalCost float64
	var totalTurns int
	for _, entry := range reg.Teams {
		ts := state.Teams[entry.Name]
		cost := ""
		turns := ""
		dur := ""
		if ts.CostUSD > 0 {
			cost = fmt.Sprintf("$%.2f", ts.CostUSD)
			totalCost += ts.CostUSD
		}
		// Read result for turns
		if res, err := ws.ReadResult(entry.Name); err == nil {
			turns = fmt.Sprintf("%d", res.NumTurns)
			totalTurns += res.NumTurns
		}
		if ts.DurationMs > 0 {
			d := time.Duration(ts.DurationMs) * time.Millisecond
			dur = fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
		}
		fmt.Printf("  %-16s │ %-8s │ %-8s │ %-6s │ %s\n", entry.Name, ts.Status, cost, turns, dur)
	}

	fmt.Printf("  ────────────────┼──────────┼──────────┼────────┼────────────\n")
	fmt.Printf("  %-16s │          │ $%-7.2f │ %-6d │\n", "Total", totalCost, totalTurns)
	fmt.Println()

	wc := wallClock.Round(time.Second)
	fmt.Printf("  Wall clock: %dm %02ds\n", int(wc.Minutes()), int(wc.Seconds())%60)
	fmt.Printf("  Results:    %s/results/\n", ws.Path)
	fmt.Printf("  Logs:       %s/logs/\n", ws.Path)
	fmt.Println()
}
