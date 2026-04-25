package ghhost

import (
	"strings"
	"testing"
)

func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		name, in    string
		owner, repo string
		wantErr     bool
		errContains string
	}{
		{name: "canonical https", in: "https://github.com/itsHabib/orchestra", owner: "itsHabib", repo: "orchestra"},
		{name: "with .git suffix", in: "https://github.com/itsHabib/orchestra.git", owner: "itsHabib", repo: "orchestra"},
		{name: "trailing slash", in: "https://github.com/itsHabib/orchestra/", owner: "itsHabib", repo: "orchestra"},
		{name: "uppercase host", in: "https://GitHub.com/itsHabib/orchestra", owner: "itsHabib", repo: "orchestra"},
		{name: "ssh rejected", in: "git@github.com:itsHabib/orchestra.git", wantErr: true, errContains: "ssh"},
		{name: "ssh scheme rejected", in: "ssh://git@github.com/itsHabib/orchestra.git", wantErr: true, errContains: "ssh"},
		{name: "non-github host", in: "https://gitlab.com/foo/bar", wantErr: true, errContains: "github.com"},
		{name: "extra path components", in: "https://github.com/itsHabib/orchestra/tree/main", wantErr: true, errContains: "owner/repo"},
		{name: "empty path", in: "https://github.com/", wantErr: true, errContains: "owner/repo"},
		{name: "empty url", in: "", wantErr: true, errContains: "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, err := ParseRepoURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (owner=%q repo=%q)", owner, repo)
				}
				if tc.errContains != "" && !contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q missing %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tc.owner || repo != tc.repo {
				t.Fatalf("got %q/%q, want %q/%q", owner, repo, tc.owner, tc.repo)
			}
		})
	}
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	return strings.Contains(s, sub)
}
