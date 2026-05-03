// Package credentials resolves agent-required secrets from the orchestra
// credentials store and the host environment, then injects them into the
// child process or session as environment variables.
//
// The store is a flat JSON object at ~/.config/orchestra/credentials.json
// (Linux/macOS) or %APPDATA%\orchestra\credentials.json (Windows): keys are
// credential names ("github_token") and values are the secret strings. Env
// wins over the file when both define the same name — handy for one-off
// dev overrides.
//
// Phase A scope: name → string. Richer secret types (per-agent ACLs,
// rotation, broker integration) are deferred to v4 per the v3 design tradeoff.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// FileName is the basename of the credentials store under the user's
// orchestra config dir.
const FileName = "credentials.json"

// Store reads and writes the credentials JSON file. Methods are safe for
// concurrent use within a single process; cross-process writes are not
// serialized (callers should not race `orchestra credentials set` against
// itself).
type Store struct {
	path string
	// warn is called for non-fatal findings (e.g. permissive file mode on
	// non-Windows). Tests can override; production wires it to stderr.
	warn func(format string, args ...any)
}

// Option customizes a Store at construction time.
type Option func(*Store)

// WithWarn overrides the warning hook. Default writes to stderr.
func WithWarn(fn func(format string, args ...any)) Option {
	return func(s *Store) {
		if fn != nil {
			s.warn = fn
		}
	}
}

// New returns a Store rooted at path. If path is empty, [DefaultPath] is
// used. The file is not required to exist; reads return [ErrNotFound].
func New(path string, opts ...Option) *Store {
	if path == "" {
		path = DefaultPath()
	}
	s := &Store{
		path: path,
		warn: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "credentials: "+format+"\n", args...)
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Path returns the file path the store reads / writes.
func (s *Store) Path() string { return s.path }

// ErrNotFound is wrapped by [Store.Read] when the file does not exist.
// Callers can treat this as "no credentials configured yet."
var ErrNotFound = errors.New("credentials: file not found")

// ErrMissing is returned by [Resolve] when one or more required
// credentials cannot be found in either the store or the env.
var ErrMissing = errors.New("credentials: required credential missing")

// Read loads the credentials map from disk. Returns [ErrNotFound] when the
// file is absent. Validates 0600 mode on POSIX hosts; Windows skips the
// check because chmod has no NTFS equivalent and 0o600 emulation is
// best-effort there.
func (s *Store) Read(_ context.Context) (map[string]string, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("credentials: stat %s: %w", s.path, err)
	}
	if runtime.GOOS != "windows" {
		mode := info.Mode().Perm()
		if mode&0o077 != 0 {
			s.warn("file mode %o on %s is permissive; recommend 0600", mode, s.path)
		}
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("credentials: read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return map[string]string{}, nil
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("credentials: parse %s: %w", s.path, err)
	}
	if raw == nil {
		raw = map[string]string{}
	}
	return raw, nil
}

// Write replaces the credentials file atomically with mode 0600. Empty
// `creds` writes a `{}` document so subsequent reads succeed.
func (s *Store) Write(_ context.Context, creds map[string]string) error {
	if creds == nil {
		creds = map[string]string{}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("credentials: mkdir %s: %w", filepath.Dir(s.path), err)
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("credentials: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("credentials: rename %s → %s: %w", tmp, s.path, err)
	}
	if runtime.GOOS != "windows" {
		// os.WriteFile honors the mode on creation but not on overwrite of
		// an existing file. Force the bits afterwards on POSIX.
		if err := os.Chmod(s.path, 0o600); err != nil {
			return fmt.Errorf("credentials: chmod %s: %w", s.path, err)
		}
	}
	return nil
}

// Set writes a single credential, preserving the rest of the store. Same
// atomic-write semantics as [Store.Write].
func (s *Store) Set(ctx context.Context, name, value string) error {
	if name == "" {
		return errors.New("credentials: name is required")
	}
	creds, err := s.Read(ctx)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if creds == nil {
		creds = map[string]string{}
	}
	creds[name] = value
	return s.Write(ctx, creds)
}

// Delete removes a single credential. Missing names are not an error
// (idempotent delete).
func (s *Store) Delete(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("credentials: name is required")
	}
	creds, err := s.Read(ctx)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, ok := creds[name]; !ok {
		return nil
	}
	delete(creds, name)
	return s.Write(ctx, creds)
}

// Has reports whether `name` is present in the store. Missing-file is
// reported as not-present (no error).
func (s *Store) Has(ctx context.Context, name string) (bool, error) {
	creds, err := s.Read(ctx)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, ok := creds[name]
	return ok, nil
}

// Names returns the sorted list of credential names in the store.
func (s *Store) Names(ctx context.Context) ([]string, error) {
	creds, err := s.Read(ctx)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(creds))
	for name := range creds {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Resolve resolves the requested credential names against the env (first)
// and the store (second). Env wins on conflict so dev overrides do not
// require editing the file. Missing names produce [ErrMissing] with a
// message naming each missing entry.
//
// The returned map is keyed by credential name and contains the secret
// values. Empty `names` returns an empty map and no error.
func (s *Store) Resolve(ctx context.Context, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	stored, err := s.Read(ctx)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	out := make(map[string]string, len(names))
	var missing []string
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if v, ok := os.LookupEnv(envName(name)); ok && v != "" {
			out[name] = v
			continue
		}
		if v, ok := stored[name]; ok && v != "" {
			out[name] = v
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%w: %v (set in %s or env)", ErrMissing, missing, s.path)
	}
	return out, nil
}

// EnvNameFor returns the environment variable name a credential maps to.
// The canonical form is upper-snake-case (e.g. `github_token` →
// `GITHUB_TOKEN`) so existing CI/dev workflows that already export
// `GITHUB_TOKEN` keep working without renaming.
func EnvNameFor(credName string) string { return envName(credName) }

func envName(credName string) string {
	out := make([]byte, 0, len(credName))
	for i := 0; i < len(credName); i++ {
		c := credName[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c-32)
		case c >= 'A' && c <= 'Z':
			out = append(out, c)
		case c >= '0' && c <= '9':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// DefaultPath returns the OS-appropriate path to credentials.json:
//
//   - Windows: %APPDATA%\orchestra\credentials.json (parity with config.json)
//   - Other:   ~/.config/orchestra/credentials.json
//
// Falls back to %TEMP%\orchestra\credentials.json (or /tmp/orchestra) if
// the user config dir cannot be resolved — failures here are extremely
// rare and the temp path keeps tests/CI from panicking on weird hosts.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.TempDir(), "orchestra")
	} else {
		dir = filepath.Join(dir, "orchestra")
	}
	return filepath.Join(dir, FileName)
}
