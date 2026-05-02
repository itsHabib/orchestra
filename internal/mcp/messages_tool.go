package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itsHabib/orchestra/internal/messaging"
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
	Recipient string `json:"recipient,omitempty" jsonschema:"narrow to one inbox (team name, \"coordinator\", or \"human\"); aggregates across all inboxes when empty"`
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
	bus, msgsDir, err := s.busForRun(ctx, args.RunID)
	if err != nil {
		return errResult("%v", err), SendMessageResult{}, nil
	}
	folder, err := resolveRecipient(msgsDir, args.Recipient)
	if err != nil {
		return errResult("%v", err), SendMessageResult{}, nil
	}

	sender := args.Sender
	if sender == "" {
		sender = defaultSender
	}
	msgType := args.Type
	if msgType == "" {
		msgType = string(messaging.MsgCorrection)
	}
	subject := args.Subject
	if subject == "" {
		subject = deriveSubject(args.Content)
	}
	now := time.Now().UTC()
	msg := &messaging.Message{
		ID:        fmt.Sprintf("%d-%s-%s", now.UnixMilli(), sender, msgType),
		Sender:    sender,
		Recipient: folder,
		Type:      messaging.MessageType(msgType),
		Subject:   subject,
		Content:   args.Content,
		ReplyTo:   args.ReplyTo,
		Timestamp: now,
	}
	if err := bus.Send(msg); err != nil {
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
	bus, msgsDir, err := s.busForRun(ctx, args.RunID)
	if err != nil {
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
		raw, err = readAllInboxes(bus, msgsDir)
	} else {
		var folder string
		folder, err = resolveRecipient(msgsDir, args.Recipient)
		if err == nil {
			raw, err = bus.ReadInbox(folder)
		}
	}
	if err != nil {
		return errResult("read messages: %v", err), ReadMessagesResult{}, nil
	}

	views := toMessageViews(raw, args.RunID, since)
	return textResult(fmt.Sprintf("%d message(s)", len(views))), ReadMessagesResult{Messages: views}, nil
}

// busForRun returns a *messaging.Bus rooted at the run's on-disk messages
// directory plus the resolved messages dir for direct callers (resource
// handlers want the path too). Lookup walks the registry, so unknown run ids
// surface a clean "not found" rather than an opaque ENOENT later.
func (s *Server) busForRun(ctx context.Context, runID string) (*messaging.Bus, string, error) {
	entry, ok, err := s.registry.Get(ctx, runID)
	if err != nil {
		return nil, "", fmt.Errorf("read registry: %w", err)
	}
	if !ok {
		return nil, "", fmt.Errorf("run %q not found", runID)
	}
	dir := messagesDir(entry.WorkspaceDir)
	return messaging.NewBus(dir), dir, nil
}

func messagesDir(workspaceDir string) string {
	return filepath.Join(workspaceDir, orchestraSubdir, "messages")
}

// resolveRecipient maps the chat-side recipient label onto the indexed folder
// name the bus expects on disk. "broadcast" / "all" returns "all" — the bus
// special-cases this string to fan out to every inbox except the sender's.
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
	// Already an indexed folder name (e.g. "2-design") — pass through if
	// the directory exists. Otherwise look for any folder ending in "-r"
	// to support friendly team-name addressing.
	if _, err := os.Stat(filepath.Join(messagesDir, r)); err == nil {
		return r, nil
	}
	entries, err := os.ReadDir(messagesDir)
	if err != nil {
		return "", fmt.Errorf("list inboxes: %w", err)
	}
	suffix := "-" + r
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), suffix) {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("unknown recipient %q (no inbox folder ends in %q)", r, suffix)
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
// MCP-sent and skill-sent messages look the same on disk.
func deriveSubject(content string) string {
	const limit = 60
	c := strings.TrimSpace(content)
	if len(c) <= limit {
		return c
	}
	return c[:limit]
}
