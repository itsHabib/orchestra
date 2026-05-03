package customtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/itsHabib/orchestra/internal/artifacts"
	"github.com/itsHabib/orchestra/internal/notify"
	"github.com/itsHabib/orchestra/internal/store"
)

// SignalCompletionTool is the host-side name of the sentinel tool. Exposed as
// a constant so config validation, engine wiring, and tests refer to the same
// string.
const SignalCompletionTool = "signal_completion"

// signalDone is the value of input.status that means the team finished
// successfully (PR open, reviews requested, CI green, comments addressed).
const signalDone = "done"

// signalBlocked is the value of input.status that means the team hit a hard
// block (genuine ambiguity, unresolvable review conflict, CI failure outside
// scope) and is waiting for human steering via orchestra msg.
const signalBlocked = "blocked"

// artifactSoftCapPerKey caps a single artifact's content size. The cap is
// reported back to the agent on violation so it can shrink and retry.
const artifactSoftCapPerKey = 256 * 1024

// artifactHardCapTotal caps the aggregate size of all artifacts attached to
// one signal_completion call. Per-key caps run first so the error message
// names the offending artifact when only one is oversized.
const artifactHardCapTotal = 4 * 1024 * 1024

// SignalCompletionHandler implements the sentinel tool from §7 of
// DESIGN-ship-feature-workflow. It is stateless — every Handle call gets a
// fresh RunContext with the live store + notifier — so a single instance can
// be registered once at engine startup and shared across all teams.
type SignalCompletionHandler struct{}

// NewSignalCompletion returns a ready-to-register SignalCompletionHandler.
func NewSignalCompletion() SignalCompletionHandler {
	return SignalCompletionHandler{}
}

// Tool returns the tool definition. The schema mirrors §7.1 of the design
// doc: required `status` enum + `summary`, optional `pr_url` (used when
// status=done) and `reason` (required when status=blocked, enforced by
// Handle rather than the schema since JSON Schema can't express "required
// when sibling=X" without anyOf gymnastics).
func (SignalCompletionHandler) Tool() Definition {
	return Definition{
		Name:        SignalCompletionTool,
		Description: "Called once per session when the team's work is fully done OR genuinely blocked. After calling this, stop emitting actions.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{
					"type":        "string",
					"enum":        []string{signalDone, signalBlocked},
					"description": "done when the PR is merge-ready; blocked when human input is required to proceed.",
				},
				"summary": map[string]any{
					"type":        "string",
					"description": "One-line summary of what was shipped (status=done) or why it's blocked (status=blocked).",
				},
				"pr_url": map[string]any{
					"type":        "string",
					"description": "Required when status=done. The merge-ready PR URL.",
				},
				"reason": map[string]any{
					"type":        "string",
					"description": "Required when status=blocked. A sentence the human can act on.",
				},
				"artifacts": map[string]any{
					"type":        "object",
					"description": "Optional structured outputs persisted host-side. Each entry becomes an artifact retrievable via mcp__orchestra__get_artifacts / read_artifact. Caps: 256KB per artifact, 4MB total.",
					"additionalProperties": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"type": map[string]any{
								"type":        "string",
								"enum":        []string{string(artifacts.TypeText), string(artifacts.TypeJSON)},
								"description": "Payload type. text → content is a string; json → content is any JSON value.",
							},
							"content": map[string]any{
								"description": "The artifact payload. A JSON string for type=text; any JSON value for type=json.",
							},
						},
						"required": []string{"type", "content"},
					},
				},
			},
			"required": []string{"status", "summary"},
		},
	}
}

// signalCompletionInput is the host-side decoded shape of a tool call.
type signalCompletionInput struct {
	Status    string                   `json:"status"`
	Summary   string                   `json:"summary"`
	PRURL     string                   `json:"pr_url,omitempty"`
	Reason    string                   `json:"reason,omitempty"`
	Artifacts map[string]artifactInput `json:"artifacts,omitempty"`
}

// artifactInput is one entry under the input's artifacts map. Content is
// decoded lazily — the parser validates type + size + JSON-shape before
// promoting to an [artifacts.Artifact] for persistence.
type artifactInput struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

// signalCompletionResult is the JSON we hand back to the agent. duplicate is
// true when the team had already signaled — the agent is expected to stop
// either way, so the field is informational.
type signalCompletionResult struct {
	OK        bool   `json:"ok"`
	Duplicate bool   `json:"duplicate"`
	Status    string `json:"status,omitempty"`
}

