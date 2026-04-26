package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads and parses an orchestra config from the given YAML file path.
// It resolves defaults and validates the config before returning. The
// returned [Result] aggregates the parsed config (when valid), the
// warnings, and the errors. The error return is reserved for I/O or
// parse failures (file not found, malformed YAML); structural validation
// issues live in result.Errors.
//
// pkg/orchestra re-exports Load as orchestra.LoadConfig.
func Load(path string) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.ResolveDefaults()
	return cfg.Validate(), nil
}
