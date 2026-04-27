package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultSourceDir is where SKILL.md files live by Claude Code convention.
const DefaultSourceDir = ".claude/skills"

// ResolveSource returns the path to a skill's SKILL.md.
//
// If override is non-empty, it is returned as-is (after resolving to absolute
// for diagnostics). Otherwise the user-global Claude Code default is used:
// `$HOME/.claude/skills/<name>/SKILL.md`.
//
// The returned path is not opened — callers handle missing-file errors when
// they read it.
func ResolveSource(name, override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("skills: resolve --from %q: %w", override, err)
		}
		return abs, nil
	}
	if name == "" {
		return "", errors.New("skills: resolve source: empty name")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("skills: resolve source: %w", err)
	}
	return filepath.Join(home, DefaultSourceDir, name, "SKILL.md"), nil
}
