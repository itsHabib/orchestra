package orchestra

// Option configures a single [Run] invocation. Options are intentionally
// few; new ones should be motivated by a real consumer rather than added
// speculatively.
//
// Experimental: the option set will grow only as dogfood apps surface
// concrete needs.
type Option func(*runOptions)

// runOptions holds the per-call knobs assembled from caller [Option]s.
type runOptions struct {
	logger       Logger
	workspaceDir string
}

func defaultRunOptions() runOptions {
	return runOptions{
		logger:       NewNoopLogger(),
		workspaceDir: ".orchestra",
	}
}

// WithLogger overrides the default [NewNoopLogger]. CLI callers typically
// pass [NewCLILogger]; UI integrations supply a custom implementation.
// Passing nil is treated as a no-op (the default logger is retained) so
// callers cannot accidentally disable logging mid-run.
func WithLogger(logger Logger) Option {
	return func(o *runOptions) {
		if logger != nil {
			o.logger = logger
		}
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
