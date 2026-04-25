package ghhost

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Branch is the subset of GitHub's branch metadata we keep. CommitSHA is the
// branch head; BaseSHA is the merge-base against the repo default branch when
// it could be resolved via the compare endpoint, and empty otherwise. Callers
// must not assume BaseSHA equality with CommitSHA implies "no commits" unless
// they have separately verified BaseSHA was resolved.
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
// and differs from branch, BaseSHA is filled from compare(default...branch);
// it is left empty when the compare endpoint cannot resolve a merge-base.
// Returns an error wrapping ErrBranchNotFound on 404.
func (c *Client) GetBranch(ctx context.Context, owner, repo, branch, defaultBranch string) (*Branch, error) {
	var b apiBranch
	branchPath := fmt.Sprintf("/repos/%s/%s/branches/%s", owner, repo, url.PathEscape(branch))
	if err := c.do(ctx, http.MethodGet, branchPath, nil, &b); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	out := &Branch{Name: b.Name, CommitSHA: b.Commit.SHA}

	if defaultBranch != "" && defaultBranch != branch {
		var cmp apiCompare
		comparePath := fmt.Sprintf("/repos/%s/%s/compare/%s...%s", owner, repo, url.PathEscape(defaultBranch), url.PathEscape(branch))
		err := c.do(ctx, http.MethodGet, comparePath, nil, &cmp)
		if err != nil && !isNotFound(err) {
			return nil, fmt.Errorf("compare branches: %w", err)
		}
		if cmp.MergeBaseCommit.SHA != "" {
			out.BaseSHA = cmp.MergeBaseCommit.SHA
		}
	}
	return out, nil
}
