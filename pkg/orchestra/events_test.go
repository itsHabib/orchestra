package orchestra_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/pkg/orchestra"
)

// TestStart_HandleEvents_DeliversInOrder runs a single-team workflow
// against the mock-claude binary and drains the Events channel. The
// observed sequence must include EventTierStart, EventTeamStart,
// EventTeamComplete, EventTierComplete, EventRunComplete in that
// order — those are the lifecycle anchors PR 2 commits to. Other event
// kinds (EventInfo, EventTeamMessage) interleave and are tolerated; the
// test just pins the lifecycle skeleton.
func TestStart_HandleEvents_DeliversInOrder(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)

	res, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.Config
	withPath(t, binDir)
	chdir(t, workDir)

	h, err := orchestra.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var kinds []orchestra.EventKind
	for ev := range h.Events() {
		kinds = append(kinds, ev.Kind)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	want := []orchestra.EventKind{
		orchestra.EventTierStart,
		orchestra.EventTeamStart,
		orchestra.EventTeamComplete,
		orchestra.EventTierComplete,
		orchestra.EventRunComplete,
	}
	if !subsequence(kinds, want) {
		t.Fatalf("event sequence missing lifecycle anchors:\nwant subseq %v\ngot %v", kindNames(want), kindNames(kinds))
	}
}

// TestEventChannel_DropsOldestUnderBackpressure starts a run with the
// minimum event buffer (1) and never drains the channel. The engine emits
// a healthy stream of EventInfo / EventTier* / EventTeam* events; the
// bounded channel must surface backpressure via at least one EventDropped
// with DropCount > 0 once the run completes.
func TestEventChannel_DropsOldestUnderBackpressure(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)

	res, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.Config
	withPath(t, binDir)
	chdir(t, workDir)

	h, err := orchestra.Start(context.Background(), cfg, orchestra.WithEventBuffer(1))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Drain whatever survived the backpressure window. The channel is
	// closed by the engine epilogue; ranging is safe.
	sawDropped := false
	totalDropped := 0
	for ev := range h.Events() {
		if ev.Kind == orchestra.EventDropped {
			sawDropped = true
			totalDropped += ev.DropCount
		}
	}
	if !sawDropped {
		t.Fatal("expected at least one EventDropped under backpressure with buffer=1, saw none")
	}
	if totalDropped <= 0 {
		t.Errorf("EventDropped surfaced but DropCount sum=%d, want > 0", totalDropped)
	}
}

// TestWithEventHandler_FiresInline registers an event handler via Run and
// asserts that the callback receives the run's lifecycle events. The
// handler is the one-shot replacement for managing a Handle and a
// goroutine — it must fire on the engine's emit path before the channel
// send.
func TestWithEventHandler_FiresInline(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)

	loaded, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loaded.Config
	withPath(t, binDir)
	chdir(t, workDir)

	var (
		mu     sync.Mutex
		events []orchestra.Event
	)
	res, err := orchestra.Run(context.Background(), cfg,
		orchestra.WithEventHandler(func(ev orchestra.Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("Run: expected non-nil result")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("WithEventHandler callback fired zero times")
	}
	kinds := make([]orchestra.EventKind, len(events))
	for i := range events {
		kinds[i] = events[i].Kind
	}
	for _, want := range []orchestra.EventKind{
		orchestra.EventTierStart,
		orchestra.EventTeamComplete,
		orchestra.EventRunComplete,
	} {
		if !contains(kinds, want) {
			t.Errorf("WithEventHandler did not see %s; saw %v", kindName(want), kindNames(kinds))
		}
	}
}

// TestWithEventBuffer_ZeroIsClampedToOne pins the documented "minimum 1"
// behavior for WithEventBuffer. Passing 0 must not produce an unbuffered
// channel (which would force the engine to block on the consumer for
// every event); it should be silently clamped to 1.
func TestWithEventBuffer_ZeroIsClampedToOne(t *testing.T) {
	binDir := t.TempDir()
	writeMockClaude(t, binDir, mockSuccessStream(), nil, 0, 0)

	workDir := t.TempDir()
	configPath := writeOneTeamConfig(t, workDir)

	res, err := orchestra.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.Config
	withPath(t, binDir)
	chdir(t, workDir)

	h, err := orchestra.Start(context.Background(), cfg, orchestra.WithEventBuffer(0))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Don't drain. With buffer=1 the engine must not deadlock on the
	// emit path — Wait should still return cleanly.
	done := make(chan struct{})
	go func() {
		_, _ = h.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Wait did not return — engine appears blocked on event emit (buffer=0 not clamped)")
	}
	// Drain the channel so the test exits cleanly. Discard each value;
	// we only care that the engine is no longer blocked on emit.
	for range h.Events() { //nolint:revive // intentionally empty drain loop
	}
}

// --- helpers --------------------------------------------------------------

// subsequence reports whether want appears in got as a subsequence — each
// element of want appears in got in order, with arbitrary other elements
// allowed in between.
func subsequence(got, want []orchestra.EventKind) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// contains reports whether k appears anywhere in kinds.
func contains(kinds []orchestra.EventKind, k orchestra.EventKind) bool {
	for _, kind := range kinds {
		if kind == k {
			return true
		}
	}
	return false
}

// kindNames returns the printable names for a slice of kinds — keeps the
// test failure messages readable without sprinkling string literals
// throughout assertions.
func kindNames(kinds []orchestra.EventKind) []string {
	out := make([]string, len(kinds))
	for i := range kinds {
		out[i] = kindName(kinds[i])
	}
	return out
}

func kindName(k orchestra.EventKind) string {
	switch k {
	case orchestra.EventTierStart:
		return "TierStart"
	case orchestra.EventTeamStart:
		return "TeamStart"
	case orchestra.EventTeamMessage:
		return "TeamMessage"
	case orchestra.EventToolCall:
		return "ToolCall"
	case orchestra.EventToolResult:
		return "ToolResult"
	case orchestra.EventTeamComplete:
		return "TeamComplete"
	case orchestra.EventTeamFailed:
		return "TeamFailed"
	case orchestra.EventTierComplete:
		return "TierComplete"
	case orchestra.EventRunComplete:
		return "RunComplete"
	case orchestra.EventDropped:
		return "Dropped"
	case orchestra.EventInfo:
		return "Info"
	case orchestra.EventWarn:
		return "Warn"
	case orchestra.EventError:
		return "Error"
	default:
		return "Unknown"
	}
}
