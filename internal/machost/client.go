// Package machost holds host-side helpers for talking to Managed Agents.
package machost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ResolveAPIKey returns the Anthropic API key, preferring the ANTHROPIC_API_KEY
// environment variable, falling back to the `api_key` field in
// <user-config-dir>/orchestra/config.json.
func ResolveAPIKey() (string, error) {
	return resolveAPIKey(defaultConfigPath)
}

// NewClient constructs an Anthropic SDK client using ResolveAPIKey. The
// managed-agents-2026-04-01 beta header is set automatically by the SDK.
func NewClient() (anthropic.Client, error) {
	key, err := ResolveAPIKey()
	if err != nil {
		return anthropic.Client{}, err
	}
	return anthropic.NewClient(option.WithAPIKey(key)), nil
}

func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "orchestra", "config.json"), nil
}

func resolveAPIKey(getConfigPath func() (string, error)) (string, error) {
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return k, nil
	}

	path, err := getConfigPath()
	if err != nil {
		return "", fmt.Errorf("no ANTHROPIC_API_KEY env var set and user config dir unavailable: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("no Anthropic API key: set ANTHROPIC_API_KEY or add `api_key` to %s", path)
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	var cfg struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.APIKey == "" {
		return "", fmt.Errorf("no Anthropic API key: set ANTHROPIC_API_KEY or populate `api_key` in %s", path)
	}
	return cfg.APIKey, nil
}
