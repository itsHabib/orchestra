package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// fabricatedResult builds a representative *orchestra.Result without
// touching the engine. Mirrors what runWithLockedWorkspace's buildResult
// step would produce after a successful auditor run.
func fabricatedResult(teamStatus string, withSummary bool) *orchestra.Result {
	team := orchestra.TeamResult{
		TeamState: orchestra.TeamState{
			Status:   teamStatus,
			NumTurns: 7,
			CostUSD:  0.0123,
		},
	}
	if withSummary {
		team.ResultSummary = "Findings:\n- Design doc alignment: matches.\n- Godoc: 1 missing on ExportedFn."
	}
	if teamStatus == "failed" {
		team.LastError = "max turns exhausted"
	}
	return &orchestra.Result{
		Project:    "pr-audit-21",
		Teams:      map[string]orchestra.TeamResult{auditorTeamName: team},
		Tiers:      [][]string{{auditorTeamName}},
		DurationMs: 9_876,
	}
}

func samplePR() PRData {
	return PRData{
		Number:      21,
		Title:       "P2.5 PR1: ValidationResult reshape",
		URL:         "https://github.com/itsHabib/orchestra/pull/21",
		BaseRefName: "main",
		HeadRefName: "p2.5/validation-result",
		Additions:   1438,
		Deletions:   243,
		Files:       []GHFile{{Path: "pkg/orchestra/types.go", Additions: 80, Deletions: 5}},
		Body:        "Summary body",
	}
}

func TestRenderMarkdown_HappyPath(t *testing.T) {
	pr := samplePR()
	result := fabricatedResult("done", true)
	gen := time.Date(2026, 4, 25, 16, 42, 11, 0, time.UTC)

	out := renderMarkdown(result, &pr, ".orchestra/pr-audit-21/", gen)

	// PR header bits
	wantSubstrings := []string{
		"# PR audit: #21",
		"P2.5 PR1: ValidationResult reshape",
		"https://github.com/itsHabib/orchestra/pull/21",
		"main -> p2.5/validation-result",
		"+1438 / -243",
		"## Audit run",
		"- **Status.** success",
		"- **Workspace.** .orchestra/pr-audit-21/",
		"- **Total cost.** $0.0123",
		"- **Total turns.** 7",
		"- **Duration.** 9876ms",
		"## auditor",
		"- **Status.** done",
		"> Findings:",
		"_Generated 2026-04-25 16:42:11 UTC by tools/pr-audit_",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(out, sub) {
			t.Errorf("markdown missing substring %q\n--- got ---\n%s\n", sub, out)
		}
	}
}

func TestRenderMarkdown_FailedTeam(t *testing.T) {
	pr := samplePR()
	result := fabricatedResult("failed", false)
	gen := time.Date(2026, 4, 25, 16, 42, 11, 0, time.UTC)

	out := renderMarkdown(result, &pr, ".orchestra/pr-audit-21/", gen)

	if !strings.Contains(out, "- **Status.** failed") {
		t.Error("markdown missing top-level failed status")
	}
	if !strings.Contains(out, "- **Error.** max turns exhausted") {
		t.Error("markdown missing per-team error line")
	}
}

func TestRenderMarkdown_NilResultStillEmitsHeader(t *testing.T) {
	pr := samplePR()
	gen := time.Date(2026, 4, 25, 16, 42, 11, 0, time.UTC)

	// Per the design doc and P2.4 contract, *Result is non-nil even on
	// partial failure. But pr-audit's own setup-error path may bail
	// before Run is invoked, in which case we still want to render the
	// PR header. This test pins that behavior.
	out := renderMarkdown(nil, &pr, ".orchestra/pr-audit-21/", gen)
	if !strings.Contains(out, "# PR audit: #21") {
		t.Error("nil result: markdown missing PR header")
	}
	if !strings.Contains(out, "- **Status.** failed") {
		t.Error("nil result: markdown missing failed top-level status")
	}
}

