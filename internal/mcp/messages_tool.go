package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/messaging"
	"github.com/itsHabib/orchestra/internal/store"
)

// Tool names for the message-bus surface.
const (
	ToolSendMessage  = "send_message"
	ToolReadMessages = "read_messages"
)

// Recipient sentinels accepted by send_message in addition to literal team
// names. The bus on disk uses indexed folder names ("0-human" etc.); these
// keywords are translated to those folders by resolveRecipient.
const (
	recipientHuman       = "human"
	recipientCoordinator = "coordinator"
	recipientBroadcast   = "broadcast"
	recipientAll         = "all"

	folderHuman       = "0-human"
	folderCoordinator = "1-coordinator"

	// backendManagedAgents marks the run as MA-backed in state.json. The
	// MCP message-bus tools are local-only — MA runs steer through session
	// events (see internal/run/service.go:343, which skips the file bus
	// init for MA backends).
	backendManagedAgents = "managed_agents"

	// idRandSuffixBytes is the entropy added to message ids to prevent
	// same-millisecond collisions. 4 bytes (8 hex chars) is overkill for
	// the expected concurrency but eliminates the silent-overwrite class
	// of bug entirely.
	idRandSuffixBytes = 4
)

// Default sender for send_message. The MCP server acts on behalf of the chat-
// side LLM, which is the human's surface in this loop.
const defaultSender = folderHuman

// SendMessageArgs is the send_message tool input.
type SendMessageArgs struct {
	RunID     string `json:"run_id" jsonschema:"run id from list_runs / get_run"`
	Recipient string `json:"recipient" jsonschema:"team name, \"coordinator\", \"human\", or \"broadcast\""`
	Content   string `json:"content" jsonschema:"message body the recipient agent will read"`
	Sender    string `json:"sender,omitempty" jsonschema:"override the sender folder (defaults to 0-human)"`
	Type      string `json:"type,omitempty" jsonschema:"optional structured message type — defaults to \"correction\""`
	Subject   string `json:"subject,omitempty" jsonschema:"optional short subject; auto-derived from the first 60 chars of content when empty"`
	ReplyTo   string `json:"reply_to,omitempty" jsonschema:"id of the message this one replies to"`
}

// SendMessageResult is the send_message tool output.
type SendMessageResult struct {
	MessageID string    `json:"message_id"`
	Recipient string    `json:"recipient"`
	WrittenAt time.Time `json:"written_at"`
}

// ReadMessagesArgs is the read_messages tool input.
type ReadMessagesArgs struct {
	RunID     string `json:"run_id" jsonschema:"run id from list_runs / get_run"`
	Recipient string `json:"recipient,omitempty" jsonschema:"narrow to one inbox (team name, \"coordinator\", or \"human\"); aggregates across all inboxes when empty. Reject \"broadcast\"/\"all\" — broadcast targets every inbox, so leave recipient empty to see them in the aggregate timeline."`
	Since     string `json:"since,omitempty" jsonschema:"RFC3339 timestamp; only messages written strictly after this are returned"`
}

// ReadMessagesResult is the read_messages tool output. Messages are sorted
// newest-first to match the orchestra://runs/{id}/messages resource.
type ReadMessagesResult struct {
	Messages []MessageView `json:"messages"`
}

