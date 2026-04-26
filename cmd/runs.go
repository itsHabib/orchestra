package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/itsHabib/orchestra/pkg/orchestra"
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
		summaries, err := orchestra.ListRuns(workspaceForRunsCmd())
		if err != nil {
			fmt.Fprintf(os.Stderr, "runs ls: %v\n", err)
			os.Exit(1)
		}
		printRunRows(summariesToRecords(summaries), runsListLimit, time.Now().UTC())
	},
}

var runsShowCmd = &cobra.Command{
	Use:   "show <run-id>",
	Short: "Show teams and backend resources for a workflow run",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		workspace := workspaceForRunsCmd()
		state, err := orchestra.LoadRun(workspace, args[0])
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "runs show: run %q not found\n", args[0])
			os.Exit(1)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "runs show: %v\n", err)
			os.Exit(1)
		}
		record, err := buildRunRecordForShow(workspace, args[0], state)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runs show: %v\n", err)
			os.Exit(1)
		}
		printRunDetail(record, time.Now().UTC())
	},
}

// workspaceForRunsCmd resolves the workspace flag to its effective value,
// falling back to the global workspaceDir default when the flag was
// omitted.
func workspaceForRunsCmd() string {
	if runsWorkspaceFlag != "" {
		return runsWorkspaceFlag
	}
	return workspaceDir
}

// summariesToRecords converts an SDK [orchestra.RunSummary] slice into the
// cmd-side runRecord shape that printRunRows expects. The SDK summary
// already carries the full [orchestra.RunState] pointer plus the active
// flag and file mtime; we just rewrap into the cmd-internal record type
// so the existing renderer keeps producing byte-identical rows.
//
// Archive directories are derived from RunSummary.RunID, which is the
// state's RunID when present and the archive directory name otherwise.
// In normal operation the two agree; legacy archives or manually edited
// state files where they diverge would compute the wrong dir here.
func summariesToRecords(summaries []orchestra.RunSummary) []runRecord {
	workspace := workspaceForRunsCmd()
	out := make([]runRecord, 0, len(summaries))
	for i := range summaries {
		s := &summaries[i]
		dir := workspace
		if !s.Active {
			dir = runArchiveDir(workspace, s.RunID)
		}
		out = append(out, runRecord{
			id:         s.RunID,
			active:     s.Active,
			dir:        dir,
			state:      s.State,
			modifiedAt: s.ModifiedAt,
		})
	}
	return out
}

// buildRunRecordForShow composes a runRecord for a `runs show` invocation
// from a state already loaded via [orchestra.LoadRun]. The renderer
// requires the file mtime and the on-disk dir, neither of which the SDK
// surface exposes — recompute them locally so output stays byte-identical.
//
// Archive lookup happens first regardless of whether the caller asked
// for the "active" alias, because LoadRun resolves "active" to the most
// recent archive when no live run exists. Falling back to the workspace
// state.json second handles the live-run case.
func buildRunRecordForShow(workspace, runID string, state *orchestra.RunState) (runRecord, error) {
	if state == nil {
		return runRecord{}, fmt.Errorf("run %q has no state", runID)
	}
	resolvedID := runID
	if state.RunID != "" {
		resolvedID = state.RunID
	}

	// Try the archive directory first using the resolved RunID. This makes
	// `runs show active` work in archived-only workspaces where LoadRun
	// returned the most recent archive instead of a live run.
	archiveStatePath := filepath.Join(runArchiveDir(workspace, resolvedID), "state.json")
	if info, err := os.Stat(archiveStatePath); err == nil {
		return runRecord{
			id:         resolvedID,
			dir:        runArchiveDir(workspace, resolvedID),
			state:      state,
			modifiedAt: info.ModTime().UTC(),
		}, nil
	} else if !os.IsNotExist(err) {
		return runRecord{}, err
	}

	activeInfo, err := os.Stat(filepath.Join(workspace, "state.json"))
	if err != nil {
		return runRecord{}, err
	}
	return runRecord{
		id:         resolvedID,
		active:     true,
		dir:        workspace,
		state:      state,
		modifiedAt: activeInfo.ModTime().UTC(),
	}, nil
}

func runArchiveDir(workspace, runID string) string {
	return filepath.Join(workspace, "archive", runID)
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
