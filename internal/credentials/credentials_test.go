package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return New(filepath.Join(dir, FileName))
}

func TestRead_NotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Read(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read err=%v, want ErrNotFound", err)
	}
}

func TestSetAndRead_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Set(ctx, "github_token", "ghp_secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got["github_token"] != "ghp_secret" {
		t.Fatalf("github_token = %q, want ghp_secret", got["github_token"])
	}
}

func TestWrite_Mode0600OnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod has no NTFS equivalent on Windows; mode is best-effort there")
	}
	s := newTestStore(t)
	if err := s.Set(context.Background(), "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o, want 0600", mode)
	}
}

func TestRead_PermissiveFileWarns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission checks are POSIX-only; Windows skips with a doc note")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`{"k":"v"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var warned bool
	s := New(path, WithWarn(func(string, ...any) { warned = true }))
	got, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got["k"] != "v" {
		t.Fatalf("got = %v", got)
	}
	if !warned {
		t.Fatal("expected a warning for permissive mode 0644")
	}
}

func TestDelete_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Delete on a missing file is a no-op.
	if err := s.Delete(ctx, "ghost"); err != nil {
		t.Fatalf("Delete on empty store: %v", err)
	}
	if err := s.Set(ctx, "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Delete(ctx, "ghost"); err != nil {
		t.Fatalf("Delete unknown: %v", err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, ok := got["k"]; ok {
		t.Fatalf("k should be gone, got %v", got)
	}
}

func TestNames_Sorted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, k := range []string{"zeta", "alpha", "mu"} {
		if err := s.Set(ctx, k, "v"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Names(ctx)
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	want := []string{"alpha", "mu", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolve_FromFile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Set(ctx, "github_token", "from_file"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	got, err := s.Resolve(ctx, []string{"github_token"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["github_token"] != "from_file" {
		t.Fatalf("github_token = %q, want from_file", got["github_token"])
	}
}

func TestResolve_EnvWinsOverFile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Set(ctx, "github_token", "from_file"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "from_env")
	got, err := s.Resolve(ctx, []string{"github_token"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["github_token"] != "from_env" {
		t.Fatalf("env should win: got %q", got["github_token"])
	}
}

func TestResolve_FromEnvOnly(t *testing.T) {
	s := newTestStore(t)
	t.Setenv("GITHUB_TOKEN", "from_env")
	got, err := s.Resolve(context.Background(), []string{"github_token"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["github_token"] != "from_env" {
		t.Fatalf("github_token = %q", got["github_token"])
	}
}

func TestResolve_MissingNames(t *testing.T) {
	s := newTestStore(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, err := s.Resolve(context.Background(), []string{"github_token", "anthropic_api_key"})
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("Resolve err=%v, want ErrMissing", err)
	}
	// The error message must name every missing credential so the agent
	// run failure surface is actionable without reading source.
	for _, want := range []string{"github_token", "anthropic_api_key"} {
		if !contains(err.Error(), want) {
			t.Errorf("err missing %q: %v", want, err)
		}
	}
}

func TestResolve_Empty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v, want empty", got)
	}
}

func TestEnvName_UpperSnake(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"github_token", "GITHUB_TOKEN"},
		{"AnthropicAPIKey", "ANTHROPICAPIKEY"},
		{"foo-bar", "FOO_BAR"},
		{"abc123", "ABC123"},
	}
	for _, c := range cases {
		got := envName(c.in)
		if got != c.want {
			t.Errorf("envName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
