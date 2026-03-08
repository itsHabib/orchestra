package messaging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/michaelhabib/orchestra/internal/fsutil"
)

// Bus provides filesystem-based message operations.
type Bus struct {
	basePath string
	mu       sync.Mutex
}

// NewBus creates a Bus rooted at the given path (e.g., "messages/").
func NewBus(basePath string) *Bus {
	return &Bus{basePath: basePath}
}

// Path returns the base path of the message bus.
func (b *Bus) Path() string {
	return b.basePath
}

// Participant holds the index and name for an inbox folder.
type Participant struct {
	Index int
	Name  string
}

// FolderName returns the indexed folder name (e.g., "0-human", "1-coordinator").
func (p Participant) FolderName() string {
	return fmt.Sprintf("%d-%s", p.Index, p.Name)
}

// BuildParticipants creates the ordered list of all message participants.
// Order: 0-human, 1-coordinator, 2-<first team>, 3-<second team>, ...
func BuildParticipants(teamNames []string) []Participant {
	participants := []Participant{
		{Index: 0, Name: "human"},
		{Index: 1, Name: "coordinator"},
	}
	for i, name := range teamNames {
		participants = append(participants, Participant{Index: i + 2, Name: name})
	}
	return participants
}

// InitInboxes creates inbox directories for all participants plus shared/.
func (b *Bus) InitInboxes(teamNames []string) error {
	participants := BuildParticipants(teamNames)
	for _, p := range participants {
		dir := filepath.Join(b.basePath, p.FolderName(), "inbox")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating inbox for %s: %w", p.FolderName(), err)
		}
	}
	// shared/ for broadcast artifacts
	if err := os.MkdirAll(filepath.Join(b.basePath, "shared"), 0o755); err != nil {
		return fmt.Errorf("creating shared dir: %w", err)
	}
	return nil
}

// Send writes a message to the recipient's inbox using atomic writes.
// If recipient is "all", copies to every inbox except the sender's.
func (b *Bus) Send(msg *Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	data, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}

	filename := fmt.Sprintf("%s.json", msg.ID)

	if msg.Recipient == "all" {
		// Broadcast: write to every inbox except sender
		entries, err := os.ReadDir(b.basePath)
		if err != nil {
			return fmt.Errorf("reading message bus dir: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Name() == "shared" {
				continue
			}
			// Skip sender's own inbox
			if entry.Name() == msg.Sender {
				continue
			}
			path := filepath.Join(b.basePath, entry.Name(), "inbox", filename)
			if err := fsutil.AtomicWrite(path, data); err != nil {
				return fmt.Errorf("broadcasting to %s: %w", entry.Name(), err)
			}
		}
		return nil
	}

	// Direct message: write to recipient's inbox
	path := filepath.Join(b.basePath, msg.Recipient, "inbox", filename)
	return fsutil.AtomicWrite(path, data)
}

// ReadInbox returns all messages for a recipient, sorted by timestamp (filename prefix).
func (b *Bus) ReadInbox(recipient string) ([]*Message, error) {
	dir := filepath.Join(b.basePath, recipient, "inbox")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Sort by name (timestamp prefix ensures chronological order)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var messages []*Message
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		messages = append(messages, &msg)
	}
	return messages, nil
}

// ReadUnread returns only unread messages for a recipient.
func (b *Bus) ReadUnread(recipient string) ([]*Message, error) {
	all, err := b.ReadInbox(recipient)
	if err != nil {
		return nil, err
	}
	var unread []*Message
	for _, msg := range all {
		if !msg.Read {
			unread = append(unread, msg)
		}
	}
	return unread, nil
}

// MarkRead marks a message as read by rewriting it with read=true.
func (b *Bus) MarkRead(recipient, messageID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	path := filepath.Join(b.basePath, recipient, "inbox", messageID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}
	msg.Read = true
	updated, err := json.MarshalIndent(&msg, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(path, updated)
}

// WriteShared writes a shared artifact (interface contract, schema, etc.)
func (b *Bus) WriteShared(name string, content []byte) error {
	path := filepath.Join(b.basePath, "shared", name)
	return fsutil.AtomicWrite(path, content)
}

// ReadShared reads a shared artifact by name.
func (b *Bus) ReadShared(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(b.basePath, "shared", name))
}

// ListShared returns names of all shared artifacts.
func (b *Bus) ListShared() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(b.basePath, "shared"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}
