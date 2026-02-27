package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/michaelhabib/orchestra/internal/config"
	"github.com/michaelhabib/orchestra/internal/dag"
	"github.com/michaelhabib/orchestra/internal/injection"
	olog "github.com/michaelhabib/orchestra/internal/log"
	"github.com/michaelhabib/orchestra/internal/workspace"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var showPrompts bool
var jsonOutput bool

var planCmd = &cobra.Command{
	Use:   "plan <config.yaml>",
	Short: "Show the execution plan without running anything",
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

		tiers, err := dag.BuildTiers(cfg.Teams)
		if err != nil {
			logger.Error("DAG error: %s", err)
			os.Exit(1)
		}

		if jsonOutput {
			type jsonTeam struct {
				Name      string        `json:"name"`
				Lead      config.Lead   `json:"lead"`
				Members   []config.Member `json:"members,omitempty"`
				Tasks     []config.Task `json:"tasks"`
				DependsOn []string      `json:"depends_on,omitempty"`
				Context   string        `json:"context,omitempty"`
			}
			type jsonTier struct {
				Tier  int        `json:"tier"`
				Teams []jsonTeam `json:"teams"`
			}
			type jsonPlan struct {
				Name     string          `json:"name"`
				Defaults config.Defaults `json:"defaults"`
				Tiers    []jsonTier      `json:"tiers"`
			}

			plan := jsonPlan{
				Name:     cfg.Name,
				Defaults: cfg.Defaults,
			}
			for tierIdx, tierNames := range tiers {
				jt := jsonTier{Tier: tierIdx}
				for _, name := range tierNames {
					team := cfg.TeamByName(name)
					jt.Teams = append(jt.Teams, jsonTeam{
						Name:      team.Name,
						Lead:      team.Lead,
						Members:   team.Members,
						Tasks:     team.Tasks,
						DependsOn: team.DependsOn,
						Context:   team.Context,
					})
				}
				plan.Tiers = append(plan.Tiers, jt)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(plan); err != nil {
				logger.Error("JSON encoding error: %s", err)
				os.Exit(1)
			}
			return
		}

		bold := color.New(color.Bold)
		dim := color.New(color.Faint)

		fmt.Println()
		bold.Printf("  Project: %s\n", cfg.Name)
		fmt.Printf("  Defaults: model=%s, max_turns=%d, timeout=%dm, permission=%s\n",
			cfg.Defaults.Model, cfg.Defaults.MaxTurns, cfg.Defaults.TimeoutMinutes, cfg.Defaults.PermissionMode)
		fmt.Println()

		totalTasks := 0
		for tierIdx, tierNames := range tiers {
			parallel := ""
			if len(tierNames) > 1 {
				parallel = "  (parallel)"
			}
			bold.Printf("  Tier %d:%s\n", tierIdx, parallel)

			for _, name := range tierNames {
				team := cfg.TeamByName(name)
				kind := "solo"
				if team.HasMembers() {
					kind = fmt.Sprintf("team, %d members", len(team.Members))
				}
				model := team.Lead.Model
				taskCount := len(team.Tasks)
				totalTasks += taskCount

				fmt.Printf("    %-16s  %s, %d tasks, model: %s\n", name, kind, taskCount, model)

				if team.HasMembers() {
					for _, m := range team.Members {
						dim.Printf("      → %s: %s\n", m.Role, m.Focus)
					}
				}

				if len(team.DependsOn) > 0 {
					dim.Printf("      depends on: %v\n", team.DependsOn)
				}

				for _, task := range team.Tasks {
					dim.Printf("      • %s", task.Summary)
					if task.Verify != "" {
						dim.Printf("  [verify: %s]", task.Verify)
					}
					fmt.Println()
				}
			}
			fmt.Println()
		}

		fmt.Printf("  %d teams, %d tiers, %d tasks total\n\n", len(cfg.Teams), len(tiers), totalTasks)

		if showPrompts {
			// Build a mock state with all teams pending to show what prompts look like
			state := &workspace.State{
				Project: cfg.Name,
				Teams:   make(map[string]workspace.TeamState),
			}

			bold.Println("  ═══ Prompts ═══")
			for _, team := range cfg.Teams {
				prompt := injection.BuildPrompt(team, cfg.Name, state, cfg)
				fmt.Println()
				bold.Printf("  ─── %s ───\n", team.Name)
				fmt.Println(prompt)
			}
		}
	},
}

func init() {
	planCmd.Flags().BoolVar(&showPrompts, "show-prompts", false, "Print the full prompt that each team lead would receive")
	planCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output plan as JSON (for programmatic consumption)")
	rootCmd.AddCommand(planCmd)
}
