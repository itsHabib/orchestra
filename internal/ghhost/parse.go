package ghhost

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ParseRepoURL splits a canonical https://github.com/<owner>/<repo>(.git)? URL
// into owner and repo. Trailing ".git" is stripped. SSH and other hosts are
// rejected.
//
//nolint:gocritic // owner/repo/error is the natural shape; project policy disallows named returns.
func ParseRepoURL(repoURL string) (string, string, error) {
	if repoURL == "" {
		return "", "", errors.New("empty repository url")
	}
	if strings.HasPrefix(repoURL, "git@") || strings.HasPrefix(repoURL, "ssh://") {
		return "", "", errors.New("ssh repository urls are not supported")
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "https" {
		return "", "", fmt.Errorf("unsupported scheme %q (only https; the PAT must not be sent in cleartext)", u.Scheme)
	}
	host := strings.ToLower(u.Host)
	if host != "github.com" && host != "www.github.com" {
		return "", "", fmt.Errorf("unsupported host %q (only github.com)", u.Host)
	}
	path := strings.Trim(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid github path %q (want owner/repo)", u.Path)
	}
	return parts[0], parts[1], nil
}
