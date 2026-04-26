// pr-audit is a small CLI consumer of pkg/orchestra. Given a GitHub PR
// number, it fetches the PR metadata + diff via `gh`, builds an
// orchestra.Config in memory describing a single audit team, runs the
// audit synchronously via orchestra.Run, and prints a structured report
// to stdout.
//
// Two test seams are exposed at package scope:
//
//   - execCommand wraps exec.CommandContext so gh shell-outs can be
//     stubbed without booting a real `gh` binary.
//   - runOrchestra wraps orchestra.Run so the engine can be stubbed
//     with a fabricated *orchestra.Result for exit-code coverage.
//
// pr-audit is a sample SDK consumer, not a supported tool. The Markdown
// and JSON output shapes can change without notice; consumers depending
// on the JSON format should pin a commit SHA.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// Exit code constants match the design doc §F10 contract:
//
//   - exitSuccess: audit ran and every team finished with status "done".
//   - exitAuditFailure: audit ran but at least one team did not finish.
//     The report still rendered to stdout; the exit code lets CI scripts
//     gate on success without parsing JSON.
//   - exitSetupError: setup-level failure (missing PR number, gh
//     unavailable, validation failure, signal abort before any team
//     produced a result).
const (
	exitSuccess      = 0
	exitAuditFailure = 1
	exitSetupError   = 2
)

// runOrchestra is the test seam wrapping [orchestra.Run]. Production
// callers leave it pointing at orchestra.Run; tests substitute a stub
// returning a fabricated *orchestra.Result so the engine path is not
// exercised. NOT goroutine-safe; tests serialize.
var runOrchestra = orchestra.Run

// realMain runs the CLI against the given args (excluding argv[0]) and
// returns an exit code, writing report bytes to stdout and diagnostics
// to stderr. Extracted from main() so tests can drive it without
// process spawning.
func realMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed := parseFlags(args, stderr)
	if parsed.exitCode != 0 {
		return parsed.exitCode
	}
	prNumber, code := parsePR(parsed.fs, stderr)
	if code != 0 {
		return code
	}

	pr, err := fetchPR(ctx, prNumber)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pr-audit: fetch PR #%d: %v\n", prNumber, err)
		if errors.Is(err, ErrGhUnavailable) {
			_, _ = fmt.Fprintln(stderr, "pr-audit: hint — is `gh` installed and authenticated? Run `gh auth status` to check.")
		}
		return exitSetupError
	}

	cfg := buildConfig(&pr)
	vr := orchestra.Validate(cfg)
	if !vr.Valid() {
		_, _ = fmt.Fprintf(stderr, "pr-audit: built config failed validation: %v\n", vr.Err())
		return exitSetupError
	}
	for _, w := range vr.Warnings {
		_, _ = fmt.Fprintf(stderr, "pr-audit: warning: %s\n", w)
	}

	workspace := filepath.Join(".orchestra", "pr-audit-"+strconv.Itoa(prNumber))
	result, runErr := runOrchestra(ctx, cfg, orchestra.WithWorkspaceDir(workspace))

	// runErr+nil result: orchestra.Run failed before producing any
	// state — the audit didn't run. Skip the report (would emit a
	// degenerate empty document on stdout) and let decideExit write
	// the stderr explanation and return exit-2.
	if result == nil {
		return decideExit(result, runErr, stderr)
	}

	// runErr can be non-nil with a non-nil result — partial failure.
	// Both renderers handle a partial result gracefully, so render
	// before deciding the exit code.
	if err := writeReport(stdout, result, &pr, workspace, *parsed.jsonOut); err != nil {
		_, _ = fmt.Fprintf(stderr, "pr-audit: render report: %v\n", err)
		return exitAuditFailure
	}

	return decideExit(result, runErr, stderr)
}

// parsedFlags bundles the outputs of parseFlags. Returning a struct
// rather than three positional values keeps the gocritic
// unnamedResult and revive nonamedreturns rules both happy.
type parsedFlags struct {
	fs       *flag.FlagSet
	jsonOut  *bool
	exitCode int
}

