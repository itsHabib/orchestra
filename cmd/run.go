package cmd

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/itsHabib/orchestra/internal/config"
	"github.com/itsHabib/orchestra/internal/dag"
	"github.com/itsHabib/orchestra/internal/injection"
	olog "github.com/itsHabib/orchestra/internal/log"
	"github.com/itsHabib/orchestra/internal/messaging"
	"github.com/itsHabib/orchestra/internal/workspace"
	"github.com/itsHabib/orchestra/pkg/spawner"
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

	// 3. Init message bus
	teamNames := make([]string, len(cfg.Teams))
	for i, t := range cfg.Teams {
		teamNames[i] = t.Name
	}
	bus := messaging.NewBus(ws.MessagesPath())
	if err := bus.InitInboxes(teamNames); err != nil {
		return fmt.Errorf("init message bus: %w", err)
	}
	participants := messaging.BuildParticipants(teamNames)

	// Build lookup: team name → inbox folder name
	inboxLookup := make(map[string]string)
	for _, p := range participants {
		inboxLookup[p.Name] = p.FolderName()
	}
	logger.Success("Message bus initialized at %s", bus.Path())

	// Create coordinator decisions directory
	os.MkdirAll(fmt.Sprintf("%s/coordinator", ws.Path), 0o755)

	// 4. Spawn coordinator in background (if enabled)
	var coordHandle *spawner.CoordinatorHandle
	if cfg.Coordinator.Enabled {
		coordPrompt := injection.BuildCoordinatorPrompt(cfg, tiers, bus.Path(), participants)
		coordLogWriter, _ := ws.LogWriter("coordinator")

		coordHandle, err = spawner.SpawnBackground(ctx, spawner.SpawnOpts{
			TeamName:       "coordinator",
			Prompt:         coordPrompt,
			Model:          cfg.Coordinator.Model,
			MaxTurns:       cfg.Coordinator.MaxTurns,
			PermissionMode: cfg.Defaults.PermissionMode,
			TimeoutMinutes: cfg.Defaults.TimeoutMinutes * len(tiers),
			LogWriter:      coordLogWriter,
			ProgressFunc:   func(team, msg string) { logger.TeamMsg(team, msg) },
		})
		if err != nil {
			logger.Warn("Coordinator spawn failed (continuing without): %s", err)
		} else {
			logger.Success("Coordinator agent spawned")
		}
	}

	// 5. Execute tier by tier
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

				// Seed inbox with bootstrap messages from completed dependencies
				if len(team.DependsOn) > 0 {
					for _, dep := range team.DependsOn {
						depResult, err := ws.ReadResult(dep)
						if err != nil || depResult == nil {
							continue
						}
						summary := depResult.Result
						if summary == "" {
							if ts, ok := state.Teams[dep]; ok {
								summary = ts.ResultSummary
							}
						}
						if summary == "" {
							continue
						}
						bus.Send(&messaging.Message{
							ID:        fmt.Sprintf("bootstrap-%s-to-%s", dep, teamName),
							Sender:    "orchestrator",
							Recipient: inboxLookup[teamName],
							Type:      messaging.MsgBootstrap,
							Subject:   fmt.Sprintf("Results from %s (completed)", dep),
							Content:   summary,
							Timestamp: time.Now(),
							Read:      false,
						})
					}
				}

				// Compute peer names (other teams in this tier)
				var peers []string
				if len(tierNames) > 1 {
					peers = tierNames
				}

				// Resolve this team's inbox folder
				teamInbox := inboxLookup[teamName]

				prompt := injection.BuildPrompt(*team, cfg.Name, state, cfg, peers, teamInbox, bus.Path())
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

			logger.TeamMsg(r.name, "Done (turns: %d, %s in / %s out)", r.res.NumTurns, fmtTokens(r.res.InputTokens), fmtTokens(r.res.OutputTokens))

			ws.WriteResult(r.res)
			ws.UpdateTeamState(r.name, workspace.TeamState{
				Status:        "done",
				ResultSummary: r.res.Result,
				CostUSD:       r.res.CostUSD,
				DurationMs:    r.res.DurationMs,
				InputTokens:   r.res.InputTokens,
				OutputTokens:  r.res.OutputTokens,
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

	// 6. Stop coordinator
	if coordHandle != nil {
		logger.Info("Signaling coordinator to stop...")
		coordHandle.Cancel()
		coordResult, coordErr := coordHandle.Wait()
		if coordErr != nil {
			logger.Warn("Coordinator exited with error: %s", coordErr)
		} else if coordResult != nil {
			logger.TeamMsg("coordinator", "Done (cost: $%.2f, turns: %d)", coordResult.CostUSD, coordResult.NumTurns)
			ws.WriteResult(coordResult)
		}
	}

	// 7. Print summary
	printSummary(ws, cfg, time.Since(wallStart))
	return nil
}

// fmtTokens formats a token count as a human-readable string (e.g. "284K", "1.2M").
func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dK", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
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

	fmt.Printf("  %-16s │ %-8s │ %-12s │ %-6s │ %-10s\n", "Team", "Status", "Tokens", "Turns", "Duration")
	fmt.Printf("  ────────────────┼──────────┼──────────────┼────────┼────────────\n")

	var totalIn, totalOut int64
	var totalTurns int
	for _, entry := range reg.Teams {
		ts := state.Teams[entry.Name]
		tokens := ""
		turns := ""
		dur := ""
		if ts.InputTokens > 0 || ts.OutputTokens > 0 {
			tokens = fmt.Sprintf("%s→%s", fmtTokens(ts.InputTokens), fmtTokens(ts.OutputTokens))
			totalIn += ts.InputTokens
			totalOut += ts.OutputTokens
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
		fmt.Printf("  %-16s │ %-8s │ %-12s │ %-6s │ %s\n", entry.Name, ts.Status, tokens, turns, dur)
	}

	fmt.Printf("  ────────────────┼──────────┼──────────────┼────────┼────────────\n")
	fmt.Printf("  %-16s │          │ %s→%-7s │ %-6d │\n", "Total", fmtTokens(totalIn), fmtTokens(totalOut), totalTurns)
	fmt.Println()

	wc := wallClock.Round(time.Second)
	fmt.Printf("  Wall clock: %dm %02ds\n", int(wc.Minutes()), int(wc.Seconds())%60)
	fmt.Printf("  Results:    %s/results/\n", ws.Path)
	fmt.Printf("  Logs:       %s/logs/\n", ws.Path)
	fmt.Println()
}
