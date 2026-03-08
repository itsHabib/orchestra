package messaging

import "time"

// MessageType enumerates the kinds of inter-agent messages.
type MessageType string

const (
	MsgQuestion         MessageType = "question"
	MsgAnswer           MessageType = "answer"
	MsgInterfaceContract MessageType = "interface-contract"
	MsgStatusUpdate     MessageType = "status-update"
	MsgCorrection       MessageType = "correction"
	MsgBlockingIssue    MessageType = "blocking-issue"
	MsgAck              MessageType = "ack"
	MsgBroadcast        MessageType = "broadcast"
	MsgGate             MessageType = "gate"      // human-in-the-loop decision required
	MsgBootstrap        MessageType = "bootstrap" // seeded by orchestrator before team starts
)

// Message is the JSON structure for all inter-agent messages.
type Message struct {
	ID        string      `json:"id"`                    // unique: <timestamp_ms>-<sender>-<type>
	Sender    string      `json:"sender"`                // inbox folder name or "0-human"
	Recipient string      `json:"recipient"`             // inbox folder name, or "all" for broadcast
	Type      MessageType `json:"type"`
	Subject   string      `json:"subject"`               // short summary
	Content   string      `json:"content"`               // full message body
	ReplyTo   string      `json:"reply_to,omitempty"`    // ID of message being replied to
	Priority  string      `json:"priority,omitempty"`    // "normal", "high", "critical"
	Timestamp time.Time   `json:"timestamp"`
	Read      bool        `json:"read"`                  // agents mark as true after processing
}