// parseFlags handles flag parsing. exitCode is non-zero when parsing
// failed or the user passed -h / --help (treated as a setup-level
// usage exit).
func parseFlags(args []string, stderr io.Writer) parsedFlags {
	fs := flag.NewFlagSet("pr-audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "emit JSON instead of Markdown")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), `pr-audit — audit a GitHub PR via the orchestra SDK

USAGE
    pr-audit [--json] <PR-number>

ENV
    PR_AUDIT_MODEL       overrides the auditor's model. Default: %q.
    PR_AUDIT_MAX_TURNS   overrides per-team max_turns. Default: %d.

EXIT CODES
    0  audit ran; every team finished
    1  audit ran; at least one team failed (report still printed)
    2  setup error (missing arg, gh unavailable, validation failure)

`,
			defaultModel, defaultMaxTurns)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError has already written its error and
		// (for -h / --help) the usage block to stderr; bail.
		return parsedFlags{fs: fs, jsonOut: jsonOut, exitCode: exitSetupError}
	}
	return parsedFlags{fs: fs, jsonOut: jsonOut}
}

// parsePR reads the positional PR number from fs and validates it.
// Returns (number, 0) on success or (0, exitSetupError) on missing /
// non-numeric / non-positive input. The two int returns share the
// canonical Go (value, exit-code) shape; gocritic asks for named
// returns here, but the design-doc convention forbids named returns,
// so the lint rule is overridden inline rather than introducing an
// awkward struct.
//
//nolint:gocritic // see godoc above; named returns conflict with nonamedreturns
func parsePR(fs *flag.FlagSet, stderr io.Writer) (int, int) {
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "pr-audit: expected exactly one positional argument: <PR-number>")
		fs.Usage()
		return 0, exitSetupError
	}
	prNumber, err := strconv.Atoi(fs.Arg(0))
	if err != nil || prNumber <= 0 {
		_, _ = fmt.Fprintf(stderr, "pr-audit: invalid PR number %q: must be a positive integer\n", fs.Arg(0))
		return 0, exitSetupError
	}
	return prNumber, 0
}

// decideExit maps (*Result, runErr) to the final exit code per §F10.
// runErr+nil result means orchestra.Run failed before producing any
// state — exitSetupError. runErr+non-nil result means partial run with
// at least one failed team — exitAuditFailure. No runErr and every
// team done is exitSuccess; anything else (e.g. a team stuck "running"
// from a misbehaving stub) is exitAuditFailure. Status determination
// is delegated to summarize so this function and the renderers stay
// in sync.
func decideExit(result *orchestra.Result, runErr error, stderr io.Writer) int {
	switch {
	case runErr != nil && result == nil:
		_, _ = fmt.Fprintf(stderr, "pr-audit: orchestra.Run failed before producing a result: %v\n", runErr)
		return exitSetupError
	case runErr != nil:
		_, _ = fmt.Fprintf(stderr, "pr-audit: orchestra.Run reported failure: %v\n", runErr)
		return exitAuditFailure
	case summarize(result).Status == "success":
		return exitSuccess
	default:
		return exitAuditFailure
	}
}

// writeReport dispatches to renderJSON or renderMarkdown and writes the
// bytes to stdout. Errors here are unusual — JSON marshaling of the
// flat schema doesn't fail in practice — but surfaced via the return
// so realMain can decide how to react.
func writeReport(stdout io.Writer, result *orchestra.Result, pr *PRData, workspace string, jsonOut bool) error {
	now := time.Now()
	if jsonOut {
		out, err := renderJSON(result, pr, workspace, now)
		if err != nil {
			return err
		}
		// Append a trailing newline so terminals don't show the next
		// prompt on the same line as the closing brace.
		if _, err := fmt.Fprintln(stdout, string(out)); err != nil {
			return fmt.Errorf("write JSON report: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprint(stdout, renderMarkdown(result, pr, workspace, now)); err != nil {
		return fmt.Errorf("write Markdown report: %w", err)
	}
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := realMain(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
