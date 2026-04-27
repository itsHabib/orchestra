// Package notify delivers signal_completion notifications to a fan-out of
// host-side sinks: an append-only NDJSON log, a TTY bell + line, and a
// best-effort platform notifier (osascript / notify-send / no-op on Windows).
//
// The fan-out tolerates per-sink failures — a broken system notifier never
// blocks the run. Only the engine writes the NDJSON log, so coordination
// across the sinks is in-process: a single mutex on the log writer suffices,
// no cross-process flock.
package notify

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// Notification is one team's signal_completion event. Reflects the
// signal_completion custom-tool input, augmented with run-level context the
// dispatcher has but the agent doesn't (Timestamp, RunID).
type Notification struct {
	Timestamp time.Time
	RunID     string
	Team      string
	Status    string // "done" | "blocked"
	Summary   string
	PRURL     string
	Reason    string
}

// Notifier delivers a Notification to one sink. Implementations must be safe
// for concurrent use by multiple teams in the same run; sibling teams in
// tier 0 finish in parallel and may signal at nearly the same instant.
//
// Notification is passed by pointer because the struct is heavy (~120 bytes)
// and the project's lint config flags by-value passes above 88. Implementations
// must not mutate fields the caller still owns; treat n as read-only.
type Notifier interface {
	Notify(ctx context.Context, n *Notification) error
}

// NotifierFunc adapts a plain function into a Notifier — useful in tests.
type NotifierFunc func(context.Context, *Notification) error

// Notify implements Notifier.
func (f NotifierFunc) Notify(ctx context.Context, n *Notification) error {
	return f(ctx, n)
}

// Compose returns a Notifier that calls every component in order. A failing
// component is logged and skipped — the composite Notify always returns nil
// because callers (the signal_completion handler) treat notification as
// best-effort and must not fail the agent's tool call on a flaky sink.
//
// The composite is order-preserving: the log writes before the terminal bell
// before the system shellout, so even if the system notifier hangs (osascript
// occasionally does on macOS) the durable log entry is already on disk.
func Compose(logger *slog.Logger, sinks ...Notifier) Notifier {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	clean := make([]Notifier, 0, len(sinks))
	for _, s := range sinks {
		if s == nil {
			continue
		}
		clean = append(clean, s)
	}
	return &fanOut{sinks: clean, logger: logger}
}

type fanOut struct {
	sinks  []Notifier
	logger *slog.Logger
}

func (f *fanOut) Notify(ctx context.Context, n *Notification) error {
	if n == nil {
		return nil
	}
	for _, sink := range f.sinks {
		if err := sink.Notify(ctx, n); err != nil {
			f.logger.Warn("notify component failed",
				"team", n.Team, "status", n.Status, "error", err)
		}
	}
	return nil
}

// Noop returns a Notifier that swallows every Notification. Useful as a
// default when the engine is constructed without an explicit notifier (tests,
// some pkg/orchestra entry points).
func Noop() Notifier { return noopNotifier{} }

type noopNotifier struct{}

func (noopNotifier) Notify(context.Context, *Notification) error { return nil }
