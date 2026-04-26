package config

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidConfig is wrapped by [Result.Err] whenever the result has at
// least one entry in Errors. Callers can use errors.Is to recognize
// validation failures regardless of how the formatted message changes:
//
//	res := cfg.Validate()
//	if errors.Is(res.Err(), config.ErrInvalidConfig) { ... }
//
// pkg/orchestra re-exports this sentinel as orchestra.ErrInvalidConfig.
var ErrInvalidConfig = errors.New("orchestra: invalid config")

// Result is the aggregate output of [Load] and [Config.Validate]. It
// carries the parsed config (when valid), the structured warnings, and
// the structured errors. Use [Result.Valid] to gate further use of
// Config; use [Result.Err] for an error-shaped view of the validation
// failures suitable for `if err != nil` patterns.
//
// pkg/orchestra re-exports this type as orchestra.ValidationResult.
type Result struct {
	// Config is the parsed, defaults-resolved config. Nil when the
	// result is invalid (Valid() == false) or when Load returned a
	// non-nil error.
	Config *Config

	// Warnings is the slice of soft validation issues. Order is the
	// order each validator emitted them; same as the pre-P2.5
	// []Warning slice.
	Warnings []Warning

	// Errors is the slice of hard validation failures. Empty when
	// Valid().
	Errors []ConfigError
}

// Valid returns true when no hard validation errors were recorded.
// Warnings do not affect validity.
func (r *Result) Valid() bool {
	return r != nil && len(r.Errors) == 0
}

// Err returns nil when Valid. Otherwise it returns an error wrapping
// [ErrInvalidConfig] with the formatted "validation errors:\n  - ..."
// text the CLI relies on for byte-identical output. Use errors.Is to
// test for invalidity:
//
//	if errors.Is(res.Err(), config.ErrInvalidConfig) { ... }
func (r *Result) Err() error {
	if r.Valid() {
		return nil
	}
	msgs := make([]string, len(r.Errors))
	for i, e := range r.Errors {
		msgs[i] = e.String()
	}
	return &invalidConfigError{
		msg: fmt.Sprintf("validation errors:\n  - %s", strings.Join(msgs, "\n  - ")),
	}
}

// invalidConfigError carries the formatted multi-line "validation
// errors:\n  - ..." string the CLI prints byte-for-byte and unwraps to
// [ErrInvalidConfig] so callers can match with errors.Is.
type invalidConfigError struct {
	msg string
}

func (e *invalidConfigError) Error() string { return e.msg }
func (e *invalidConfigError) Unwrap() error { return ErrInvalidConfig }

// ConfigError is a hard validation failure with the same shape and
// semantics as [Warning]. Two parallel types — not a unified Issue with
// Severity — because the warning vs. error distinction is
// domain-meaningful and consumers iterate the slice they care about.
//
// pkg/orchestra re-exports this type as orchestra.ConfigError.
type ConfigError struct {
	// Field is the structured YAML path to the offending node, e.g.
	// {"teams", "0", "tasks", "2", "verify"} for a missing verify on
	// team 0's third task. Empty for project-level issues (missing
	// project name, unknown backend.kind, etc.).
	Field []string
	// Team is the denormalized team name when Field points into a team
	// subtree; empty otherwise. Exists for ergonomic display so
	// String() can render `team "foo": message` without walking Field
	// back into Config. Programmatic consumers should prefer Field.
	Team string
	// Message is the human-readable description of the issue.
	Message string
}

// String returns the human-readable form: `team "foo": message` when
// Team is non-empty, else just Message. Matches [Warning.String]
// exactly so CLI rendering is unchanged.
func (e ConfigError) String() string {
	if e.Team != "" {
		return fmt.Sprintf("team %q: %s", e.Team, e.Message)
	}
	return e.Message
}
