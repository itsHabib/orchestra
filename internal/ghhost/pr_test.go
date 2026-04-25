package ghhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenPullRequest_Created(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %q, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var got map[string]string
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		if got["title"] != "Hello" || got["head"] != "feat" || got["base"] != "main" {
			t.Errorf("body %+v missing expected fields", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/octo/repo/pull/12","number":12}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("t", WithBase(srv.URL), WithHTTPClient(srv.Client()))
	pr, err := c.OpenPullRequest(context.Background(), &OpenPRRequest{
		Owner: "octo", Repo: "repo", Head: "feat", Base: "main", Title: "Hello", Body: "ignored",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr.URL != "https://github.com/octo/repo/pull/12" || pr.Number != 12 {
		t.Fatalf("unexpected pr: %+v", pr)
	}
}

func TestOpenPullRequest_AlreadyExists(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"A pull request already exists for octo:feat."}]}`))
		case http.MethodGet:
			if !strings.Contains(r.URL.RawQuery, "head=octo%3Afeat") {
				t.Errorf("expected head=octo:feat in query, got %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"html_url":"https://github.com/octo/repo/pull/7","number":7}]`))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("t", WithBase(srv.URL), WithHTTPClient(srv.Client()))
	_, err := c.OpenPullRequest(context.Background(), &OpenPRRequest{
		Owner: "octo", Repo: "repo", Head: "feat", Base: "main", Title: "Hi",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrPullRequestExists) {
		t.Fatalf("expected ErrPullRequestExists, got %v", err)
	}
	var existing *PullRequestExistsError
	if !errors.As(err, &existing) || existing.URL != "https://github.com/octo/repo/pull/7" {
		t.Fatalf("expected PullRequestExistsError with url, got %v", err)
	}
}

func TestOpenPullRequest_OtherFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/repo/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("t", WithBase(srv.URL), WithHTTPClient(srv.Client()))
	_, err := c.OpenPullRequest(context.Background(), &OpenPRRequest{Owner: "octo", Repo: "repo", Head: "x", Base: "main"})
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrPullRequestExists) {
		t.Fatalf("non-422 must not be classified as already-exists, got %v", err)
	}
}