// Handle records the signal in the run state and fires a notification.
// Idempotent on a duplicate call: if the team already has a SignalStatus
// set, the second call is a no-op and returns ok=true,duplicate=true (per
// §14 Q10) — a confused agent calling twice cannot erase the original
// outcome or re-fire the notification.
//
// Exception: the blocked → done transition is allowed and overwrites the
// recorded state. This is the legitimate recovery flow from §7.2 (the
// team blocks, gets unblocked via steering, then signals done) and
// without it the run-status derivation in §11.2 never sees the team
// reach done. All other repeated combinations (done→blocked, done→done,
// blocked→blocked) stay idempotent — those are confused-agent shapes.
//
// Persistence ordering when artifacts are attached: artifact FILES are
// written first, then a single UpdateAgentState commits Signal* fields and
// appends the artifact keys atomically. Reasoning (codex/copilot/claude
// review on PR #35): if the state write committed before the file writes
// and the file write then failed, the agent would see is_error and a
// retry would be a duplicate — its artifacts permanently lost. Files-first
// flips that — a state-write failure leaves orphan files but the retry
// works because Put treats ErrAlreadyExists from a prior attempt as
// idempotent (first-write content wins on the same key). The TOCTOU
// snapshot pre-check guards against orphans when an agent (incorrectly)
// re-emits with a different artifact set after a successful first signal.
func (SignalCompletionHandler) Handle(ctx context.Context, run *RunContext, team string, raw json.RawMessage) (json.RawMessage, error) {
	if run == nil {
		return nil, errors.New("signal_completion: nil run context")
	}
	if team == "" {
		return nil, errors.New("signal_completion: empty team")
	}
	in, err := parseSignalCompletionInput(raw)
	if err != nil {
		return nil, err
	}

	if run.Store == nil {
		return nil, errors.New("signal_completion: nil store")
	}
	now := run.Time()

	// Pre-check duplicate via a snapshot so a confused agent re-signaling
	// with a *different* artifact set does not leak orphan files. The
	// authoritative duplicate check still happens inside writeSignalState's
	// UpdateAgentState closure; this is a best-effort guard. The TOCTOU
	// window is bounded by the engine's per-team serial dispatch — only a
	// rogue concurrent caller could exploit it.
	likelyDuplicate := snapshotIsDuplicate(ctx, run.Store, team, in.Status)

	// Persist artifact files first. Put is idempotent on retry — if a prior
	// attempt at the same signal landed the file but the subsequent state
	// write failed, the retry sees ErrAlreadyExists and treats it as success
	// (the on-disk content from the first attempt is preserved; second-call
	// content for the same key is silently ignored). Documented as a
	// best-effort retry guarantee.
	var writtenKeys []string
	if !likelyDuplicate && len(in.Artifacts) > 0 && run.Artifacts != nil {
		keys, perr := persistArtifactFiles(ctx, run, team, in.Artifacts)
		if perr != nil {
			return nil, perr
		}
		writtenKeys = keys
	}

	write, err := writeSignalState(ctx, run.Store, team, &in, writtenKeys, now)
	if err != nil {
		return nil, err
	}

	if !write.duplicate && run.Notifier != nil {
		if notifyErr := run.Notifier.Notify(ctx, &notify.Notification{
			Timestamp: now,
			RunID:     run.RunID,
			Team:      team,
			Status:    in.Status,
			Summary:   in.Summary,
			PRURL:     in.PRURL,
			Reason:    in.Reason,
		}); notifyErr != nil {
			// fanOut.Notify is documented to swallow per-sink failures — a
			// non-nil error here means the composite itself refused (rare:
			// e.g. a custom Notifier not built via Compose). Surface as an
			// is_error tool result so the agent learns the host could not
			// deliver the notification, but keep the state write — the
			// signal is recorded regardless.
			return nil, fmt.Errorf("signal_completion: notify: %w", notifyErr)
		}
	}

	// Echo the *recorded* status, not the input's. On a duplicate call the
	// first signal has already won; reflecting the input's status would
	// mislead a confused agent into thinking its second call took effect.
	out, err := json.Marshal(&signalCompletionResult{
		OK:        true,
		Duplicate: write.duplicate,
		Status:    write.recordedStatus,
	})
	if err != nil {
		return nil, fmt.Errorf("signal_completion: marshal result: %w", err)
	}
	return out, nil
}

