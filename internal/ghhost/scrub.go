package ghhost

import "strings"

// Scrub replaces every occurrence of token in s with "***". An empty token is
// a no-op so callers can scrub uniformly even when PAT resolution failed.
func Scrub(token, s string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// ScrubError returns a wrapper around err whose Error() has token replaced
// with "***". Sentinel and typed-error matching via errors.Is/errors.As is
// preserved through Unwrap. Empty token returns err unchanged.
func ScrubError(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	scrubbed := Scrub(token, msg)
	if scrubbed == msg {
		return err
	}
	return &scrubbedError{inner: err, msg: scrubbed}
}

type scrubbedError struct {
	inner error
	msg   string
}

func (s *scrubbedError) Error() string { return s.msg }
func (s *scrubbedError) Unwrap() error { return s.inner }
