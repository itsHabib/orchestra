package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestComposeFanOutInvokesEverySink(t *testing.T) {
	t.Parallel()
	var first, second atomic.Int32
	composed := Compose(nil,
		NotifierFunc(func(context.Context, *Notification) error {
			first.Add(1)
			return nil
		}),
		NotifierFunc(func(context.Context, *Notification) error {
			second.Add(1)
			return nil
		}),
	)
	if err := composed.Notify(context.Background(), &Notification{Team: "alpha", Status: "done"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("expected both sinks to fire, got first=%d second=%d", first.Load(), second.Load())
	}
}

func TestComposeToleratesFailingComponent(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	composed := Compose(slog.New(slog.NewTextHandler(io.Discard, nil)),
		NotifierFunc(func(context.Context, *Notification) error {
			return errors.New("sink down")
		}),
		NotifierFunc(func(context.Context, *Notification) error {
			hits.Add(1)
			return nil
		}),
	)
	if err := composed.Notify(context.Background(), &Notification{Team: "alpha"}); err != nil {
		t.Fatalf("compose should swallow component errors, got %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("downstream sink should still fire after upstream failure")
	}
}

func TestComposeSkipsNilSinks(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	composed := Compose(nil,
		nil,
		NotifierFunc(func(context.Context, *Notification) error {
			hits.Add(1)
			return nil
		}),
		nil,
	)
	if err := composed.Notify(context.Background(), &Notification{Team: "alpha"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected the non-nil sink to fire once, got %d", hits.Load())
	}
}

func TestNoopAlwaysSucceeds(t *testing.T) {
	t.Parallel()
	if err := Noop().Notify(context.Background(), &Notification{}); err != nil {
		t.Fatalf("noop should never fail: %v", err)
	}
}

func TestLogNotifierAppendsNDJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "notifications.ndjson")
	n := NewLog(path)
	stamp := time.Date(2026, 4, 26, 9, 0, 0, 0, time.UTC)

	for i, status := range []string{"done", "blocked"} {
		ts := stamp.Add(time.Duration(i) * time.Minute)
		err := n.Notify(context.Background(), &Notification{
			Timestamp: ts,
			RunID:     "run_1",
			Team:      "alpha",
			Status:    status,
			Summary:   "ok",
		})
		if err != nil {
			t.Fatalf("notify %d: %v", i, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), lines)
	}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not valid JSON: %v\n%q", i, err, line)
		}
		if rec["team"] != "alpha" {
			t.Fatalf("line %d: team mismatch: %v", i, rec["team"])
		}
	}
}

func TestLogNotifierCreatesParentDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "notifications.ndjson")
	n := NewLog(path)
	if err := n.Notify(context.Background(), &Notification{Team: "alpha", Status: "done", Summary: "x"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestLogNotifierFillsZeroTimestamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "notifications.ndjson")
	n := NewLog(path)
	if err := n.Notify(context.Background(), &Notification{Team: "alpha", Status: "done", Summary: "x"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	data, _ := os.ReadFile(path)
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &rec); err != nil {
		t.Fatalf("parse: %v", err)
	}
	ts, ok := rec["ts"].(string)
	if !ok || ts == "" {
		t.Fatalf("ts not populated when input timestamp was zero: %+v", rec)
	}
}

func TestTerminalNotifierWritesOnlyToTTY(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	tty := newTerminalForTest(&buf, true)
	if err := tty.Notify(context.Background(), &Notification{Team: "alpha", Status: "done", Summary: "ok", RunID: "run_1"}); err != nil {
		t.Fatalf("notify tty: %v", err)
	}
	if !strings.Contains(buf.String(), "[NOTIFY] run_1/alpha: done — ok") {
		t.Fatalf("missing line in tty output: %q", buf.String())
	}
	if !strings.HasPrefix(buf.String(), "\a") {
		t.Fatalf("missing bell prefix: %q", buf.String())
	}

	buf.Reset()
	notTTY := newTerminalForTest(&buf, false)
	if err := notTTY.Notify(context.Background(), &Notification{Team: "alpha"}); err != nil {
		t.Fatalf("notify non-tty: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("non-tty writer should not emit anything, got %q", buf.String())
	}
}

func TestFormatTerminalLineFallsBackForBlankRunID(t *testing.T) {
	t.Parallel()
	got := FormatTerminalLine(&Notification{Team: "alpha", Status: "done", Summary: "ok"})
	if !strings.Contains(got, "-/alpha") {
		t.Fatalf("expected `-` placeholder for empty run id, got %q", got)
	}
}

func TestFormatTerminalLineStripsControlChars(t *testing.T) {
	t.Parallel()
	got := FormatTerminalLine(&Notification{
		RunID:   "run\x07_1",
		Team:    "alpha\nbeta",
		Status:  "done\r",
		Summary: "ship\x1b[31m red \x1b[0m it",
	})
	// The format scaffold contributes one trailing '\n' and the leading bell
	// (0x07). Strip those before checking for control-byte leaks from the
	// agent-provided fields, then assert no new control bytes survived.
	if got[0] != '\a' {
		t.Fatalf("bell prefix missing")
	}
	body := strings.TrimSuffix(got[1:], "\n")
	for _, c := range []byte{'\n', '\r', 0x07, 0x1b} {
		if strings.ContainsRune(body, rune(c)) {
			t.Fatalf("control byte 0x%02x leaked through to body: %q", c, body)
		}
	}
	if !strings.Contains(got, "alpha beta") {
		t.Fatalf("newline in team should have been replaced with space: %q", got)
	}
}

func TestFormatTerminalLineTruncatesLongSummary(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 500)
	got := FormatTerminalLine(&Notification{Team: "alpha", Status: "done", Summary: long})
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "...") {
		t.Fatalf("expected ellipsis suffix on truncated summary, got %q", got)
	}
	if len(got) > 256 {
		t.Fatalf("rendered line too long: %d bytes", len(got))
	}
}

func TestRunBoundedCommandTimesOut(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("sleep is not on PATH by default on Windows; this test asserts unix shell shape")
	}
	// runBoundedCommand uses systemNotifyTimeout (3s) internally; we want a
	// shorter horizon so the test is fast. Override via the unexported
	// helper: pass a short ctx and let the inner timeout still fire — sleep
	// 10s will exceed both. The deadline-exceeded branch sets a clear error
	// message so we can assert without timing the test on millisecond bounds.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	start := time.Now()
	err := runBoundedCommand(ctx, "sleep", "sleep", "10")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if elapsed > systemNotifyTimeout+500*time.Millisecond {
		t.Fatalf("runBoundedCommand should cap at %s, elapsed=%s", systemNotifyTimeout, elapsed)
	}
}
