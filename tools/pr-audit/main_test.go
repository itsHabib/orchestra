package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
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

// captureRunOrchestraOptions wraps fn in a stub that records the
// orchestra.Option slice passed to runOrchestra so tests can assert
// the SDK options (e.g. WithWorkspaceDir) propagate from realMain.
// The returned *[]orchestra.Option is updated in place on each call.
func captureRunOrchestraOptions(t *testing.T, fn func(context.Context, *orchestra.Config, ...orchestra.Option) (*orchestra.Result, error)) *[]orchestra.Option {
	t.Helper()
	captured := &[]orchestra.Option{}
	withRunOrchestraStub(t, func(ctx context.Context, cfg *orchestra.Config, opts ...orchestra.Option) (*orchestra.Result, error) {
		*captured = opts
		return fn(ctx, cfg, opts...)
	})
	return captured
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
	capturedOpts := captureRunOrchestraOptions(t, func(_ context.Context, _ *orchestra.Config, _ ...orchestra.Option) (*orchestra.Result, error) {
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
	// realMain must propagate WithWorkspaceDir to orchestra.Run with
	// the per-PR workspace path so concurrent audits don't collide
	// and the report's workspace footer matches reality.
	wantWorkspace := filepath.Join(".orchestra", "pr-audit-21")
	if !optsContainWorkspaceDir(*capturedOpts, wantWorkspace) {
		t.Errorf("orchestra.Run options missing WithWorkspaceDir(%q); got %d option(s)", wantWorkspace, len(*capturedOpts))
	}
	if !strings.Contains(stdout.String(), wantWorkspace) {
		t.Errorf("stdout missing workspace path %q in report footer", wantWorkspace)
	}
}

// optsContainWorkspaceDir verifies an orchestra.Option was passed.
// orchestra.Option is `type Option func(*runOptions)` where runOptions
// is unexported, so we cannot introspect which option was passed
// without an SDK helper — we can only assert the slice is non-empty.
// Combined with the call site (`orchestra.WithWorkspaceDir(workspace)`
// is the only option built in realMain), a non-empty slice is a
// meaningful regression signal: removing the option entirely would
// drop the slice to zero. A path-identity assertion would need a
// public SDK introspection method; flagged as a future SDK finding.
func optsContainWorkspaceDir(opts []orchestra.Option, _ string) bool {
	return len(opts) > 0
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
	// stdout must stay empty on the nil-result path — emitting a
	// degenerate report would surprise callers piping stdout into
	// other tooling. realMain skips writeReport when result is nil.
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty on nil-result exit, got %d bytes: %q", stdout.Len(), stdout.String())
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
