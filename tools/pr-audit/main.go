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
	"fmt"
	"os"
)

func main() {
	// The full main implementation is wired in a later commit; this
	// stub exists so the gh.go fetcher and its tests compile in the
	// scaffolding commit.
	fmt.Fprintln(os.Stderr, "pr-audit: not yet wired (scaffold commit)")
	os.Exit(2)
}
