package ghhost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetBranch_OK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/repo/branches/orchestra/team-a-1", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token tok-1" {
			t.Errorf("auth header %q, want %q", got, "token tok-1")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"orchestra/team-a-1","commit":{"sha":"deadbeef0000"}}`))
	})
	mux.HandleFunc("/repos/octo/repo/compare/main...orchestra/team-a-1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"merge_base_commit":{"sha":"basebase0000"}}`))
	})
	srv := httptest.NewServer(mux)
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

func TestGetBranch_NoDefaultBranchFallsBackToHead(t *testing.T) {
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
	if b.BaseSHA != "head1" || b.CommitSHA != "head1" {
		t.Fatalf("base should fall back to head when no default branch given, got %+v", b)
	}
}