func parseSignalCompletionInput(raw json.RawMessage) (signalCompletionInput, error) {
	var in signalCompletionInput
	if len(raw) == 0 {
		return in, errors.New("signal_completion: empty input")
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, fmt.Errorf("signal_completion: parse input: %w", err)
	}
	switch in.Status {
	case signalDone, signalBlocked:
	default:
		return in, fmt.Errorf("signal_completion: status must be %q or %q, got %q",
			signalDone, signalBlocked, in.Status)
	}
	if in.Summary == "" {
		return in, errors.New("signal_completion: summary is required")
	}
	if in.Status == signalDone && in.PRURL == "" {
		return in, errors.New("signal_completion: pr_url is required when status=done")
	}
	if in.Status == signalBlocked && in.Reason == "" {
		return in, errors.New("signal_completion: reason is required when status=blocked")
	}
	if err := validateArtifacts(in.Artifacts); err != nil {
		return in, err
	}
	return in, nil
}

// validateArtifacts enforces type, size, and shape rules on the optional
// artifacts map. Per-key checks fire first so the agent gets an actionable
// error naming the offending key when only one artifact is wrong; the
// aggregate cap fires only after every artifact passes its individual check.
func validateArtifacts(arts map[string]artifactInput) error {
	if len(arts) == 0 {
		return nil
	}
	keys := sortedArtifactKeys(arts)
	var total int
	for _, k := range keys {
		art := arts[k]
		if err := validateArtifactKey(k); err != nil {
			return err
		}
		switch art.Type {
		case string(artifacts.TypeText), string(artifacts.TypeJSON):
		default:
			return fmt.Errorf("signal_completion: artifact %q: type must be %q or %q, got %q",
				k, artifacts.TypeText, artifacts.TypeJSON, art.Type)
		}
		if len(art.Content) == 0 {
			return fmt.Errorf("signal_completion: artifact %q: content is empty", k)
		}
		if !json.Valid(art.Content) {
			return fmt.Errorf("signal_completion: artifact %q: content is not valid JSON", k)
		}
		if size := len(art.Content); size > artifactSoftCapPerKey {
			return fmt.Errorf("signal_completion: artifact %q: content size %d > cap %d",
				k, size, artifactSoftCapPerKey)
		}
		if art.Type == string(artifacts.TypeText) {
			var s string
			if err := json.Unmarshal(art.Content, &s); err != nil {
				return fmt.Errorf("signal_completion: artifact %q: type=text requires a JSON string content", k)
			}
		}
		total += len(art.Content)
	}
	if total > artifactHardCapTotal {
		return fmt.Errorf("signal_completion: artifacts total size %d > cap %d", total, artifactHardCapTotal)
	}
	return nil
}

// validateArtifactKey rejects empty keys and obvious path-traversal shapes.
// The artifacts package enforces the same rules at write time as defense in
// depth; surfacing them here gives the agent a clearer error path.
func validateArtifactKey(key string) error {
	if key == "" {
		return errors.New("signal_completion: artifact key is empty")
	}
	if key == "." || key == ".." {
		return fmt.Errorf("signal_completion: artifact key %q is reserved", key)
	}
	for _, r := range key {
		switch r {
		case '/', '\\', 0:
			return fmt.Errorf("signal_completion: artifact key %q contains invalid characters", key)
		}
	}
	if key[0] == '.' {
		return fmt.Errorf("signal_completion: artifact key %q must not start with a dot", key)
	}
	return nil
}

