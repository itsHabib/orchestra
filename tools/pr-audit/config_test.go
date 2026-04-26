package main

import (
	"context"
	"strings"
	"testing"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// fixturePR loads the canned PR #21 testdata via the same
// fetchPR-with-stub harness used by gh_test.go. Returns the parsed
// PRData. Helper exists so config_test.go and report_test.go can share
// a realistic input without re-implementing the stub plumbing.
func fixturePR(t *testing.T) PRData {
	t.Helper()
	withExecStub(t,
		execStubCall{
			wantArgs:   []string{"gh", "pr", "view", "21", "--json", ghJSONFields},
			stdoutFile: "testdata/pr-21.json",
		},
		execStubCall{
			wantArgs:   []string{"gh", "pr", "diff", "21"},
			stdoutFile: "testdata/pr-21.diff",
		},
	)
	pr, err := fetchPR(context.Background(), 21)
	if err != nil {
		t.Fatalf("fetchPR for fixture: %v", err)
	}
	return pr
}

func TestBuildConfig_ValidatesClean(t *testing.T) {
	pr := fixturePR(t)
	cfg := buildConfig(pr)

	res := orchestra.Validate(cfg)
	if !res.Valid() {
		t.Fatalf("orchestra.Validate(buildConfig(...)) failed: %v", res.Err())
	}
	// Warnings are allowed (e.g. task/member ratio); we just want zero
	// hard errors. Surface warnings on test verbosity to make future
	// regressions obvious.
	for _, w := range res.Warnings {
		t.Logf("validation warning: %s", w)
	}
}

func TestBuildConfig_TeamAndTaskShape(t *testing.T) {
	pr := fixturePR(t)
	cfg := buildConfig(pr)

	if len(cfg.Teams) != 1 {
		t.Fatalf("Teams len = %d, want 1", len(cfg.Teams))
	}
	team := cfg.Teams[0]
	if team.Name != auditorTeamName {
		t.Errorf("team.Name = %q, want %q", team.Name, auditorTeamName)
	}
	if len(team.Tasks) != 3 {
		t.Errorf("Tasks len = %d, want 3", len(team.Tasks))
	}

	wantPrefixes := []string{
		"Design-doc alignment",
		"Godoc completeness",
		"CHANGELOG hygiene",
	}
	for i, want := range wantPrefixes {
		if !strings.HasPrefix(team.Tasks[i].Summary, want) {
			t.Errorf("Tasks[%d].Summary = %q, want prefix %q", i, team.Tasks[i].Summary, want)
		}
	}
}

func TestBuildConfig_ContextEmbedsPRMetaAndDiff(t *testing.T) {
	pr := fixturePR(t)
	cfg := buildConfig(pr)
	ctx := cfg.Teams[0].Context

	if !strings.Contains(ctx, pr.Title) {
		t.Errorf("context missing PR title %q", pr.Title)
	}
	if !strings.Contains(ctx, pr.URL) {
		t.Errorf("context missing PR url %q", pr.URL)
	}
	if !strings.Contains(ctx, "diff --git") {
		t.Error("context missing diff content")
	}
}

func TestBuildConfig_TruncatesOversizeDiff(t *testing.T) {
	pr := fixturePR(t)
	if len(pr.Diff) <= diffTruncationLimit {
		t.Fatalf("fixture diff is %d bytes; test requires >%d to exercise truncation", len(pr.Diff), diffTruncationLimit)
	}

	cfg := buildConfig(pr)
	ctx := cfg.Teams[0].Context

	if !strings.Contains(ctx, "[diff truncated]") {
		t.Error("oversize diff: context missing `[diff truncated]` footer")
	}
	// The full untruncated diff would push the team Context past the
	// fixture diff length; assert the embedded diff is bounded by the
	// truncation limit plus the footer.
	embeddedDiff := ctx[strings.Index(ctx, "diff --git"):]
	if len(embeddedDiff) > diffTruncationLimit+len("\n\n[diff truncated]\n")+64 {
		t.Errorf("embedded diff length = %d, want <= %d (truncation limit + footer)", len(embeddedDiff), diffTruncationLimit)
	}
}

func TestBuildConfig_LeavesUnderLimitUntouched(t *testing.T) {
	pr := PRData{
		Number: 42,
		Title:  "small PR",
		URL:    "https://example.com/pr/42",
		Diff:   "diff --git a/foo.go b/foo.go\n+func Foo() {}\n",
	}
	cfg := buildConfig(pr)
	ctx := cfg.Teams[0].Context

	if strings.Contains(ctx, "[diff truncated]") {
		t.Error("under-limit diff: context wrongly carries `[diff truncated]` footer")
	}
	if !strings.Contains(ctx, "func Foo()") {
		t.Error("under-limit diff content missing from context")
	}
}

func TestModelFromEnv(t *testing.T) {
	// Restore env between subtests by setting and clearing explicitly.
	t.Setenv("PR_AUDIT_MODEL", "")
	if got := modelFromEnv("haiku"); got != "haiku" {
		t.Errorf("empty env: got %q, want %q", got, "haiku")
	}
	t.Setenv("PR_AUDIT_MODEL", "sonnet")
	if got := modelFromEnv("haiku"); got != "sonnet" {
		t.Errorf("override: got %q, want %q", got, "sonnet")
	}
}

func TestMaxTurnsFromEnv(t *testing.T) {
	t.Setenv("PR_AUDIT_MAX_TURNS", "")
	if got := maxTurnsFromEnv(10); got != 10 {
		t.Errorf("empty env: got %d, want 10", got)
	}
	t.Setenv("PR_AUDIT_MAX_TURNS", "25")
	if got := maxTurnsFromEnv(10); got != 25 {
		t.Errorf("override: got %d, want 25", got)
	}
	t.Setenv("PR_AUDIT_MAX_TURNS", "not-a-number")
	if got := maxTurnsFromEnv(10); got != 10 {
		t.Errorf("non-numeric: got %d, want 10 (fallback)", got)
	}
	t.Setenv("PR_AUDIT_MAX_TURNS", "0")
	if got := maxTurnsFromEnv(10); got != 10 {
		t.Errorf("zero: got %d, want 10 (fallback)", got)
	}
	t.Setenv("PR_AUDIT_MAX_TURNS", "-3")
	if got := maxTurnsFromEnv(10); got != 10 {
		t.Errorf("negative: got %d, want 10 (fallback)", got)
	}
}

func TestBuildConfig_ModelFromEnvOverride(t *testing.T) {
	t.Setenv("PR_AUDIT_MODEL", "opus")
	pr := fixturePR(t)
	cfg := buildConfig(pr)
	if cfg.Defaults.Model != "opus" {
		t.Errorf("Defaults.Model = %q, want %q", cfg.Defaults.Model, "opus")
	}
}

func TestBuildConfig_MaxTurnsFromEnvOverride(t *testing.T) {
	t.Setenv("PR_AUDIT_MAX_TURNS", "33")
	pr := fixturePR(t)
	cfg := buildConfig(pr)
	if cfg.Defaults.MaxTurns != 33 {
		t.Errorf("Defaults.MaxTurns = %d, want 33", cfg.Defaults.MaxTurns)
	}
}
