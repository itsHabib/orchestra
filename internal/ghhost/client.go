// Package ghhost is the host-side GitHub API client used by orchestra to
// resolve pushed branches after managed-agents sessions end and optionally
// to open pull requests. It is not exposed to agent containers; the personal
// access token lives only in process memory and on outgoing request headers.
package ghhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultBase = "https://api.github.com"

// httpDoer is the minimal interface Client needs. *http.Client satisfies it;
// tests pass an httptest.Server's Client.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is the orchestra-internal GitHub API client. Narrow by design: it
// exposes only the endpoints P1.5 needs.
type Client struct {
	http  httpDoer
	token string
	base  string
}

// Option configures a Client.
type Option func(*Client)

// WithBase overrides the GitHub API base URL. Used by tests targeting an
// httptest.Server; production callers omit it.
func WithBase(base string) Option {
	return func(c *Client) {
		c.base = strings.TrimRight(base, "/")
	}
}

// WithHTTPClient overrides the HTTP doer. Production callers rely on the
// default 30s-timeout *http.Client.
func WithHTTPClient(d httpDoer) Option {
	return func(c *Client) {
		c.http = d
	}
}

// New constructs a Client. token must be a personal access token with repo
// (private) or public_repo (public) scope.
func New(token string, opts ...Option) *Client {
	c := &Client{
		http:  &http.Client{Timeout: 30 * time.Second},
		token: token,
		base:  defaultBase,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Token returns the PAT this Client was constructed with. Callers use it to
// scrub error messages outside of the package; the Client itself never logs
// the token.
func (c *Client) Token() string { return c.token }

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return ScrubError(err, c.token)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return ScrubError(parseAPIError(resp.StatusCode, data), c.token)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
