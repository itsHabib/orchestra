package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/itsHabib/orchestra/internal/skills"
)

var skillsUploadFrom string

var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage orchestra skills registered with the Anthropic Skills API",
	Long: "Register skill directories with the Anthropic Skills API and cache\n" +
		"the resulting skill_ids in <user-config-dir>/orchestra/skills.json.\n\n" +
		"The cache is read by the engine on session creation; cached skill_ids\n" +
		"are attached to the MA agent so the role definition (SKILL.md) is\n" +
		"surfaced to the model via Anthropic's native skill primitive.",
}

var skillsUploadCmd = &cobra.Command{
	Use:   "upload <name>",
	Short: "Register a skill directory and cache the resulting skill_id",
	Long: "Walks the skill directory (default: $HOME/.claude/skills/<name>/),\n" +
		"requires a SKILL.md at its root, and uploads every regular non-hidden\n" +
		"file as part of the skill bundle. On a content change re-runs publish\n" +
		"a new version under the same skill_id.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSkillsUpload(cmd.Context(), args[0])
	},
}

var skillsLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List cached skills with drift status",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runSkillsLs(cmd.Context())
	},
}

var skillsSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Re-publish any cached skills whose source directory has drifted",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runSkillsSync(cmd.Context())
	},
}

func init() {
	skillsUploadCmd.Flags().StringVar(&skillsUploadFrom, "from", "", "Path to skill directory (default: $HOME/.claude/skills/<name>/)")

	skillsCmd.AddCommand(skillsUploadCmd)
	skillsCmd.AddCommand(skillsLsCmd)
	skillsCmd.AddCommand(skillsSyncCmd)
	rootCmd.AddCommand(skillsCmd)
}

func runSkillsUpload(ctx context.Context, name string) error {
	source, err := skills.ResolveSource(name, skillsUploadFrom)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("skills upload: source %s: %w", source, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("skills upload: source %s must be a directory containing SKILL.md", source)
	}

	svc, err := skills.NewHostService()
	if err != nil {
		return fmt.Errorf("skills upload: %w", err)
	}
	res, err := svc.Upload(ctx, name, source)
	if err != nil {
		return fmt.Errorf("skills upload: %w", err)
	}

	switch res.Action {
	case skills.SyncUpToDate:
		fmt.Printf("up-to-date  %s  skill_id=%s  version=%s  hash=%s\n",
			res.Name, res.Entry.SkillID, res.Entry.LatestVersion, shortHash(res.Entry.ContentHash))
	case skills.SyncReuploaded:
		if res.PreviousVersion != "" {
			fmt.Printf("reuploaded  %s  skill_id=%s  version=%s  prev=%s  hash=%s\n",
				res.Name, res.Entry.SkillID, res.Entry.LatestVersion, res.PreviousVersion, shortHash(res.Entry.ContentHash))
		} else {
			fmt.Printf("registered  %s  skill_id=%s  version=%s  hash=%s\n",
				res.Name, res.Entry.SkillID, res.Entry.LatestVersion, shortHash(res.Entry.ContentHash))
		}
	default:
		fmt.Printf("%s  %s  skill_id=%s\n", res.Action, res.Name, res.Entry.SkillID)
	}
	return nil
}

func runSkillsLs(ctx context.Context) error {
	svc, err := skills.NewHostService()
	if err != nil {
		return fmt.Errorf("skills ls: %w", err)
	}
	looks, err := svc.SortedLookups(ctx)
	if err != nil {
		return fmt.Errorf("skills ls: %w", err)
	}
	if len(looks) == 0 {
		fmt.Println("No cached skills.")
		fmt.Printf("Cache: %s\n", skills.DefaultCachePath())
		return nil
	}
	fmt.Printf("%-24s  %-32s  %-20s  %-9s  %-16s  %s\n", "NAME", "SKILL ID", "VERSION", "HASH", "REGISTERED AT", "STATUS")
	for i := range looks {
		look := &looks[i]
		fmt.Printf("%-24s  %-32s  %-20s  %-9s  %-16s  %s\n",
			look.Name,
			displayOrDash(look.Entry.SkillID),
			displayOrDash(look.Entry.LatestVersion),
			shortHash(look.Entry.ContentHash),
			formatCacheTime(look.Entry.RegisteredAt),
			driftStatus(look),
		)
	}
	return nil
}

func runSkillsSync(ctx context.Context) error {
	svc, err := skills.NewHostService()
	if err != nil {
		return fmt.Errorf("skills sync: %w", err)
	}
	results, err := svc.Sync(ctx)
	if err != nil {
		return fmt.Errorf("skills sync: %w", err)
	}
	if len(results) == 0 {
		fmt.Println("No cached skills.")
		return nil
	}
	for i := range results {
		r := &results[i]
		switch r.Action {
		case skills.SyncUpToDate:
			fmt.Printf("up-to-date  %s  skill_id=%s  version=%s\n", r.Name, r.Entry.SkillID, r.Entry.LatestVersion)
		case skills.SyncReuploaded:
			fmt.Printf("reuploaded  %s  skill_id=%s  version=%s  prev=%s\n",
				r.Name, r.Entry.SkillID, r.Entry.LatestVersion, r.PreviousVersion)
		case skills.SyncSkipped:
			reason := "skipped"
			switch {
			case r.SourceMissing:
				reason = "source missing: " + r.Entry.SourcePath
			case r.Err != nil:
				reason = r.Err.Error()
			}
			fmt.Printf("skipped     %s  %s\n", r.Name, reason)
		default:
			fmt.Printf("%-10s  %s\n", r.Action, r.Name)
		}
	}
	return nil
}

func driftStatus(look *skills.Lookup) string {
	switch {
	case !look.Found:
		return "not registered"
	case look.SourceMissing:
		return "source missing (" + look.Entry.SourcePath + ")"
	case look.Drifted:
		return "drifted (current=" + shortHash(look.CurrentHash) + ")"
	default:
		return "fresh"
	}
}

func shortHash(h string) string {
	const prefix = "sha256:"
	short := strings.TrimPrefix(h, prefix)
	if len(short) > 9 {
		short = short[:9]
	}
	if short == "" {
		return "-"
	}
	return short
}
