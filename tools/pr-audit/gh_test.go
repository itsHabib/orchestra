package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelperProcess is the canonical Go pattern for stubbing exec.Cmd:
// the test binary is re-invoked with GO_HELPER_PROCESS=1 plus a payload
// describing what stdout/stderr/exit to produce. The actual test code
// substitutes [execCommand] with a wrapper that arranges this.
//
// See https://npf.io/2015/06/testing-exec-command/ for the original
// write-up of the pattern.
func TestHelperProcess(_ *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		return
	}

	stdoutPath := os.Getenv("HELPER_STDOUT_FILE")
	if stdoutPath != "" {
		data, err := os.ReadFile(stdoutPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: read stdout file:", err)
			os.Exit(2)
		}
		if _, err := os.Stdout.Write(data); err != nil {
			fmt.Fprintln(os.Stderr, "helper: write stdout:", err)
			os.Exit(2)
		}
	}
	if stderr := os.Getenv("HELPER_STDERR"); stderr != "" {
		fmt.Fprint(os.Stderr, stderr)
	}
	if code := os.Getenv("HELPER_EXIT"); code != "" {
		os.Exit(asExitCode(code))
	}
	os.Exit(0)
}

func asExitCode(s string) int {
	switch s {
	case "1":
		return 1
	case "2":
		return 2
	default:
		return 0
	}
}

// execStub is a fakeable version of exec.CommandContext. Each call to
// the returned function is matched in order against the configured
// stdoutPaths / exit codes.
type execStub struct {
	calls []execStubCall
	idx   int
}

type execStubCall struct {
	wantArgs   []string // expected args, including "gh"
	stdoutFile string   // path to a file whose contents become stdout
	stderr     string
	exit       string
}

func newExecStub(calls ...execStubCall) *execStub {
	return &execStub{calls: calls}
}

// commandContext is the exec.CommandContext-compatible function the
// stub returns. Each invocation pulls the next configured call and
// re-execs the test binary into [TestHelperProcess] with the recorded
// stdout/stderr/exit code.
func (s *execStub) commandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	if s.idx >= len(s.calls) {
		// Fall through with an empty call so the test sees a
		// recognizable failure rather than panicking on an out-of-bounds
		// access. The test asserts s.idx == len(s.calls) at the end.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_HELPER_PROCESS=1", "HELPER_EXIT=2", "HELPER_STDERR=stub: unexpected extra call")
		return cmd
	}
	call := s.calls[s.idx]
	s.idx++

	gotArgs := append([]string{name}, arg...)
	if call.wantArgs != nil && !equalSlices(gotArgs, call.wantArgs) {
		// Surface the mismatch via the helper so the test sees a
		// non-zero exit with an error message rather than a silent pass.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_HELPER_PROCESS=1", "HELPER_EXIT=2",
			"HELPER_STDERR=stub: arg mismatch: got "+strings.Join(gotArgs, " ")+
				" want "+strings.Join(call.wantArgs, " "))
		return cmd
	}

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess", "--")
	env := append(os.Environ(), "GO_HELPER_PROCESS=1")
	if call.stdoutFile != "" {
		env = append(env, "HELPER_STDOUT_FILE="+call.stdoutFile)
	}
	if call.stderr != "" {
		env = append(env, "HELPER_STDERR="+call.stderr)
	}
	if call.exit != "" {
		env = append(env, "HELPER_EXIT="+call.exit)
	}
	cmd.Env = env
	return cmd
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// withExecStub installs the stub on the package-level execCommand var
// and registers a t.Cleanup that restores the original. Tests should
// call it before exercising fetchPR.
func withExecStub(t *testing.T, calls ...execStubCall) *execStub {
	t.Helper()
	stub := newExecStub(calls...)
	orig := execCommand
	execCommand = stub.commandContext
	t.Cleanup(func() { execCommand = orig })
	return stub
}

func TestFetchPR_ParsesFixture(t *testing.T) {
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

	pr, err := fetchPR(context.Background(), 21)
	if err != nil {
		t.Fatalf("fetchPR returned error: %v", err)
	}

	if pr.Number != 21 {
		t.Errorf("Number = %d, want 21", pr.Number)
	}
	if pr.Title == "" {
		t.Error("Title is empty")
	}
	if pr.URL == "" {
		t.Error("URL is empty")
	}
	if pr.BaseRefName == "" {
		t.Error("BaseRefName is empty")
	}
	if pr.HeadRefName == "" {
		t.Error("HeadRefName is empty")
	}
	if pr.Additions == 0 {
		t.Error("Additions is zero")
	}
	if len(pr.Files) == 0 {
		t.Error("Files is empty")
	}
	if !strings.Contains(pr.Diff, "diff --git") {
		t.Errorf("Diff missing `diff --git` marker; first 60 bytes: %q", firstN(pr.Diff, 60))
	}
	if stub.idx != len(stub.calls) {
		t.Errorf("execStub: %d calls consumed, want %d", stub.idx, len(stub.calls))
	}
}

func TestFetchPR_GhFailureWrapsErrGhUnavailable(t *testing.T) {
	withExecStub(t,
		execStubCall{
			wantArgs: []string{"gh", "pr", "view", "21", "--json", ghJSONFields},
			stderr:   "gh: not authenticated\n",
			exit:     "1",
		},
	)

	_, err := fetchPR(context.Background(), 21)
	if err == nil {
		t.Fatal("fetchPR succeeded; want error")
	}
	if !errors.Is(err, ErrGhUnavailable) {
		t.Errorf("error = %v, want errors.Is(_, ErrGhUnavailable)", err)
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("error message = %q, want stderr forwarded", err.Error())
	}
}

func TestFetchPR_DiffFailureWrapsErrGhUnavailable(t *testing.T) {
	withExecStub(t,
		execStubCall{
			wantArgs:   []string{"gh", "pr", "view", "21", "--json", ghJSONFields},
			stdoutFile: "testdata/pr-21.json",
		},
		execStubCall{
			wantArgs: []string{"gh", "pr", "diff", "21"},
			stderr:   "gh: rate limit exceeded\n",
			exit:     "1",
		},
	)

	_, err := fetchPR(context.Background(), 21)
	if err == nil {
		t.Fatal("fetchPR succeeded; want error")
	}
	if !errors.Is(err, ErrGhUnavailable) {
		t.Errorf("error = %v, want errors.Is(_, ErrGhUnavailable)", err)
	}
}

func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