// MessageView is the MCP-exposed shape for one message. Mirrors the on-disk
// messaging.Message but uses RFC3339 timestamps and stable JSON tag casing
// for the chat-side LLM. Renaming a field here is a breaking change to the
// MCP surface.
type MessageView struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Sender    string    `json:"sender"`
	Recipient string    `json:"recipient"`
	Type      string    `json:"type,omitempty"`
	Subject   string    `json:"subject,omitempty"`
	Content   string    `json:"content"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Read      bool      `json:"read"`
	WrittenAt time.Time `json:"written_at"`
}

// SDK-constrained signature: ToolHandlerFor[In, Out] passes In by value, so
// SendMessageArgs cannot be a pointer here without giving up the typed
// handler. The struct is 112 bytes (7 strings); the gocritic threshold is 88.
//
//nolint:gocritic // hugeParam — SDK ToolHandlerFor[In, Out] passes In by value.
func (s *Server) handleSendMessage(ctx context.Context, _ *mcp.CallToolRequest, args SendMessageArgs) (*mcp.CallToolResult, SendMessageResult, error) {
	if args.RunID == "" {
		return errResult("run_id is required"), SendMessageResult{}, nil
	}
	if args.Recipient == "" {
		return errResult("recipient is required"), SendMessageResult{}, nil
	}
	if args.Content == "" {
		return errResult("content is required"), SendMessageResult{}, nil
	}

	sender := args.Sender
	if sender == "" {
		sender = defaultSender
	}
	msgType := args.Type
	if msgType == "" {
		msgType = string(messaging.MsgCorrection)
	}
	// sender / type flow into the on-disk filename via msg.ID; reject
	// path-unsafe values up front to prevent a malicious caller from
	// escaping the inbox via "../" or absolute path components.
	if err := validateIDComponent("sender", sender); err != nil {
		return errResult("%v", err), SendMessageResult{}, nil
	}
	if err := validateIDComponent("type", msgType); err != nil {
		return errResult("%v", err), SendMessageResult{}, nil
	}

	run, err := s.busForRun(ctx, args.RunID)
	if err != nil {
		return errResult("%v", err), SendMessageResult{}, nil
	}
	if err := s.requireFileBus(ctx, run.entry.WorkspaceDir); err != nil {
		return errResult("%v", err), SendMessageResult{}, nil
	}
	folder, err := resolveRecipient(run.msgsDir, args.Recipient)
	if err != nil {
		return errResult("%v", err), SendMessageResult{}, nil
	}

	subject := args.Subject
	if subject == "" {
		subject = deriveSubject(args.Content)
	}
	id, err := newMessageID(sender, msgType)
	if err != nil {
		return errResult("generate message id: %v", err), SendMessageResult{}, nil
	}
	now := time.Now().UTC()
	msg := &messaging.Message{
		ID:        id,
		Sender:    sender,
		Recipient: folder,
		Type:      messaging.MessageType(msgType),
		Subject:   subject,
		Content:   args.Content,
		ReplyTo:   args.ReplyTo,
		Timestamp: now,
	}
	if err := run.bus.Send(msg); err != nil {
		return errResult("send: %v", err), SendMessageResult{}, nil
	}

	out := SendMessageResult{
		MessageID: msg.ID,
		Recipient: folder,
		WrittenAt: msg.Timestamp,
	}
	return textResult(fmt.Sprintf("sent %s to %s", out.MessageID, out.Recipient)), out, nil
}

func (s *Server) handleReadMessages(ctx context.Context, _ *mcp.CallToolRequest, args ReadMessagesArgs) (*mcp.CallToolResult, ReadMessagesResult, error) {
	if args.RunID == "" {
		return errResult("run_id is required"), ReadMessagesResult{}, nil
	}
	if isBroadcastKeyword(args.Recipient) {
		return errResult(
			"recipient %q has no single inbox; leave recipient empty to aggregate every inbox (broadcast messages appear in the aggregate)",
			args.Recipient,
		), ReadMessagesResult{}, nil
	}
	run, err := s.busForRun(ctx, args.RunID)
	if err != nil {
		return errResult("%v", err), ReadMessagesResult{}, nil
	}
	if err := s.requireFileBus(ctx, run.entry.WorkspaceDir); err != nil {
		return errResult("%v", err), ReadMessagesResult{}, nil
	}
	var since time.Time
	if args.Since != "" {
		since, err = time.Parse(time.RFC3339, args.Since)
		if err != nil {
			return errResult("invalid since (want RFC3339): %v", err), ReadMessagesResult{}, nil
		}
	}

	var raw []*messaging.Message
	if args.Recipient == "" {
		raw, err = readAllInboxes(run.bus, run.msgsDir)
	} else {
		var folder string
		folder, err = resolveRecipient(run.msgsDir, args.Recipient)
		if err == nil {
			raw, err = run.bus.ReadInbox(folder)
		}
	}
	if err != nil {
		return errResult("read messages: %v", err), ReadMessagesResult{}, nil
	}

	views := toMessageViews(raw, args.RunID, since)
	return textResult(fmt.Sprintf("%d message(s)", len(views))), ReadMessagesResult{Messages: views}, nil
}

// resolvedRun bundles the registry entry with the bus rooted at its messages
// directory. busForRun returns this so callers (handlers and resource readers
// alike) get the workspace dir for state reads without a second registry hit.
type resolvedRun struct {
	entry   Entry
	bus     *messaging.Bus
	msgsDir string
}

// busForRun looks up the run by id and returns the bus, message directory,
// and underlying entry. Distinguishes ENOTFOUND in the registry from a
// registry-read I/O error so resource handlers can map each appropriately.
func (s *Server) busForRun(ctx context.Context, runID string) (resolvedRun, error) {
	entry, ok, err := s.registry.Get(ctx, runID)
	if err != nil {
		return resolvedRun{}, fmt.Errorf("read registry: %w", err)
	}
	if !ok {
		return resolvedRun{}, fmt.Errorf("run %q not found", runID)
	}
	dir := messagesDir(entry.WorkspaceDir)
	return resolvedRun{entry: entry, bus: messaging.NewBus(dir), msgsDir: dir}, nil
}

// requireFileBus rejects message-bus operations against runs whose backend
// does not initialize the file bus. The MA backend skips the inbox seed
// (internal/run/service.go) — sending there would either fail with ENOENT
// or land messages no MA agent ever reads (steering for MA goes through
// session events). Surface that explicitly so the chat-side LLM redirects to
// the right path rather than silently no-op'ing.
//
// store.ErrNotFound is treated as "state not written yet": the engine writes
// state.json on first transition; a freshly-spawned run can be queried
// before that happens. Letting the caller proceed is correct — bus.Send will
// surface its own ENOENT if the inbox dir is missing in that race.
func (s *Server) requireFileBus(ctx context.Context, workspaceDir string) error {
	state, err := s.stateReader(ctx, stateDir(workspaceDir))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("read run state: %w", err)
	}
	if state != nil && state.Backend == backendManagedAgents {
		return errors.New(
			"run is managed_agents-backed and has no file bus; use the orchestra msg CLI to steer MA sessions via session events",
		)
	}
	return nil
}

func messagesDir(workspaceDir string) string {
	return filepath.Join(workspaceDir, orchestraSubdir, "messages")
}

// isBroadcastKeyword returns true when r is one of the recipient sentinels
// that fan out across every inbox. read_messages cannot narrow to "all"
// because there is no inbox by that name; send_message handles it by
// dispatching to bus.Send's broadcast path.
func isBroadcastKeyword(r string) bool {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case recipientBroadcast, recipientAll:
		return true
	default:
		return false
	}
}

// resolveRecipient maps the chat-side recipient label onto the indexed folder
// name the bus expects on disk. "broadcast" / "all" returns "all" — the bus
// special-cases this string to fan out to every inbox except the sender's.
//
// Friendly-name resolution matches the suffix of the indexed folder format
// `<index>-<name>`. The match is exact-on-name and case-insensitive: "design"
// maps to "2-design" but never to "2-api-design" (the prior HasSuffix-based
// match conflated those because both end in "-design").
func resolveRecipient(messagesDir, recipient string) (string, error) {
	r := strings.TrimSpace(recipient)
	if r == "" {
		return "", errors.New("recipient is required")
	}
	switch strings.ToLower(r) {
	case recipientHuman:
		return folderHuman, nil
	case recipientCoordinator:
		return folderCoordinator, nil
	case recipientBroadcast, recipientAll:
		return recipientAll, nil
	}
	if _, err := os.Stat(filepath.Join(messagesDir, r)); err == nil {
		return r, nil
	}
	entries, err := os.ReadDir(messagesDir)
	if err != nil {
		return "", fmt.Errorf("list inboxes: %w", err)
	}
	wantLower := strings.ToLower(r)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		_, name, ok := strings.Cut(e.Name(), "-")
		if !ok {
			continue
		}
		if strings.EqualFold(name, wantLower) {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("unknown recipient %q (no inbox with team name %q under %s)", r, r, messagesDir)
}

// readAllInboxes returns every message across every participant inbox under
// msgsDir. Used when read_messages is called without a recipient filter — the
// chat-side LLM gets a single timeline rather than having to fan out.
func readAllInboxes(bus *messaging.Bus, msgsDir string) ([]*messaging.Message, error) {
	entries, err := os.ReadDir(msgsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list inboxes: %w", err)
	}
	// Guard against double-counting on broadcast: a broadcast message is
	// fan-out written to every inbox except the sender's, so the same id
	// appears N-1 times. De-dupe by ID.
	seen := make(map[string]struct{})
	var out []*messaging.Message
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "shared" {
			continue
		}
		msgs, err := bus.ReadInbox(e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		for _, m := range msgs {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			out = append(out, m)
		}
	}
	return out, nil
}

// toMessageViews converts on-disk messages into MCP-exposed views, applies the
// `since` filter, and sorts newest-first. RunID is stamped onto every entry —
// the on-disk messaging.Message has no run_id since the path makes it implicit.
func toMessageViews(raw []*messaging.Message, runID string, since time.Time) []MessageView {
	out := make([]MessageView, 0, len(raw))
	for _, m := range raw {
		if !since.IsZero() && !m.Timestamp.After(since) {
			continue
		}
		out = append(out, MessageView{
			ID:        m.ID,
			RunID:     runID,
			Sender:    m.Sender,
			Recipient: m.Recipient,
			Type:      string(m.Type),
			Subject:   m.Subject,
			Content:   m.Content,
			ReplyTo:   m.ReplyTo,
			Read:      m.Read,
			WrittenAt: m.Timestamp,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WrittenAt.After(out[j].WrittenAt) })
	return out
}

// deriveSubject mirrors the orchestra-msg skill's first-60-chars convention so
// MCP-sent and skill-sent messages look the same on disk. Slices on rune
// boundaries so multibyte UTF-8 (emoji, CJK, accented characters) does not get
// cut mid-codepoint and produce mojibake.
func deriveSubject(content string) string {
	const limit = 60
	c := strings.TrimSpace(content)
	runes := []rune(c)
	if len(runes) <= limit {
		return c
	}
	return string(runes[:limit])
}

// newMessageID returns a globally-unique id of the form
// <unix_ms>-<sender>-<type>-<rand_hex>. The random suffix eliminates the
// silent-overwrite collision that the prior <ms>-<sender>-<type> shape
// allowed under same-millisecond same-sender same-type concurrency.
//
// crypto/rand.Read is documented to never fail in practice on supported
// platforms, but the error is propagated rather than swallowed so a
// constrained host environment surfaces it loudly instead of producing a
// degenerate id.
func newMessageID(sender, msgType string) (string, error) {
	buf := make([]byte, idRandSuffixBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return fmt.Sprintf("%d-%s-%s-%s",
		time.Now().UTC().UnixMilli(),
		sender,
		msgType,
		hex.EncodeToString(buf),
	), nil
}

// validateIDComponent rejects values that would let a caller escape the
// inbox via the message id (the id becomes the on-disk filename in
// bus.Send). The accepted alphabet — letters, digits, "-", "_" — is the
// shape internal/messaging.BuildParticipants produces and the
// orchestra-msg skill writes today. Rejecting "." entirely keeps the
// special filesystem names "." and ".." out of the id.
func validateIDComponent(field, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", field)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("%s %q has invalid character %q (allowed: a-z A-Z 0-9 - _)", field, v, r)
		}
	}
	return nil
}