func TestRenderJSON_Schema(t *testing.T) {
	pr := samplePR()
	result := fabricatedResult("done", true)
	gen := time.Date(2026, 4, 25, 16, 42, 11, 0, time.UTC)

	out, err := renderJSON(result, &pr, ".orchestra/pr-audit-21/", gen)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}

	var doc jsonReport
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("JSON unmarshal: %v\nraw: %s", err, out)
	}

	if doc.PR.Number != 21 {
		t.Errorf("doc.PR.Number = %d, want 21", doc.PR.Number)
	}
	if doc.PR.Title == "" {
		t.Error("doc.PR.Title empty")
	}
	if doc.Status != "success" {
		t.Errorf("doc.Status = %q, want success", doc.Status)
	}
	if doc.Workspace != ".orchestra/pr-audit-21/" {
		t.Errorf("doc.Workspace = %q", doc.Workspace)
	}
	if doc.CostUSD != 0.0123 {
		t.Errorf("doc.CostUSD = %v, want 0.0123", doc.CostUSD)
	}
	if doc.Turns != 7 {
		t.Errorf("doc.Turns = %d, want 7", doc.Turns)
	}
	if doc.DurationMs != 9876 {
		t.Errorf("doc.DurationMs = %d, want 9876", doc.DurationMs)
	}
	if len(doc.Teams) != 1 {
		t.Fatalf("doc.Teams len = %d, want 1", len(doc.Teams))
	}
	if doc.Teams[0].Name != auditorTeamName {
		t.Errorf("Teams[0].Name = %q", doc.Teams[0].Name)
	}
	if doc.Teams[0].Status != "done" {
		t.Errorf("Teams[0].Status = %q", doc.Teams[0].Status)
	}
	if !strings.Contains(doc.Teams[0].Summary, "Findings:") {
		t.Errorf("Teams[0].Summary = %q, want contains \"Findings:\"", doc.Teams[0].Summary)
	}
	if got := doc.GeneratedAt.UTC().Format(time.RFC3339); got != "2026-04-25T16:42:11Z" {
		t.Errorf("doc.GeneratedAt = %q, want 2026-04-25T16:42:11Z", got)
	}
}

func TestRenderJSON_FailedTeamCarriesError(t *testing.T) {
	pr := samplePR()
	result := fabricatedResult("failed", false)
	gen := time.Date(2026, 4, 25, 16, 42, 11, 0, time.UTC)

	out, err := renderJSON(result, &pr, ".orchestra/pr-audit-21/", gen)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}

	var doc jsonReport
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}
	if doc.Status != "failed" {
		t.Errorf("doc.Status = %q, want failed", doc.Status)
	}
	if len(doc.Teams) != 1 {
		t.Fatalf("doc.Teams len = %d, want 1", len(doc.Teams))
	}
	if doc.Teams[0].Error != "max turns exhausted" {
		t.Errorf("Teams[0].Error = %q, want \"max turns exhausted\"", doc.Teams[0].Error)
	}
}

func TestRenderJSON_NilResultEmitsFailedStatus(t *testing.T) {
	pr := samplePR()
	gen := time.Date(2026, 4, 25, 16, 42, 11, 0, time.UTC)

	out, err := renderJSON(nil, &pr, ".orchestra/pr-audit-21/", gen)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	var doc jsonReport
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}
	if doc.Status != "failed" {
		t.Errorf("nil result: doc.Status = %q, want failed", doc.Status)
	}
	if len(doc.Teams) != 0 {
		t.Errorf("nil result: doc.Teams len = %d, want 0", len(doc.Teams))
	}
	if doc.PR.Number != 21 {
		t.Errorf("nil result: doc.PR.Number = %d, want 21 (PR meta still populated)", doc.PR.Number)
	}
}

func TestRenderJSON_TierOrderingDeterministic(t *testing.T) {
	// Two-team result, alphabetical-vs-tier order would differ.
	result := &orchestra.Result{
		Project: "pr-audit-21",
		Teams: map[string]orchestra.TeamResult{
			"zeta":  {TeamState: orchestra.TeamState{Status: "done", NumTurns: 1}},
			"alpha": {TeamState: orchestra.TeamState{Status: "done", NumTurns: 1}},
		},
		Tiers: [][]string{{"zeta"}, {"alpha"}},
	}

	for range 5 {
		pr := samplePR()
		out, err := renderJSON(result, &pr, "/ws", time.Now())
		if err != nil {
			t.Fatalf("renderJSON: %v", err)
		}
		var doc jsonReport
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(doc.Teams) != 2 || doc.Teams[0].Name != "zeta" || doc.Teams[1].Name != "alpha" {
			t.Fatalf("teams not in tier order: %+v", doc.Teams)
		}
	}
}
