package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// withRunOrchestraStub replaces the package-level runOrchestra var
// with a stub that returns the supplied result/error. Restored in
// t.Cleanup. NOT goroutine-safe; tests serialize.
func withRunOrchestraStub(t *testing.T, fn func(context.Context, *orchestra.Config, ...orchestra.Option) (*orchestra.Result, error)) {
	t.Helper()
	orig := runOrchestra
	runOrchestra = fn
	t.Cleanup(func() { runOrchestra = orig })
}

func TestRealMain_SuccessReturnsZero(t *testing.T) {
	stub := withExecStub(t,
		execStubCall{
			wantArgs:   []string{"gh", "pr", "view", "21", "--json", ghJSONFields},
			stdoutFile: "testdata/pr-21.json",
		},
		execStubCall{
			wantArgs:   []string{"gh", "pr", "diff", "21"},
			stdoutFile: "testdata/pr-21.diff",
		},
	)
	withRunOrchestraStub(t, func(_ context.Context, _ *orchestra.Config, _ ...orchestra.Option) (*orchestra.Result, error) {
		return &orchestra.Result{
			Project: "pr-audit-21",
			Teams: map[string]orchestra.TeamResult{
				auditorTeamName: {TeamState: orchestra.TeamState{Status: "done", NumTurns: 7, CostUSD: 0.0123, ResultSummary: "looks good"}},
			},
			Tiers:      [][]string{{auditorTeamName}},
			DurationMs: 9876,
		}, nil
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := realMain(context.Background(), []string{"21"}, stdout, stderr)

	if code != exitSuccess {
		t.Errorf("exit = %d, want %d (success); stderr: %s", code, exitSuccess, stderr.String())
	}
	if stub.idx != len(stub.calls) {
		t.Errorf("execStub: %d calls consumed, want %d", stub.idx, len(stub.calls))
	}
	if !strings.Contains(stdout.String(), "# PR audit: #21") {
		t.Error("stdout missing PR header")
	}
	if !strings.Contains(stdout.String(), "looks good") {
		t.Error("stdout missing result summary")
	}
}

func TestRealMain_FailedTeamReturnsOne(t *testing.T) {
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
	withRunOrchestraStub(t, func(_ context.Context, _ *orchestra.Config, _ ...orchestra.Option) (*orchestra.Result, error) {
		// P2.4 contract: result is non-nil even when runErr is non-nil.
		return &orchestra.Result{
			Project: "pr-audit-21",
			Teams: map[string]orchestra.TeamResult{
				auditorTeamName: {TeamState: orchestra.TeamState{Status: "failed", NumTurns: 3, LastError: "boom"}},
			},
			Tiers:      [][]string{{auditorTeamName}},
			DurationMs: 1234,
		}, errors.New("tier 0: teams failed: [auditor]")
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := realMain(context.Background(), []string{"21"}, stdout, stderr)

	if code != exitAuditFailure {
		t.Errorf("exit = %d, want %d (audit failure); stderr: %s", code, exitAuditFailure, stderr.String())
	}
	if !strings.Contains(stdout.String(), "- **Status.** failed") {
		t.Error("stdout missing failed status")
	}
	if !strings.Contains(stderr.String(), "orchestra.Run reported failure") {
		t.Error("stderr missing run-failure note")
	}
}

func TestRealMain_NoArgReturnsTwo(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := realMain(context.Background(), []string{}, stdout, stderr)
	if code != exitSetupError {
		t.Errorf("exit = %d, want %d (setup error); stderr: %s", code, exitSetupError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "expected exactly one positional argument") {
		t.Error("stderr missing usage hint")
	}
}

func TestRealMain_NonNumericArgReturnsTwo(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := realMain(context.Background(), []string{"abc"}, stdout, stderr)
	if code != exitSetupError {
		t.Errorf("exit = %d, want %d (setup error); stderr: %s", code, exitSetupError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid PR number") {
		t.Error("stderr missing invalid-PR hint")
	}
}

func TestRealMain_GhFailureReturnsTwo(t *testing.T) {
	withExecStub(t,
		execStubCall{
			wantArgs: []string{"gh", "pr", "view", "21", "--json", ghJSONFields},
			stderr:   "gh: not authenticated\n",
			exit:     "1",
		},
	)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := realMain(context.Background(), []string{"21"}, stdout, stderr)
	if code != exitSetupError {
		t.Errorf("exit = %d, want %d (setup error); stderr: %s", code, exitSetupError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "is `gh` installed and authenticated") {
		t.Error("stderr missing gh hint")
	}
}

func TestRealMain_JSONFlagSwitchesShape(t *testing.T) {
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
	withRunOrchestraStub(t, func(_ context.Context, _ *orchestra.Config, _ ...orchestra.Option) (*orchestra.Result, error) {
		return &orchestra.Result{
			Project:    "pr-audit-21",
			Teams:      map[string]orchestra.TeamResult{auditorTeamName: {TeamState: orchestra.TeamState{Status: "done"}}},
			Tiers:      [][]string{{auditorTeamName}},
			DurationMs: 100,
		}, nil
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := realMain(context.Background(), []string{"--json", "21"}, stdout, stderr)
	if code != exitSuccess {
		t.Errorf("exit = %d, want %d; stderr: %s", code, exitSuccess, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("--json output not a JSON object; first 80 bytes: %q", out[:min(80, len(out))])
	}
	if strings.Contains(out, "# PR audit:") {
		t.Error("--json output should not contain Markdown header")
	}
}

func TestRealMain_RunErrorWithNilResultReturnsTwo(t *testing.T) {
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
	withRunOrchestraStub(t, func(_ context.Context, _ *orchestra.Config, _ ...orchestra.Option) (*orchestra.Result, error) {
		return nil, errors.New("workspace lock contention: orchestra: run already in progress for workspace")
	})

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := realMain(context.Background(), []string{"21"}, stdout, stderr)

	// nil result + non-nil err is the "engine never produced a result"
	// path — exit 2, not 1, because the audit didn't run.
	if code != exitSetupError {
		t.Errorf("exit = %d, want %d (setup error); stderr: %s", code, exitSetupError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "orchestra.Run failed before producing a result") {
		t.Error("stderr missing nil-result note")
	}
}

func TestRealMain_HelpFlagSetupError(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := realMain(context.Background(), []string{"--help"}, stdout, stderr)
	if code != exitSetupError {
		t.Errorf("exit = %d, want %d (--help is treated as a setup error)", code, exitSetupError)
	}
	if !strings.Contains(stderr.String(), "PR_AUDIT_MODEL") {
		t.Error("--help didn't surface PR_AUDIT_MODEL")
	}
	if !strings.Contains(stderr.String(), "PR_AUDIT_MAX_TURNS") {
		t.Error("--help didn't surface PR_AUDIT_MAX_TURNS")
	}
}
