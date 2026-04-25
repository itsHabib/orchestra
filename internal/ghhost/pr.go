package ghhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// OpenPRRequest captures the fields needed to open a pull request.
type OpenPRRequest struct {
	Owner string
	Repo  string
	Head  string
	Base  string
	Title string
	Body  string
}

// PullRequest is the subset of the create-PR response we keep.
type PullRequest struct {
	URL    string
	Number int
}

type apiPRResponse struct {
	HTMLURL string `json:"html_url"`
	Number  int    `json:"number"`
}

type apiPROpenBody struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body,omitempty"`
}

// OpenPullRequest creates a pull request. When GitHub returns 422 with
// "already exists", the existing open PR is fetched and a
// *PullRequestExistsError (which wraps ErrPullRequestExists) is returned with
// the existing URL.
func (c *Client) OpenPullRequest(ctx context.Context, req *OpenPRRequest) (*PullRequest, error) {
	body, err := json.Marshal(apiPROpenBody{Title: req.Title, Head: req.Head, Base: req.Base, Body: req.Body})
	if err != nil {
		return nil, fmt.Errorf("encode pr body: %w", err)
	}

	var resp apiPRResponse
	err = c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", req.Owner, req.Repo), bytes.NewReader(body), &resp)
	if err == nil {
		return &PullRequest{URL: resp.HTMLURL, Number: resp.Number}, nil
	}
	if !isAlreadyExists(err) {
		return nil, fmt.Errorf("open pull request: %w", err)
	}

	existing, lookupErr := c.findOpenPR(ctx, req.Owner, req.Repo, req.Head, req.Base)
	if lookupErr != nil {
		return nil, fmt.Errorf("locate existing pr: %w", lookupErr)
	}
	return nil, &PullRequestExistsError{URL: existing.HTMLURL}
}

func (c *Client) findOpenPR(ctx context.Context, owner, repo, head, base string) (*apiPRResponse, error) {
	q := url.Values{}
	q.Set("state", "open")
	q.Set("head", owner+":"+head)
	q.Set("base", base)
	path := fmt.Sprintf("/repos/%s/%s/pulls?%s", owner, repo, q.Encode())
	var prs []apiPRResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, fmt.Errorf("no open pr found for %s:%s -> %s", owner, head, base)
	}
	return &prs[0], nil
}
