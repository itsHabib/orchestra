package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/messaging"
)

// seedRunID is the canonical run id used by the test helpers. Each test gets
// a fresh server (and tempdir), so the same id can appear across files
// without collision.
const seedRunID = "alpha"

// seedRun builds a registered run with a real on-disk message bus rooted at
// <wsRoot>/alpha/.orchestra/messages and returns the bus. Tests that assert
// send/read end-to-end against a real bus call this rather than stubbing.
// teamNames is the set of participants beyond the implicit human +
// coordinator (e.g. {"design", "build"}).
func seedRun(t *testing.T, srv *Server, teamNames []string) *messaging.Bus {
	t.Helper()
	wsDir := filepath.Join(srv.WorkspaceRoot(), seedRunID)
	msgsDir := filepath.Join(wsDir, ".orchestra", "messages")
	bus := messaging.NewBus(msgsDir)
	if err := bus.InitInboxes(teamNames); err != nil {
		t.Fatalf("InitInboxes: %v", err)
	}
	if err := srv.Registry().Put(context.Background(), &Entry{
		RunID:        seedRunID,
		WorkspaceDir: wsDir,
		StartedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Registry.Put: %v", err)
	}
	return bus
}

func TestHandleSendMessage_HappyPath_TeamRecipient(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	bus := seedRun(t, srv, []string{"design", "build"})

	res, out, err := srv.handleSendMessage(context.Background(), nil, SendMessageArgs{
		RunID:     "alpha",
		Recipient: "design",
		Content:   "please add edge cases",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if out.Recipient != "2-design" {
		t.Fatalf("recipient folder: got %q, want %q", out.Recipient, "2-design")
	}

	got, err := bus.ReadInbox("2-design")
	if err != nil {
		t.Fatalf("ReadInbox: %v", err)
	}
	if len(got) != 1 || got[0].Content != "please add edge cases" {
		t.Fatalf("messages: %+v", got)
	}
	if got[0].Sender != "0-human" {
		t.Fatalf("default sender: got %q, want %q", got[0].Sender, "0-human")
	}
}

func TestHandleSendMessage_BroadcastFansOut(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	bus := seedRun(t, srv, []string{"design", "build"})

	_, _, err := srv.handleSendMessage(context.Background(), nil, SendMessageArgs{
		RunID:     "alpha",
		Recipient: "broadcast",
		Content:   "API contract published",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	for _, folder := range []string{"1-coordinator", "2-design", "3-build"} {
		msgs, err := bus.ReadInbox(folder)
		if err != nil {
			t.Fatalf("ReadInbox %s: %v", folder, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("%s: got %d messages, want 1", folder, len(msgs))
		}
	}
	human, err := bus.ReadInbox("0-human")
	if err != nil {
		t.Fatalf("ReadInbox 0-human: %v", err)
	}
	if len(human) != 0 {
		t.Fatalf("broadcast should not echo to sender; got %d", len(human))
	}
}

func TestHandleSendMessage_RejectsUnknownRecipient(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	seedRun(t, srv, []string{"design"})

	res, _, err := srv.handleSendMessage(context.Background(), nil, SendMessageArgs{
		RunID:     "alpha",
		Recipient: "ghost",
		Content:   "hi",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on unknown recipient")
	}
}

func TestHandleSendMessage_RequiresFields(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	cases := []struct {
		name string
		args SendMessageArgs
		want string
	}{
		{"no run id", SendMessageArgs{Recipient: "design", Content: "hi"}, "run_id"},
		{"no recipient", SendMessageArgs{RunID: "alpha", Content: "hi"}, "recipient"},
		{"no content", SendMessageArgs{RunID: "alpha", Recipient: "design"}, "content"},
	}
	for _, tc := range cases {
		res, _, err := srv.handleSendMessage(context.Background(), nil, tc.args)
		if err != nil {
			t.Fatalf("%s: handler error: %v", tc.name, err)
		}
		if !res.IsError {
			t.Fatalf("%s: expected IsError", tc.name)
		}
		if !strings.Contains(resultText(res), tc.want) {
			t.Fatalf("%s: error text %q missing %q", tc.name, resultText(res), tc.want)
		}
	}
}

func TestHandleSendMessage_RunNotFound(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))

	res, _, err := srv.handleSendMessage(context.Background(), nil, SendMessageArgs{
		RunID:     "ghost",
		Recipient: "design",
		Content:   "hi",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError")
	}
	if !strings.Contains(resultText(res), "ghost") {
		t.Fatalf("error text: %q", resultText(res))
	}
}

func TestHandleReadMessages_AggregatesNewestFirst(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	bus := seedRun(t, srv, []string{"design", "build"})

	earlier := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	mustSend(t, bus, "2-design", &messaging.Message{
		ID:        "100-0-human-correction",
		Sender:    "0-human",
		Recipient: "2-design",
		Type:      messaging.MsgCorrection,
		Content:   "first",
		Timestamp: earlier,
	})
	mustSend(t, bus, "3-build", &messaging.Message{
		ID:        "200-0-human-correction",
		Sender:    "0-human",
		Recipient: "3-build",
		Type:      messaging.MsgCorrection,
		Content:   "second",
		Timestamp: later,
	})

	res, out, err := srv.handleReadMessages(context.Background(), nil, ReadMessagesArgs{
		RunID: "alpha",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", resultText(res))
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages: %d", len(out.Messages))
	}
	if out.Messages[0].Content != "second" {
		t.Fatalf("expected newest-first ordering; got %s then %s", out.Messages[0].Content, out.Messages[1].Content)
	}
	if out.Messages[0].RunID != "alpha" {
		t.Fatalf("RunID stamp: got %q", out.Messages[0].RunID)
	}
}

func TestHandleReadMessages_FilterBySince(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	bus := seedRun(t, srv, []string{"design"})

	earlier := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	mustSend(t, bus, "2-design", &messaging.Message{
		ID: "early", Sender: "0-human", Recipient: "2-design",
		Type: messaging.MsgCorrection, Content: "old", Timestamp: earlier,
	})
	mustSend(t, bus, "2-design", &messaging.Message{
		ID: "late", Sender: "0-human", Recipient: "2-design",
		Type: messaging.MsgCorrection, Content: "new", Timestamp: later,
	})

	cutoff := earlier.Add(time.Minute).Format(time.RFC3339)
	_, out, err := srv.handleReadMessages(context.Background(), nil, ReadMessagesArgs{
		RunID: "alpha",
		Since: cutoff,
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].ID != "late" {
		ids := make([]string, 0, len(out.Messages))
		for _, m := range out.Messages {
			ids = append(ids, m.ID)
		}
		t.Fatalf("filter: got %v, want [late]", ids)
	}
}

func TestHandleReadMessages_NarrowsToOneInbox(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	bus := seedRun(t, srv, []string{"design", "build"})

	mustSend(t, bus, "2-design", &messaging.Message{
		ID: "d1", Sender: "0-human", Recipient: "2-design",
		Type: messaging.MsgCorrection, Content: "to design", Timestamp: time.Now().UTC(),
	})
	mustSend(t, bus, "3-build", &messaging.Message{
		ID: "b1", Sender: "0-human", Recipient: "3-build",
		Type: messaging.MsgCorrection, Content: "to build", Timestamp: time.Now().UTC(),
	})

	_, out, err := srv.handleReadMessages(context.Background(), nil, ReadMessagesArgs{
		RunID:     "alpha",
		Recipient: "design",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].ID != "d1" {
		t.Fatalf("recipient narrow: %+v", out.Messages)
	}
}

func TestHandleReadMessages_RejectsBadSince(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	seedRun(t, srv, []string{"design"})

	res, _, err := srv.handleReadMessages(context.Background(), nil, ReadMessagesArgs{
		RunID: "alpha",
		Since: "yesterday",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on bad since")
	}
}

func TestHandleReadMessages_BroadcastDeduped(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, &stubSpawner{}, stateReaderFn(nil))
	bus := seedRun(t, srv, []string{"design", "build"})

	if err := bus.Send(&messaging.Message{
		ID:        "1000-0-human-broadcast",
		Sender:    "0-human",
		Recipient: "all",
		Type:      messaging.MsgBroadcast,
		Content:   "hello everyone",
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Send broadcast: %v", err)
	}

	_, out, err := srv.handleReadMessages(context.Background(), nil, ReadMessagesArgs{RunID: "alpha"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(out.Messages) != 1 {
		ids := make([]string, 0, len(out.Messages))
		for _, m := range out.Messages {
			ids = append(ids, m.ID)
		}
		t.Fatalf("broadcast aggregated: got %d (%v), want 1 deduped entry", len(out.Messages), ids)
	}
}

func mustSend(t *testing.T, bus *messaging.Bus, _ string, msg *messaging.Message) {
	t.Helper()
	if err := bus.Send(msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
}
