package ghhost

import (
	"context"
	"fmt"
	"net/http"
)

// Branch is the subset of GitHub's branch metadata we keep. CommitSHA is the
// branch head; BaseSHA is the merge-base against the repo default branch
// (resolved via the compare endpoint when available, falling back to the head
// SHA when no comparison is possible).
type Branch struct {
	Name      string
	CommitSHA string
	BaseSHA   string
}

type apiBranch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type apiCompare struct {
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
}

// GetBranch fetches owner/repo/branches/<branch>. When defaultBranch is set
// and differs from branch, BaseSHA is filled from compare(default...branch).
// Returns an error wrapping ErrBranchNotFound on 404.
func (c *Client) GetBranch(ctx context.Context, owner, repo, branch, defaultBranch string) (*Branch, error) {
	var b apiBranch
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/branches/%s", owner, repo, branch), nil, &b); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	out := &Branch{Name: b.Name, CommitSHA: b.Commit.SHA, BaseSHA: b.Commit.SHA}

	if defaultBranch != "" && defaultBranch != branch {
		var cmp apiCompare
		err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/compare/%s...%s", owner, repo, defaultBranch, branch), nil, &cmp)
		if err != nil && !isNotFound(err) {
			return nil, fmt.Errorf("compare branches: %w", err)
		}
		if cmp.MergeBaseCommit.SHA != "" {
			out.BaseSHA = cmp.MergeBaseCommit.SHA
		}
	}
	return out, nil
}
