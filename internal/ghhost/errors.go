package ghhost

import "errors"

// ErrBranchNotFound is returned by Client.GetBranch when the requested branch
// does not exist on the remote.
var ErrBranchNotFound = errors.New("branch not found")

// ErrPATMissing is returned by ResolvePAT when no GitHub token is configured.
var ErrPATMissing = errors.New("github pat not configured")

// ErrPullRequestExists is returned by Client.OpenPullRequest (wrapped in a
// *PullRequestExistsError carrying the URL) when an open PR already exists for
// the head:base pair.
var ErrPullRequestExists = errors.New("pull request already exists")

// PullRequestExistsError is the typed companion to ErrPullRequestExists.
// Callers use errors.As to recover the existing PR's URL.
type PullRequestExistsError struct {
	URL string
}

// Error implements error.
func (e *PullRequestExistsError) Error() string {
	return "pull request already exists: " + e.URL
}

// Unwrap returns ErrPullRequestExists so errors.Is matches.
func (e *PullRequestExistsError) Unwrap() error {
	return ErrPullRequestExists
}
