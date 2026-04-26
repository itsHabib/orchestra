package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// execCommand is the test seam wrapping [exec.CommandContext]. Production
// callers leave it pointing at [exec.CommandContext]; tests substitute a
// stub that returns canned bytes from testdata.
//
// NOT goroutine-safe: tests that mutate this var must run sequentially.
// Today the test suite is single-threaded; before adding parallel tests
// that touch fetchPR, switch to per-call injection.
var execCommand = exec.CommandContext

// PRData is the parsed `gh pr view` payload joined with the raw `gh pr
// diff` output. Captured once via fetchPR and threaded through the rest
// of the program (config builder, report renderers).
type PRData struct {
	Number      int
	Title       string
	Body        string
	URL         string
	BaseRefName string
	HeadRefName string
	Additions   int
	Deletions   int
	Files       []GHFile
	Diff        string
}

// GHFile captures the per-file change summary from `gh pr view --json
// files`.
type GHFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// ErrGhUnavailable wraps any non-zero `gh` exit, missing-binary failure,
// or authentication error. Surfaces from main as exit code 2 with a hint
// about installing or authenticating gh.
var ErrGhUnavailable = errors.New("gh CLI unavailable")

// ghViewPayload is the intermediate shape for `gh pr view --json ...`.
type ghViewPayload struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	URL         string   `json:"url"`
	BaseRefName string   `json:"baseRefName"`
	HeadRefName string   `json:"headRefName"`
	Additions   int      `json:"additions"`
	Deletions   int      `json:"deletions"`
	Files       []GHFile `json:"files"`
}

// ghJSONFields is the canonical --json field list passed to `gh pr view`.
// Kept centralized so the test fixture (capture command) and the
// production call stay in sync.
const ghJSONFields = "title,body,files,additions,deletions,baseRefName,headRefName,url"

// fetchPR shells to `gh pr view <PR> --json ...` and `gh pr diff <PR>`
// and returns the merged [PRData]. Errors wrap [ErrGhUnavailable] with
// the captured stderr so the caller can print a single hint regardless
// of which underlying step failed.
func fetchPR(ctx context.Context, pr int) (PRData, error) {
	prStr := strconv.Itoa(pr)

	viewBytes, err := runGh(ctx, "pr", "view", prStr, "--json", ghJSONFields)
	if err != nil {
		return PRData{}, err
	}
	var view ghViewPayload
	if err := json.Unmarshal(viewBytes, &view); err != nil {
		return PRData{}, fmt.Errorf("parse `gh pr view` output: %w", err)
	}

	diffBytes, err := runGh(ctx, "pr", "diff", prStr)
	if err != nil {
		return PRData{}, err
	}

	return PRData{
		Number:      pr,
		Title:       view.Title,
		Body:        view.Body,
		URL:         view.URL,
		BaseRefName: view.BaseRefName,
		HeadRefName: view.HeadRefName,
		Additions:   view.Additions,
		Deletions:   view.Deletions,
		Files:       view.Files,
		Diff:        string(diffBytes),
	}, nil
}

// runGh invokes the gh CLI with the given args via the [execCommand]
// seam. Returns combined stdout. On non-zero exit or exec failure,
// wraps [ErrGhUnavailable] with stderr so the caller renders a single
// hint message.
func runGh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := execCommand(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: gh %v: %v: %s", ErrGhUnavailable, args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}
