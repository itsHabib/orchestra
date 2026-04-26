package orchestra

// defaultEventBuffer is the default capacity of [Handle.Events]. Sized so
// a typical run with a half-dozen teams and a few tiers can buffer the
// burst of TeamMessage / ToolCall events without a slow consumer dropping
// anything; tunable via [WithEventBuffer].
const defaultEventBuffer = 256

// Option configures a single [Run] invocation. Options are intentionally
// few; new ones should be motivated by a real consumer rather than added
// speculatively.
//
// Experimental: the option set will grow only as dogfood apps surface
// concrete needs.
type Option func(*runOptions)

// runOptions holds the per-call knobs assembled from caller [Option]s.
type runOptions struct {
	workspaceDir string
	eventBuffer  int
	eventHandler func(Event)
}

func defaultRunOptions() runOptions {
	return runOptions{
		workspaceDir: ".orchestra",
		eventBuffer:  defaultEventBuffer,
	}
}

// WithWorkspaceDir overrides the default ".orchestra" workspace location.
// Relative paths are resolved against os.Getwd() at [Run] call time,
// matching CLI behavior. Empty values are ignored.
func WithWorkspaceDir(path string) Option {
	return func(o *runOptions) {
		if path != "" {
			o.workspaceDir = path
		}
	}
}

// WithEventBuffer sets the buffer size of [Handle.Events]. Default is
// 256. Minimum is 1 — an unbuffered channel would force the engine to
// block on the consumer for every event, defeating the drop-oldest
// safety. Values below 1 are clamped to 1.
//
// Larger buffers reduce drop risk under bursty load at the cost of
// memory; smaller buffers surface backpressure (via [EventDropped])
// sooner.
//
// Experimental.
func WithEventBuffer(n int) Option {
	return func(o *runOptions) {
		if n < 1 {
			n = 1
		}
		o.eventBuffer = n
	}
}

// WithEventHandler registers a callback invoked synchronously by the
// engine for each emitted event before the event reaches the bounded
// channel returned by [Handle.Events]. Useful for one-shot callers using
// [Run] who don't want to manage a Handle and a goroutine for the
// channel.
//
// The callback must not block — it runs on the engine's emit path.
// Heavy work belongs in a goroutine consuming [Handle.Events] instead.
//
// If both [WithEventHandler] and a channel consumer are wired, both
// fire; the handler fires first so its observations precede the channel
// send.
//
// Passing nil clears any previously-set handler.
//
// Experimental.
func WithEventHandler(fn func(Event)) Option {
	return func(o *runOptions) {
		o.eventHandler = fn
	}
}
