// Package skills registers orchestra skill directories with the Anthropic
// Managed Agents Skills API and caches the resulting skill IDs for reuse
// across sessions. A skill is a directory containing at least a SKILL.md at
// its root and any helper files (scripts, templates, sub-prompts) the agent
// might need; the cache lets the engine attach the same registered skill to
// many sessions without re-uploading on every run, while still detecting
// drift in the source directory.
package skills

import (
	"context"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Entry is one skill's row in the cache. SourcePath is recorded so a later
// `skills sync` knows where to re-read from when drift is detected. The path
// points at the skill's directory (containing SKILL.md), not at SKILL.md
// itself — the registration API uploads every file in the directory.
type Entry struct {
	SkillID       string    `json:"skill_id"`
	LatestVersion string    `json:"latest_version"`
	ContentHash   string    `json:"content_hash"`
	SourcePath    string    `json:"source_path"`
	RegisteredAt  time.Time `json:"registered_at"`
}

// Lookup is the result of resolving a skill name against the cache plus a fresh
// hash of the source directory.
type Lookup struct {
	Name          string
	Entry         Entry
	Found         bool
	SourceMissing bool
	CurrentHash   string
	Drifted       bool
}

// SyncResult describes one skill's outcome from a Sync pass.
type SyncResult struct {
	Name            string
	Entry           Entry
	Action          SyncAction
	PreviousVersion string
	SourceMissing   bool
	Err             error
}

// SyncAction names what Sync did with one cached skill.
type SyncAction string

const (
	// SyncUpToDate means the cached entry matched the source.
	SyncUpToDate SyncAction = "up-to-date"
	// SyncReuploaded means drift was detected and a new skill version was
	// registered, or the skill was registered for the first time.
	SyncReuploaded SyncAction = "reuploaded"
	// SyncSkipped means the source directory was missing or otherwise unreadable.
	SyncSkipped SyncAction = "skipped"
)

// RegistrationClient is the subset of the Anthropic Beta Skills API the
// skills service uses. NewSkill registers a skill for the first time;
// NewSkillVersion publishes a new version under an existing skill_id when
// the source directory drifts.
type RegistrationClient interface {
	NewSkill(ctx context.Context, params anthropic.BetaSkillNewParams, opts ...option.RequestOption) (*anthropic.BetaSkillNewResponse, error)
	NewSkillVersion(ctx context.Context, skillID string, params anthropic.BetaSkillVersionNewParams, opts ...option.RequestOption) (*anthropic.BetaSkillVersionNewResponse, error)
}
