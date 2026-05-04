package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Load reads and parses an orchestra config from the given YAML file path.
// It resolves defaults and validates the config before returning. The
// returned [Result] aggregates the parsed config (when valid), the
// warnings, and the errors. The error return is reserved for I/O or
// parse failures (file not found, malformed YAML); structural validation
// issues live in result.Errors.
//
// Relative `files.path` entries on each agent are canonicalized against
// the directory containing the loaded YAML file. The rest of the system
// (validation, the run engine, the file uploader) only ever sees absolute
// paths so the run is reproducible regardless of the CWD `orchestra run`
// is invoked from.
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
	resolveRelativeFilePaths(&cfg, path)
	return cfg.Validate(), nil
}

// resolveRelativeFilePaths rewrites every agent's relative [FileMount.Path]
// entry to an absolute path rooted at the config file's directory. Absolute
// entries are left untouched. Empty paths are left as-is — validation will
// reject them with a clear error.
func resolveRelativeFilePaths(cfg *Config, configPath string) {
	if cfg == nil {
		return
	}
	dir, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		// Fall back to the literal dir; downstream upload may still
		// succeed if the CWD happens to match. Validation will surface
		// the underlying file-not-found if it doesn't.
		dir = filepath.Dir(configPath)
	}
	for i := range cfg.Agents {
		for j := range cfg.Agents[i].Files {
			p := cfg.Agents[i].Files[j].Path
			if p == "" {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			// filepath.Clean normalizes separators (forward slashes
			// in YAML → backslashes on Windows) so downstream
			// comparisons and atomic-write paths use the host's
			// native separator regardless of how the YAML was
			// written.
			cfg.Agents[i].Files[j].Path = filepath.Clean(p)
		}
	}
}
