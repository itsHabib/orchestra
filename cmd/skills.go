package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/itsHabib/orchestra/internal/skills"
)

var (
	skillsUploadFrom string
	skillsProbeFrom  string
	skillsProbeMount string
	skillsProbeModel string
	skillsProbeKeep  bool
)

var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage orchestra skills uploaded to the Anthropic Files API",
	Long: "Upload SKILL.md files to the Anthropic Files API and cache the\n" +
		"resulting file IDs in <user-config-dir>/orchestra/skills.json.\n\n" +
		"The cache is read by the engine on session creation; cached file IDs\n" +
		"are attached to MA sessions as file resources so the agent can read\n" +
		"its role from the SKILL.md mounted in the container.",
}

var skillsUploadCmd = &cobra.Command{
	Use:   "upload <name>",
	Short: "Upload one skill's SKILL.md and cache the resulting file_id",
	Args:  cobra.ExactArgs(1),
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
	Short: "Re-upload any cached skills whose source file has drifted",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runSkillsSync(cmd.Context())
	},
}

var skillsProbeMountCmd = &cobra.Command{
	Use:    "probe-mount <name>",
	Short:  "One-off probe: upload SKILL.md, attach to a fresh MA session, dump container filesystem",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSkillsProbeMount(cmd.Context(), args[0])
	},
}

func init() {
	skillsUploadCmd.Flags().StringVar(&skillsUploadFrom, "from", "", "Path to SKILL.md (default: $HOME/.claude/skills/<name>/SKILL.md)")
	skillsProbeMountCmd.Flags().StringVar(&skillsProbeFrom, "from", "", "Path to SKILL.md (default: $HOME/.claude/skills/<name>/SKILL.md)")
	skillsProbeMountCmd.Flags().StringVar(&skillsProbeMount, "mount-path", "", "Override mount path (default: SDK default /mnt/session/uploads/<file_id>)")
	skillsProbeMountCmd.Flags().StringVar(&skillsProbeModel, "model", "claude-sonnet-4-6", "Model id for the probe agent")
	skillsProbeMountCmd.Flags().BoolVar(&skillsProbeKeep, "keep", false, "Leave the env+agent+session in place instead of archiving on exit")

	skillsCmd.AddCommand(skillsUploadCmd)
	skillsCmd.AddCommand(skillsLsCmd)
	skillsCmd.AddCommand(skillsSyncCmd)
	skillsCmd.AddCommand(skillsProbeMountCmd)
	rootCmd.AddCommand(skillsCmd)
}

func runSkillsUpload(ctx context.Context, name string) error {
	source, err := skills.ResolveSource(name, skillsUploadFrom)
	if err != nil {
		return err
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("skills upload: source %s: %w", source, err)
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
		fmt.Printf("up-to-date  %s  file_id=%s  hash=%s\n", res.Name, res.Entry.FileID, shortHash(res.Entry.ContentHash))
	case skills.SyncReuploaded:
		if res.PreviousFile != "" {
			fmt.Printf("reuploaded  %s  file_id=%s  prev=%s  hash=%s\n",
				res.Name, res.Entry.FileID, res.PreviousFile, shortHash(res.Entry.ContentHash))
		} else {
			fmt.Printf("uploaded    %s  file_id=%s  hash=%s\n", res.Name, res.Entry.FileID, shortHash(res.Entry.ContentHash))
		}
	default:
		fmt.Printf("%s  %s  file_id=%s\n", res.Action, res.Name, res.Entry.FileID)
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
	fmt.Printf("%-24s  %-32s  %-9s  %-16s  %s\n", "NAME", "FILE ID", "HASH", "UPLOADED AT", "STATUS")
	for i := range looks {
		look := &looks[i]
		fmt.Printf("%-24s  %-32s  %-9s  %-16s  %s\n",
			look.Name,
			look.Entry.FileID,
			shortHash(look.Entry.ContentHash),
			formatCacheTime(look.Entry.UploadedAt),
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
			fmt.Printf("up-to-date  %s  file_id=%s\n", r.Name, r.Entry.FileID)
		case skills.SyncReuploaded:
			fmt.Printf("reuploaded  %s  file_id=%s  prev=%s\n", r.Name, r.Entry.FileID, r.PreviousFile)
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
