// Package artifacts persists structured outputs produced by an agent's
// signal_completion(artifacts={...}) tool call. The host (orchestra) extracts
// the artifacts payload from the session event stream and writes one envelope
// JSON file per artifact to <root>/<agent>/<key>.json. The chat-side LLM
// reads them back via mcp__orchestra__get_artifacts / read_artifact.
//
// Storage layout is deliberately flat — one directory per agent, one file per
// artifact, no compression, no encryption. The envelope carries the metadata
// (type, phase, size, written) alongside the raw content so list-style reads
// don't have to fan out across sidecar files.
package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Type is the declared artifact payload type. The agent picks one in
// signal_completion(artifacts={key: {type: ..., content: ...}}) and the host
// validates against it before persisting.
type Type string

const (
	// TypeText carries plain text. Content on disk is a JSON-encoded string.
	TypeText Type = "text"
	// TypeJSON carries arbitrary JSON. Content on disk is the raw value.
	TypeJSON Type = "json"
)

// Artifact is a single payload as accepted from the agent or returned by
// [Store.Get]. Content is the raw JSON bytes — for [TypeText] this is a
// JSON-encoded string, for [TypeJSON] it is whatever JSON value the agent
// emitted. The host treats Content opaquely except for type-shape validation
// at write time.
type Artifact struct {
	Type    Type
	Content json.RawMessage
}

// Meta is the descriptive metadata for one stored artifact. Returned from
// [Store.Put] (so the caller can echo Size/Written without reloading) and
// from [Store.List] (so clients can filter without reading content).
type Meta struct {
	RunID   string    `json:"run_id"`
	Agent   string    `json:"agent"`
	Phase   string    `json:"phase,omitempty"`
	Key     string    `json:"key"`
	Type    Type      `json:"type"`
	Size    int64     `json:"size"`
	Written time.Time `json:"written"`
}

// Store is the host-side artifact persistence interface. The signal_completion
// handler writes via [Store.Put]; the MCP read tools read via [Store.Get] and
// [Store.List]. A nil Store wired into [customtools.RunContext] disables
// persistence — handlers must tolerate that for the local-backend and unit-
// test paths.
type Store interface {
	// Put writes one artifact. Returns ErrAlreadyExists when a key collision
	// is detected. The returned Meta carries the values actually persisted —
	// callers should prefer it over reconstructing from inputs.
	Put(ctx context.Context, runID, agent, key, phase string, art Artifact) (Meta, error)

	// Get returns the content + metadata for one artifact. Returns
	// ErrNotFound when the key is unknown. The Artifact.Content is a copy of
	// the on-disk envelope's content field, suitable for direct return to an
	// MCP read_artifact caller.
	Get(ctx context.Context, runID, agent, key string) (Artifact, Meta, error)

	// List returns metadata for every artifact under one agent's namespace,
	// or every artifact in the run when agent is empty. Order is sorted by
	// (agent, key) so callers can rely on it for stable diffs. A missing
	// agent dir or a missing root returns (nil, nil) — that's the normal
	// "no artifacts yet" state for a freshly-spawned run.
	List(ctx context.Context, runID, agent string) ([]Meta, error)
}

// ErrNotFound is returned when [Store.Get] cannot find a requested artifact.
var ErrNotFound = errors.New("artifacts: not found")

// ErrAlreadyExists is returned when [Store.Put] detects an existing artifact
// at the target (agent, key) coordinate. The signal_completion handler treats
// this as a programming error — the agent's input map cannot have duplicate
// keys after JSON parsing, and the first-signal-wins idempotency rule blocks
// a second signal from re-emitting them.
var ErrAlreadyExists = errors.New("artifacts: already exists")

// envelope is the on-disk JSON shape of one artifact. Host-only — agents
// never see this; they emit and consume the [Artifact] shape via signal
// events and the read MCP tools.
type envelope struct {
	Type    Type            `json:"type"`
	Phase   string          `json:"phase,omitempty"`
	Size    int64           `json:"size"`
	Written time.Time       `json:"written"`
	Content json.RawMessage `json:"content"`
}
