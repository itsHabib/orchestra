package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads and parses an orchestra config from the given YAML file path.
// It resolves defaults and validates the config before returning.
func Load(path string) (*Config, []Warning, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.ResolveDefaults()

	warnings, err := cfg.Validate()
	if err != nil {
		return nil, warnings, err
	}

	return &cfg, warnings, nil
}
