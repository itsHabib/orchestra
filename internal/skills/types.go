// Package skills uploads orchestra skill documents to the Anthropic Files API
// and caches the resulting file IDs for reuse across sessions. A skill is a
// markdown file (typically SKILL.md) describing a role; the cache lets the
// engine attach the same file to many sessions without re-uploading on every
// run, while still detecting drift in the source file.
package skills

import (
	"context"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Entry is one skill's row in the cache. SourcePath is recorded so a later
// `skills sync` knows where to re-read from when drift is detected.
type Entry struct {
	FileID      string    `json:"file_id"`
	ContentHash string    `json:"content_hash"`
	SourcePath  string    `json:"source_path"`
	Filename    string    `json:"filename,omitempty"`
	UploadedAt  time.Time `json:"uploaded_at"`
}

// Lookup is the result of resolving a skill name against the cache plus a fresh
// hash of the source file.
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
	Name          string
	Entry         Entry
	Action        SyncAction
	PreviousFile  string
	SourceMissing bool
	Err           error
}

// SyncAction names what Sync did with one cached skill.
type SyncAction string

const (
	// SyncUpToDate means the cached entry matched the source.
	SyncUpToDate SyncAction = "up-to-date"
	// SyncReuploaded means drift was detected and the file was re-uploaded.
	SyncReuploaded SyncAction = "reuploaded"
	// SyncSkipped means the source file was missing or otherwise unreadable.
	SyncSkipped SyncAction = "skipped"
)

// UploadClient is the subset of the Anthropic Files API the skills service uses.
type UploadClient interface {
	Upload(context.Context, anthropic.BetaFileUploadParams, ...option.RequestOption) (*anthropic.FileMetadata, error)
	Delete(context.Context, string, anthropic.BetaFileDeleteParams, ...option.RequestOption) (*anthropic.DeletedFile, error)
}
