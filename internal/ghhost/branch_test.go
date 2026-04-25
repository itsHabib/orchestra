package ghhost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dispatchOnEscapedPath returns an http.Handler that matches against
// r.URL.EscapedPath() (since Go 1.22 ServeMux routes on the escaped path
// and slashes inside path segments arrive as %2F).
func dispatchOnEscapedPath(t *testing.T, routes map[string]http.HandlerFunc) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := routes[r.URL.EscapedPath()]; ok {
			h(w, r)
			return
		}
		t.Logf("unrouted %s %s (escaped=%q)", r.Method, r.URL.RequestURI(), r.URL.EscapedPath())
		http.NotFound(w, r)
	})
}

func TestGetBranch_OK(t *testing.T) {
	srv := httptest.NewServer(dispatchOnEscapedPath(t, map[string]http.HandlerFunc{
		"/repos/octo/repo/branches/orchestra%2Fteam-a-1": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "token tok-1" {
				t.Errorf("auth header %q, want %q", got, "token tok-1")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"orchestra/team-a-1","commit":{"sha":"deadbeef0000"}}`))
		},
		"/repos/octo/repo/compare/main...orchestra%2Fteam-a-1": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"merge_base_commit":{"sha":"basebase0000"}}`))
		},
	}))
	defer srv.Close()

	c := New("tok-1", WithBase(srv.URL), WithHTTPClient(srv.Client()))
	b, err := c.GetBranch(context.Background(), "octo", "repo", "orchestra/team-a-1", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.CommitSHA != "deadbeef0000" || b.BaseSHA != "basebase0000" {
		t.Fatalf("unexpected branch %+v", b)
	}
}

func TestGetBranch_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/repo/branches/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Branch not found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("tok-1", WithBase(srv.URL), WithHTTPClient(srv.Client()))
	_, err := c.GetBranch(context.Background(), "octo", "repo", "missing", "main")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("expected ErrBranchNotFound, got %v", err)
	}
}

func TestGetBranch_TokenScrubbedOnAuthError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/repo/branches/x", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// echo back the token so we can prove the scrubber catches it.
		_, _ = w.Write([]byte(`{"message":"bad creds: ` + r.Header.Get("Authorization") + `"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	const token = "super-secret-pat-1234"
	c := New(token, WithBase(srv.URL), WithHTTPClient(srv.Client()))
	_, err := c.GetBranch(context.Background(), "octo", "repo", "x", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if contains(err.Error(), token) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
	if !contains(err.Error(), "***") {
		t.Fatalf("expected scrubber marker in %q", err.Error())
	}
}

func TestGetBranch_NoDefaultBranchLeavesBaseEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/repo/branches/feat", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"feat","commit":{"sha":"head1"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("t", WithBase(srv.URL), WithHTTPClient(srv.Client()))
	b, err := c.GetBranch(context.Background(), "octo", "repo", "feat", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.CommitSHA != "head1" {
		t.Fatalf("CommitSHA = %q, want head1", b.CommitSHA)
	}
	if b.BaseSHA != "" {
		t.Fatalf("BaseSHA must be empty when no default branch given, got %q", b.BaseSHA)
	}
}

func TestGetBranch_BranchNameWithSlashEscaped(t *testing.T) {
	const wantBranch = "orchestra/team-a-1"
	srv := httptest.NewServer(dispatchOnEscapedPath(t, map[string]http.HandlerFunc{
		"/repos/octo/repo/branches/orchestra%2Fteam-a-1": func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.RequestURI, "%2F") {
				t.Errorf("request URI %q should contain %%2F-escaped slash", r.RequestURI)
			}
			_, _ = w.Write([]byte(`{"name":"orchestra/team-a-1","commit":{"sha":"head1"}}`))
		},
	}))
	defer srv.Close()

	c := New("t", WithBase(srv.URL), WithHTTPClient(srv.Client()))
	b, err := c.GetBranch(context.Background(), "octo", "repo", wantBranch, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Name != wantBranch {
		t.Fatalf("name = %q", b.Name)
	}
}
