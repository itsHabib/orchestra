package ghhost

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePAT_EnvWins(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "env-token")
	got, err := resolvePAT(func() (string, error) { return "", errors.New("should not be called") })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "env-token" {
		t.Fatalf("got %q, want %q", got, "env-token")
	}
}

func TestResolvePAT_ConfigFile(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"github_token":"file-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePAT(func() (string, error) { return path, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "file-token" {
		t.Fatalf("got %q, want %q", got, "file-token")
	}
}

func TestResolvePAT_MissingBoth(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.json")
	_, err := resolvePAT(func() (string, error) { return path, nil })
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrPATMissing) {
		t.Fatalf("expected ErrPATMissing, got %v", err)
	}
	if !contains(err.Error(), "GITHUB_TOKEN") || !contains(err.Error(), "github_token") {
		t.Fatalf("error %q should reference both env var and config field", err.Error())
	}
}

func TestResolvePAT_EmptyTokenInConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"only-anth-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePAT(func() (string, error) { return path, nil })
	if err == nil || !errors.Is(err, ErrPATMissing) {
		t.Fatalf("expected ErrPATMissing, got %v", err)
	}
}

func TestResolvePAT_MalformedJSON(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePAT(func() (string, error) { return path, nil })
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "parse") {
		t.Fatalf("error %q should mention parse failure", err.Error())
	}
}
