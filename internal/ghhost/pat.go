package ghhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ResolvePAT returns the GitHub personal access token, preferring the
// GITHUB_TOKEN environment variable and falling back to the github_token
// field in <user-config-dir>/orchestra/config.json.
func ResolvePAT() (string, error) {
	return resolvePAT(defaultConfigPath)
}

func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "orchestra", "config.json"), nil
}

func resolvePAT(getConfigPath func() (string, error)) (string, error) {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t, nil
	}

	path, err := getConfigPath()
	if err != nil {
		return "", fmt.Errorf("%w: GITHUB_TOKEN unset and user config dir unavailable: %w", ErrPATMissing, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: set GITHUB_TOKEN or add `github_token` to %s", ErrPATMissing, path)
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	var cfg struct {
		GitHubToken string `json:"github_token"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.GitHubToken == "" {
		return "", fmt.Errorf("%w: set GITHUB_TOKEN or populate `github_token` in %s", ErrPATMissing, path)
	}
	return cfg.GitHubToken, nil
}