func sortedArtifactKeys(arts map[string]artifactInput) []string {
	keys := make([]string, 0, len(arts))
	for k := range arts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// signalWrite is the result of applying a signal_completion input to the
// team state.
type signalWrite struct {
	// recordedStatus is what the team state ended up holding — the original
	// status on a duplicate, the new status otherwise. Echoed back to the
	// agent so a confused second call can't claim its status overrode the
	// first.
	recordedStatus string
	// duplicate is true when the team had already signaled before this call.
	// Callers use it to skip notification.
	duplicate bool
}

// persistArtifactFiles writes each artifact's content to the store and
// returns the sorted list of keys that landed on disk. Idempotent on retry:
// when Put returns ErrAlreadyExists (a file from a prior attempt at this
// same signal_completion exists), the key is treated as written and the
// existing on-disk content is preserved. This lets a state-write failure
// after the file write retry cleanly without losing the signal — at the
// cost of dropping a second-call content update for the same key (first-
// write wins). The trade-off is documented on Handle.
//
// File writes happen BEFORE the state commit; the state.json artifact key
// list is appended atomically by writeSignalState as part of the same
// closure that commits the Signal* fields. See Handle's doc comment for
// the rationale.
func persistArtifactFiles(ctx context.Context, run *RunContext, team string, arts map[string]artifactInput) ([]string, error) {
	keys := sortedArtifactKeys(arts)
	written := make([]string, 0, len(keys))
	for _, k := range keys {
		art := artifacts.Artifact{
			Type:    artifacts.Type(arts[k].Type),
			Content: append(json.RawMessage(nil), arts[k].Content...),
		}
		if _, err := run.Artifacts.Put(ctx, run.RunID, team, k, run.Phase, art); err != nil {
			if !errors.Is(err, artifacts.ErrAlreadyExists) {
				return nil, fmt.Errorf("signal_completion: persist artifact %q: %w", k, err)
			}
			// Idempotent retry: a prior attempt at this same signal already
			// wrote this key. Keep the prior content (first-write wins).
		}
		written = append(written, k)
	}
	return written, nil
}

// snapshotIsDuplicate returns true when state.json indicates a prior
// signal_completion call has already won. Best-effort guard against orphan
// artifact files when a confused agent re-emits with a different payload —
// the authoritative duplicate check still runs inside writeSignalState's
// UpdateAgentState closure under the per-team mutex. Returns false on any
// load error so the engine errs toward writing rather than silently dropping
// artifacts.
func snapshotIsDuplicate(ctx context.Context, st store.Store, team, newStatus string) bool {
	state, err := st.LoadRunState(ctx)
	if err != nil || state == nil {
		return false
	}
	ts, ok := state.Agents[team]
	if !ok || ts.SignalStatus == "" {
		return false
	}
	// Recovery transition is NOT a duplicate.
	if ts.SignalStatus == signalBlocked && newStatus == signalDone {
		return false
	}
	return true
}

// writeSignalState applies the input to the team state under the existing
// UpdateAgentState funnel and atomically appends artifactKeys to the agent's
// Artifacts list when the write is fresh. Combining the two state updates
// in one closure is the only way to keep the Signal* commit and the
// artifact-key registration consistent: a separate state write for the keys
// would re-introduce the failure-mid-sequence gap the codex/copilot/claude
// review on PR #35 flagged.
//
// Transition rules (§7.2 + §14 Q10 reconciliation):
//   - SignalStatus == "" → any input wins. First call after a fresh team.
//   - SignalStatus == "blocked" + input "done" → ALLOWED. Legitimate
//     recovery: team blocked, was unblocked via steering, now signals
//     completion. Overwrites all Signal* fields with the new outcome and
//     appends recovery-call artifact keys.
//   - All other repeated combinations (done→done, done→blocked,
//     blocked→blocked) → idempotent no-op, returns duplicate=true. Artifact
//     keys passed in are NOT appended (the caller already short-circuited
//     file writes via the snapshot pre-check).
func writeSignalState(ctx context.Context, st store.Store, team string, in *signalCompletionInput, artifactKeys []string, now time.Time) (signalWrite, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var out signalWrite
	err := st.UpdateAgentState(ctx, team, func(ts *store.AgentState) {
		recoverFromBlocked := ts.SignalStatus == signalBlocked && in.Status == signalDone
		if ts.SignalStatus != "" && !recoverFromBlocked {
			out.duplicate = true
			out.recordedStatus = ts.SignalStatus
			return
		}
		ts.SignalStatus = in.Status
		ts.SignalSummary = in.Summary
		ts.SignalPRURL = in.PRURL
		ts.SignalReason = in.Reason
		ts.SignalAt = now
		appendArtifactKeysLocked(ts, artifactKeys)
		out.recordedStatus = in.Status
	})
	if err != nil {
		return signalWrite{}, fmt.Errorf("signal_completion: update state: %w", err)
	}
	return out, nil
}

// appendArtifactKeysLocked merges sorted artifact keys into ts.Artifacts,
// deduping defensively. Caller must hold the per-team state mutex (i.e.
// run inside an UpdateAgentState closure). State.json output stays stable
// because the merged list is re-sorted lexicographically.
func appendArtifactKeysLocked(ts *store.AgentState, keys []string) {
	if len(keys) == 0 {
		return
	}
	existing := make(map[string]struct{}, len(ts.Artifacts))
	for _, k := range ts.Artifacts {
		existing[k] = struct{}{}
	}
	for _, k := range keys {
		if _, ok := existing[k]; ok {
			continue
		}
		ts.Artifacts = append(ts.Artifacts, k)
		existing[k] = struct{}{}
	}
	sort.Strings(ts.Artifacts)
}
