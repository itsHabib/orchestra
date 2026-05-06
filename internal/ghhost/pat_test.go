package ghhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// recordingWarn captures every emitted warning for assertion. Concurrent-
// safe so resolvePAT internals can grow callers without breaking the
// tests.
type recordingWarn struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingWarn) emit(format string, args ...any) {
	rendered := fmt.Sprintf(format, args...)
	r.mu.Lock()
	r.calls = append(r.calls, rendered)
	r.mu.Unlock()
}

func (r *recordingWarn) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func TestResolvePAT_EnvWins(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "env-token")
	rec := &recordingWarn{}
	got, err := resolvePAT(
		context.Background(),
		func() (string, error) { return "", errors.New("should not be called") },
		func() string { return "" },
		rec.emit,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "env-token" {
		t.Fatalf("got %q, want %q", got, "env-token")
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("env path should not warn, got %v", calls)
	}
}

// TestResolvePAT_CredentialsFile covers the canonical Phase A path:
// orchestra.credentials.json holds the token, no env var, no legacy
// config.json. Returns the value cleanly with no deprecation warning.
func TestResolvePAT_CredentialsFile(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	credsPath := writeCredentials(t, map[string]string{"github_token": "creds-token"})
	rec := &recordingWarn{}

	got, err := resolvePAT(
		context.Background(),
		func() (string, error) {
			return filepath.Join(filepath.Dir(credsPath), "missing-config.json"), nil
		},
		func() string { return credsPath },
		rec.emit,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "creds-token" {
		t.Fatalf("got %q, want %q", got, "creds-token")
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("credentials.json path should not warn, got %v", calls)
	}
}

// TestResolvePAT_CredentialsFileWinsOverConfig pins precedence: when both
// files have the token, credentials.json wins and no deprecation warning
// fires (the operator already migrated; the legacy config.json entry is
// just dead weight, not an error).
func TestResolvePAT_CredentialsFileWinsOverConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	credsPath := writeCredentials(t, map[string]string{"github_token": "creds-token"})
	configPath := writeConfig(t, `{"github_token":"config-token"}`)
	rec := &recordingWarn{}

	got, err := resolvePAT(
		context.Background(),
		func() (string, error) { return configPath, nil },
		func() string { return credsPath },
		rec.emit,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "creds-token" {
		t.Fatalf("got %q, want creds-token (precedence)", got)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no warning when credentials.json wins, got %v", calls)
	}
}

// TestResolvePAT_ConfigFileEmitsDeprecation covers the legacy fallback
// path: only config.json has the token. Returns the value AND emits the
// one-shot deprecation warning so the operator knows to migrate.
func TestResolvePAT_ConfigFileEmitsDeprecation(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	credsPath := filepath.Join(t.TempDir(), "credentials.json") // not created
	configPath := writeConfig(t, `{"github_token":"config-token"}`)
	rec := &recordingWarn{}

	got, err := resolvePAT(
		context.Background(),
		func() (string, error) { return configPath, nil },
		func() string { return credsPath },
		rec.emit,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "config-token" {
		t.Fatalf("got %q, want config-token", got)
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one deprecation warning, got %d: %v", len(calls), calls)
	}
	for _, want := range []string{"deprecated", configPath, credsPath} {
		if !contains(calls[0], want) {
			t.Errorf("warning text %q missing %q", calls[0], want)
		}
	}
}

// TestResolvePAT_MissingBoth covers the no-source-set path: error message
// must name all three lookup sites (env, credentials.json, config.json)
// so the operator can pick one.
func TestResolvePAT_MissingBoth(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	credsPath := filepath.Join(tmp, "credentials.json") // not created
	configPath := filepath.Join(tmp, "config.json")     // not created
	rec := &recordingWarn{}

	_, err := resolvePAT(
		context.Background(),
		func() (string, error) { return configPath, nil },
		func() string { return credsPath },
		rec.emit,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrPATMissing) {
		t.Fatalf("expected ErrPATMissing, got %v", err)
	}
	for _, want := range []string{"GITHUB_TOKEN", credsPath, configPath} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q should reference %q", err.Error(), want)
		}
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("missing-both path should not warn, got %v", calls)
	}
}

// TestResolvePAT_EmptyTokenInConfig covers the config-file-present-but-
// empty-token case: same shape as missing both. No warning fires (the
// fallback never won).
func TestResolvePAT_EmptyTokenInConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	credsPath := filepath.Join(t.TempDir(), "credentials.json") // not created
	configPath := writeConfig(t, `{"api_key":"only-anth-key"}`)
	rec := &recordingWarn{}

	_, err := resolvePAT(
		context.Background(),
		func() (string, error) { return configPath, nil },
		func() string { return credsPath },
		rec.emit,
	)
	if err == nil || !errors.Is(err, ErrPATMissing) {
		t.Fatalf("expected ErrPATMissing, got %v", err)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("empty-token-in-config path should not warn, got %v", calls)
	}
}

// TestResolvePAT_MalformedJSON covers a corrupt config.json: surface the
// parse error directly (not ErrPATMissing) so the operator knows to fix
// the file rather than thinking the token is just absent.
func TestResolvePAT_MalformedJSON(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	credsPath := filepath.Join(t.TempDir(), "credentials.json") // not created
	configPath := writeConfig(t, `not json`)

	_, err := resolvePAT(
		context.Background(),
		func() (string, error) { return configPath, nil },
		func() string { return credsPath },
		nil,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "parse") {
		t.Fatalf("error %q should mention parse failure", err.Error())
	}
}

// writeCredentials writes a credentials.json file at a fresh temp path
// with mode 0600. Returns the path so tests can inject it via getCredsPath.
func writeCredentials(t *testing.T, creds map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	data := []byte("{")
	first := true
	for k, v := range creds {
		if !first {
			data = append(data, ',')
		}
		first = false
		data = append(data, '"')
		data = append(data, k...)
		data = append(data, '"', ':', '"')
		data = append(data, v...)
		data = append(data, '"')
	}
	data = append(data, '}')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return path
}

// writeConfig writes a config.json file at a fresh temp path with the
// given JSON body and mode 0600.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
