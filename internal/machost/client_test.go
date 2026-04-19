package machost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveAPIKey_EnvVarWins(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-from-env")

	// Make the config-path lookup return a path that exists with a different
	// key, to prove env var takes precedence.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	writeConfig(t, path, `{"api_key":"sk-ant-from-file"}`)

	got, err := resolveAPIKey(func() (string, error) { return path, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sk-ant-from-env" {
		t.Fatalf("env var should win; got %q", got)
	}
}

func TestResolveAPIKey_ConfigFileFallback(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	writeConfig(t, path, `{"api_key":"sk-ant-from-file"}`)

	got, err := resolveAPIKey(func() (string, error) { return path, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sk-ant-from-file" {
		t.Fatalf("got %q, want sk-ant-from-file", got)
	}
}

func TestResolveAPIKey_MissingEverywhere(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	tmp := t.TempDir()
	path := filepath.Join(tmp, "does-not-exist.json")

	_, err := resolveAPIKey(func() (string, error) { return path, nil })
	if err == nil {
		t.Fatal("expected error when neither env nor file provide a key")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ANTHROPIC_API_KEY") {
		t.Fatalf("error should mention ANTHROPIC_API_KEY; got: %v", err)
	}
	if !strings.Contains(msg, path) {
		t.Fatalf("error should mention the config path; got: %v", err)
	}
}

func TestResolveAPIKey_EmptyFieldInFile(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	writeConfig(t, path, `{"api_key":""}`)

	_, err := resolveAPIKey(func() (string, error) { return path, nil })
	if err == nil {
		t.Fatal("expected error on empty api_key field")
	}
	if !strings.Contains(err.Error(), "populate") {
		t.Fatalf("error should hint at populating the field; got: %v", err)
	}
}

func TestResolveAPIKey_MalformedFile(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	writeConfig(t, path, `{not json`)

	_, err := resolveAPIKey(func() (string, error) { return path, nil })
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error should mention parse failure; got: %v", err)
	}
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
