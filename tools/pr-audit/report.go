package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// reportMeta is the per-PR header surfaced into both the Markdown and
// JSON renderers. Kept a small struct so the JSON renderer's nested
// "pr" object and the Markdown renderer's header share a single shape.
type reportMeta struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	BaseRefName string `json:"base_ref_name"`
	HeadRefName string `json:"head_ref_name"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
}

// reportTeam is the per-team JSON shape. Kept flat so callers don't
// have to reach into nested SDK types.
type reportTeam struct {
	Name    string  `json:"name"`
	Status  string  `json:"status"`
	Turns   int     `json:"turns"`
	CostUSD float64 `json:"cost_usd"`
	Summary string  `json:"summary"`
	Error   string  `json:"error,omitempty"`
}

// jsonReport is the flat output schema for `pr-audit --json`. Fields
// not stable across releases — README documents that consumers pin a
// commit SHA.
type jsonReport struct {
	PR          reportMeta   `json:"pr"`
	Status      string       `json:"status"` // success | failed
	Workspace   string       `json:"workspace"`
	CostUSD     float64      `json:"cost_usd"`
	Turns       int          `json:"turns"`
	DurationMs  int64        `json:"duration_ms"`
	Teams       []reportTeam `json:"teams"`
	GeneratedAt time.Time    `json:"generated_at"`
}

// renderMarkdown renders a human-readable Markdown report from the
// orchestra.Result. Result is non-nil even on partial failure
// (P2.4 contract) — the renderer treats a nil result as a degenerate
// case (no teams ran) and still emits the PR header. pr is taken by
// pointer purely to avoid a 144-byte stack copy; the function does
// not mutate it.
func renderMarkdown(result *orchestra.Result, pr *PRData, workspace string, generatedAt time.Time) string {
	var b strings.Builder
	writeMarkdownHeader(&b, pr)
	writeMarkdownAuditRun(&b, result, workspace)
	writeMarkdownTeams(&b, result)
	writeMarkdownFooter(&b, generatedAt)
	return b.String()
}

func writeMarkdownHeader(b *strings.Builder, pr *PRData) {
	fmt.Fprintf(b, "# PR audit: #%d\n\n", pr.Number)
	fmt.Fprintf(b, "- **Title.** %s\n", pr.Title)
	if pr.URL != "" {
		fmt.Fprintf(b, "- **URL.** %s\n", pr.URL)
	}
	if pr.BaseRefName != "" || pr.HeadRefName != "" {
		fmt.Fprintf(b, "- **Branch.** %s -> %s\n", pr.BaseRefName, pr.HeadRefName)
	}
	fmt.Fprintf(b, "- **Diff.** +%d / -%d lines\n", pr.Additions, pr.Deletions)
	fmt.Fprintln(b)
}

func writeMarkdownAuditRun(b *strings.Builder, result *orchestra.Result, workspace string) {
	s := summarize(result)
	fmt.Fprintln(b, "## Audit run")
	fmt.Fprintln(b)
	fmt.Fprintf(b, "- **Status.** %s\n", s.Status)
	fmt.Fprintf(b, "- **Workspace.** %s\n", workspace)
	fmt.Fprintf(b, "- **Total cost.** $%.4f USD\n", s.TotalCost)
	fmt.Fprintf(b, "- **Total turns.** %d\n", s.TotalTurns)
	fmt.Fprintf(b, "- **Duration.** %dms\n", s.DurationMs)
	fmt.Fprintln(b)
}

func writeMarkdownTeams(b *strings.Builder, result *orchestra.Result) {
	if result == nil {
		return
	}
	for _, name := range orderedTeamNames(result) {
		// Index into the map rather than `for _, t := range Teams` to
		// satisfy gocritic's rangeValCopy lint rule — TeamResult embeds
		// the wide TeamState. Both forms make one copy per iteration;
		// only the lint disposition differs.
		teams := result.Teams
		t := teams[name]
		fmt.Fprintf(b, "## %s\n\n", name)
		fmt.Fprintf(b, "- **Status.** %s\n", nonEmpty(t.Status, "(unknown)"))
		fmt.Fprintf(b, "- **Turns.** %d\n", t.NumTurns)
		fmt.Fprintf(b, "- **Cost.** $%.4f USD\n", t.CostUSD)
		if t.LastError != "" {
			fmt.Fprintf(b, "- **Error.** %s\n", t.LastError)
		}
		if t.ResultSummary != "" {
			fmt.Fprintln(b)
			for _, line := range strings.Split(strings.TrimSpace(t.ResultSummary), "\n") {
				fmt.Fprintf(b, "> %s\n", line)
			}
		}
		fmt.Fprintln(b)
	}
}

func writeMarkdownFooter(b *strings.Builder, generatedAt time.Time) {
	fmt.Fprintln(b, "---")
	fmt.Fprintln(b)
	fmt.Fprintf(b, "_Generated %s by tools/pr-audit_\n", generatedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
}

// renderJSON renders a flat JSON document with the same field set as
// the Markdown report. As with renderMarkdown, a nil result produces a
// degenerate document with empty teams and status="failed" so callers
// distinguish "audit ran" from "audit didn't run" via the status field.
// pr is taken by pointer purely to avoid a 144-byte stack copy; the
// function does not mutate it.
func renderJSON(result *orchestra.Result, pr *PRData, workspace string, generatedAt time.Time) ([]byte, error) {
	s := summarize(result)
	teams := teamsJSON(result)
	doc := jsonReport{
		PR: reportMeta{
			Number:      pr.Number,
			Title:       pr.Title,
			URL:         pr.URL,
			BaseRefName: pr.BaseRefName,
			HeadRefName: pr.HeadRefName,
			Additions:   pr.Additions,
			Deletions:   pr.Deletions,
		},
		Status:      s.Status,
		Workspace:   workspace,
		CostUSD:     s.TotalCost,
		Turns:       s.TotalTurns,
		DurationMs:  s.DurationMs,
		Teams:       teams,
		GeneratedAt: generatedAt.UTC(),
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal report: %w", err)
	}
	return out, nil
}

// runSummary is the four scalar fields the Markdown and JSON renderers
// share: overall status (success / failed), summed cost, summed turns,
// and duration. Returned as a struct (not named returns, which the
// linter forbids) so future additions don't shift the call signature.
type runSummary struct {
	Status     string
	TotalCost  float64
	TotalTurns int
	DurationMs int64
}

// summarize reduces a *Result to a runSummary. A team in any state
// other than "done" forces overall Status to "failed". A nil result —
// or a result with zero teams — is treated as a degenerate failure:
// nothing ran, so we cannot claim success. decideExit relies on this
// status to gate exitSuccess vs exitAuditFailure, so the renderers
// and the exit-code path stay in sync.
func summarize(result *orchestra.Result) runSummary {
	if result == nil {
		return runSummary{Status: "failed"}
	}
	out := runSummary{Status: "success", DurationMs: result.DurationMs}
	if len(result.Teams) == 0 {
		out.Status = "failed"
	}
	for name := range result.Teams {
		t := result.Teams[name]
		out.TotalCost += t.CostUSD
		out.TotalTurns += t.NumTurns
		if t.Status != "done" {
			out.Status = "failed"
		}
	}
	return out
}

// teamsJSON flattens result.Teams into a deterministically-ordered
// slice for the JSON renderer. Map iteration order in Go is
// non-deterministic, so this guards against test flakes and gives
// downstream consumers a stable shape to diff.
func teamsJSON(result *orchestra.Result) []reportTeam {
	if result == nil {
		return nil
	}
	out := make([]reportTeam, 0, len(result.Teams))
	for _, name := range orderedTeamNames(result) {
		// Index into the map rather than `for _, t := range Teams` to
		// satisfy gocritic's rangeValCopy lint rule (see writeMarkdownTeams).
		t := result.Teams[name]
		out = append(out, reportTeam{
			Name:    name,
			Status:  t.Status,
			Turns:   t.NumTurns,
			CostUSD: t.CostUSD,
			Summary: t.ResultSummary,
			Error:   t.LastError,
		})
	}
	return out
}

// orderedTeamNames returns the team names in tier order (the order the
// engine ran them) and falls back to alphabetical when the tier
// metadata is empty or refers to teams not present in the Teams map.
// Stable ordering matters for both the Markdown table-of-contents feel
// and for deterministic JSON output.
func orderedTeamNames(result *orchestra.Result) []string {
	seen := make(map[string]bool, len(result.Teams))
	var ordered []string
	for _, tier := range result.Tiers {
		for _, name := range tier {
			if _, ok := result.Teams[name]; !ok {
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			ordered = append(ordered, name)
		}
	}
	if len(ordered) == len(result.Teams) {
		return ordered
	}
	// Fallback: append any teams the tier metadata missed in
	// alphabetical order so we never silently drop a team from the
	// report. This branch is defensive — under healthy runs Tiers
	// covers every team in Teams.
	leftover := make([]string, 0, len(result.Teams)-len(ordered))
	for name := range result.Teams {
		if !seen[name] {
			leftover = append(leftover, name)
		}
	}
	sort.Strings(leftover)
	return append(ordered, leftover...)
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
