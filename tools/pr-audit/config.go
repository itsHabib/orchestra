package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

const (
	// defaultModel is the auditor's default model. Overridden by
	// PR_AUDIT_MODEL.
	defaultModel = "haiku"
	// defaultMaxTurns caps the auditor's per-task turn budget.
	// Overridden by PR_AUDIT_MAX_TURNS. Kept low — the audit is read-only
	// reasoning, not a build-and-test loop.
	defaultMaxTurns = 10
	// diffTruncationLimit caps the inline diff size in the team's
	// Context. Roughly the size of a 1500-line patch — about the largest
	// a haiku-class model can usefully read in one pass. Beyond this we
	// append a `[diff truncated]` footer so the model knows the input
	// was clipped.
	diffTruncationLimit = 50 * 1024
	// auditorTeamName is the only team in the audit config. Surfaced as
	// a constant so the report renderer and tests can reference the same
	// string.
	auditorTeamName = "auditor"
)

// modelFromEnv returns PR_AUDIT_MODEL or fallback when unset.
func modelFromEnv(fallback string) string {
	if v := os.Getenv("PR_AUDIT_MODEL"); v != "" {
		return v
	}
	return fallback
}

// maxTurnsFromEnv returns PR_AUDIT_MAX_TURNS as an int, or fallback
// when unset, non-numeric, or non-positive. A bad value silently falls
// back — surfacing it as an error would force callers to handle
// env-parsing in their orchestration layer. fallback is a parameter
// (rather than reading defaultMaxTurns directly) so tests can verify
// the fallback path with a non-default value.
func maxTurnsFromEnv(fallback int) int {
	v := os.Getenv("PR_AUDIT_MAX_TURNS")
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// truncateDiff caps diff at diffTruncationLimit, appending a marker
// footer when truncation occurred. Returned string is safe to inline
// into a model prompt.
func truncateDiff(diff string) string {
	if len(diff) <= diffTruncationLimit {
		return diff
	}
	return diff[:diffTruncationLimit] + "\n\n[diff truncated]\n"
}

// buildConfig assembles the orchestra.Config used by the audit run.
// Pure function — no I/O beyond the env reads in modelFromEnv and
// maxTurnsFromEnv. The caller is expected to validate the returned
// config via orchestra.Validate before passing to orchestra.Run. pr is
// taken by pointer purely to avoid a 144-byte stack copy; the function
// does not mutate it.
func buildConfig(pr *PRData) *orchestra.Config {
	context := buildAuditorContext(pr)

	return &orchestra.Config{
		Name: "pr-audit-" + strconv.Itoa(pr.Number),
		Defaults: orchestra.Defaults{
			Model:    modelFromEnv(defaultModel),
			MaxTurns: maxTurnsFromEnv(defaultMaxTurns),
			// Read-only audit; bypassPermissions keeps the local
			// claude subprocess from prompting on tool calls.
			PermissionMode: "bypassPermissions",
			TimeoutMinutes: 15,
		},
		Teams: []orchestra.Team{
			{
				Name:    auditorTeamName,
				Lead:    orchestra.Lead{Role: "Senior PR auditor — reads the PR and writes a structured findings report"},
				Context: context,
				Tasks: []orchestra.Task{
					{
						Summary: "Design-doc alignment: identify which design doc this PR claims to implement and check whether the diff matches the spec",
						Details: "Look at the PR body and commit messages for a reference to docs/features/*.md. " +
							"Open the referenced doc and skim §3 (scope), §6 (implementation order), §7 (acceptance). " +
							"Compare the diff against the in-scope items: which were implemented, which were skipped, which were extras. " +
							"Flag any in-scope item that is missing or any out-of-scope behavior that snuck in.",
						Verify: "Findings report names the design doc and lists alignment / drift items.",
					},
					{
						Summary: "Godoc completeness: every newly exported identifier in the diff has a meaningful godoc comment",
						Details: "Scan the diff for `+func`, `+type`, `+const`, `+var` lines that introduce exported identifiers (uppercase first letter). " +
							"For each, verify the line above introduces a doc comment that names the identifier and explains what it does. " +
							"Flag missing godoc; flag godoc that just restates the name (e.g. `// Foo is the foo`).",
						Verify: "Findings report lists every newly exported identifier and its godoc verdict.",
					},
					{
						Summary: "CHANGELOG hygiene: a CHANGELOG.md entry under Unreleased exists and matches the change set",
						Details: "If the PR touches pkg/orchestra (the SDK surface), it must add a CHANGELOG.md entry under `## Unreleased`. " +
							"Verify the entry is present, classified correctly (Experimental / Stable, breaking vs additive), " +
							"and that its bullet list matches the diff (no missing changes, no phantom claims).",
						Verify: "Findings report names the CHANGELOG section header, lists each bullet, and notes diff↔changelog drift.",
					},
				},
			},
		},
	}
}

// buildAuditorContext is the inline prompt context handed to the
// auditor team. PR meta + truncated diff. Kept ASCII-fenced so the
// model sees a clear separator between meta and diff. pr is taken by
// pointer purely to avoid a 144-byte stack copy.
func buildAuditorContext(pr *PRData) string {
	diff := truncateDiff(pr.Diff)

	files := ""
	for _, f := range pr.Files {
		files += fmt.Sprintf("- %s (+%d / -%d)\n", f.Path, f.Additions, f.Deletions)
	}

	return fmt.Sprintf(`You are auditing a GitHub pull request. Your job is read-only: produce a structured findings report. Do not modify any files. Do not run any tests.

## PR meta

- **Number.** #%d
- **Title.** %s
- **URL.** %s
- **Branch.** %s -> %s
- **Diff.** +%d / -%d lines

## Files changed

%s
## PR body

%s

## Diff

%s
`,
		pr.Number, pr.Title, pr.URL, pr.BaseRefName, pr.HeadRefName,
		pr.Additions, pr.Deletions, files, pr.Body, diff)
}
