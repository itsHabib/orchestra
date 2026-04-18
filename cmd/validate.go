package cmd

import (
	"fmt"
	"os"

	"github.com/itsHabib/orchestra/internal/config"
	olog "github.com/itsHabib/orchestra/internal/log"
	"github.com/spf13/cobra"
)

var validateCmd = &cobra.Command{
	Use:   "validate <config.yaml>",
	Short: "Parse and validate an orchestra config",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		logger := olog.New()

		cfg, warnings, err := config.Load(args[0])
		if err != nil {
			logger.Error("Validation failed: %s", err)
			os.Exit(1)
		}

		for _, w := range warnings {
			logger.Warn("%s", w)
		}

		logger.Success("Config is valid: %d teams, project %q", len(cfg.Teams), cfg.Name)

		// Print team summary
		for _, t := range cfg.Teams {
			kind := "solo"
			if t.HasMembers() {
				kind = fmt.Sprintf("team (%d members)", len(t.Members))
			}
			deps := ""
			if len(t.DependsOn) > 0 {
				deps = fmt.Sprintf(" → depends on %v", t.DependsOn)
			}
			logger.Info("%s: %s, %d tasks%s", t.Name, kind, len(t.Tasks), deps)
		}
	},
}
