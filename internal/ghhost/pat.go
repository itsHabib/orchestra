package ghhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/itsHabib/orchestra/internal/credentials"
)

// ResolvePAT returns the GitHub personal access token. Lookup order:
//
//  1. $GITHUB_TOKEN environment variable.
//  2. github_token in <user-config-dir>/orchestra/credentials.json — the
//     canonical home for all orchestra secrets, including this one.
//  3. github_token in <user-config-dir>/orchestra/config.json — the legacy
//     location, kept for back-compat. Emits a one-shot deprecation warning
//     to stderr; this fallback will be removed in a future minor release.
//
// The order is deliberate: env first so dev overrides Just Work, then
// credentials.json (the consolidated store both `requires_credentials:` and
// the github_repository ResourceRef path read from), and finally
// config.json so existing setups keep working through the migration.
//
// ctx is plumbed through to the credentials store read so callers can
// cancel a long-running startup. The store's Read is local I/O and
// returns quickly in practice; ctx mostly matters for tooling (linters,
// tracing) that wants the chain explicit.
func ResolvePAT(ctx context.Context) (string, error) {
	return resolvePAT(ctx, defaultConfigPath, defaultCredentialsStorePath, patWarn)
}

// patWarnOnce ensures the config.json deprecation warning fires at most
// once per process. Tests inject their own warn function so the once
// behavior does not leak across test cases.
var patWarnOnce sync.Once

// patWarn is the production warning emitter — writes once to stderr.
// Tests pass a recording warn instead so they observe the per-call shape.
func patWarn(format string, args ...any) {
	patWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "ghhost: "+format+"\n", args...)
	})
}

func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "orchestra", "config.json"), nil
}

func defaultCredentialsStorePath() string {
	return credentials.DefaultPath()
}

// resolvePAT is the testable seam. getCredsPath returns the path the
// credentials store should read from (empty string skips the credentials
// lookup entirely — used by some tests to keep the layout simple).
// warn is invoked when the config.json fallback wins so the operator knows
// to migrate.
func resolvePAT(
	ctx context.Context,
	getConfigPath func() (string, error),
	getCredsPath func() string,
	warn func(format string, args ...any),
) (string, error) {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t, nil
	}

	credsPath := ""
	if getCredsPath != nil {
		credsPath = getCredsPath()
	}
	if credsPath != "" {
		token, err := readCredentialsToken(ctx, credsPath)
		if err != nil {
			return "", err
		}
		if token != "" {
			return token, nil
		}
	}

	configPath, err := getConfigPath()
	if err != nil {
		return "", fmt.Errorf("%w: GITHUB_TOKEN unset and user config dir unavailable: %w", ErrPATMissing, err)
	}
	res, err := readConfigToken(configPath)
	if err != nil {
		return "", err
	}
	if !res.found {
		return "", fmt.Errorf(
			"%w: set GITHUB_TOKEN, add `github_token` to %s, or add it to %s",
			ErrPATMissing, credsPath, configPath,
		)
	}
	if warn != nil {
		warn(
			"github_token in %s is deprecated; move it to %s — config.json fallback will be removed in a future release",
			configPath, credsPath,
		)
	}
	return res.token, nil
}

// readCredentialsToken returns the github_token value from the credentials
// store at path, "" if the store does not have it (or the file does not
// exist), and an error only on a real I/O / parse failure. Missing file
// is the "no credentials configured yet" path and falls through to
// config.json.
func readCredentialsToken(ctx context.Context, path string) (string, error) {
	store := credentials.New(path)
	creds, err := store.Read(ctx)
	if err != nil {
		if errors.Is(err, credentials.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return creds["github_token"], nil
}

// configTokenResult bundles the legacy config.json read so callers can
// distinguish "file present, token populated" (warn + return) from
// "file/key missing" (fall through to the missing-token error).
type configTokenResult struct {
	token string
	found bool
}

// readConfigToken returns the github_token from the legacy config.json at
// configPath. found is true when the file parsed cleanly and the token
// field is non-empty — i.e. the caller should warn about deprecation.
func readConfigToken(configPath string) (configTokenResult, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return configTokenResult{}, nil
		}
		return configTokenResult{}, fmt.Errorf("read %s: %w", configPath, err)
	}
	var cfg struct {
		GitHubToken string `json:"github_token"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return configTokenResult{}, fmt.Errorf("parse %s: %w", configPath, err)
	}
	if cfg.GitHubToken == "" {
		return configTokenResult{}, nil
	}
	return configTokenResult{token: cfg.GitHubToken, found: true}, nil
}
