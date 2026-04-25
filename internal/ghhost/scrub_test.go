package ghhost

import (
	"errors"
	"fmt"
	"testing"
)

func TestScrub(t *testing.T) {
	if got := Scrub("secret", "x secret y secret z"); got != "x *** y *** z" {
		t.Fatalf("got %q", got)
	}
	if got := Scrub("", "x secret y"); got != "x secret y" {
		t.Fatalf("empty token must be no-op, got %q", got)
	}
	if got := Scrub("secret", ""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestScrubError_NilOrEmptyToken(t *testing.T) {
	if got := ScrubError(nil, "secret"); got != nil {
		t.Fatalf("nil err should pass through, got %v", got)
	}
	original := errors.New("contains secret in message")
	got := ScrubError(original, "")
	if errors.Unwrap(got) != nil {
		t.Fatalf("empty token should not wrap err: unwrap = %v", errors.Unwrap(got))
	}
	if got.Error() != original.Error() {
		t.Fatalf("empty token must leave message intact, got %q", got.Error())
	}
}

func TestScrubError_PreservesSentinel(t *testing.T) {
	wrapped := fmt.Errorf("got token=secret-123 while looking for branch: %w", ErrBranchNotFound)
	scrubbed := ScrubError(wrapped, "secret-123")
	if !errors.Is(scrubbed, ErrBranchNotFound) {
		t.Fatal("scrubbing must preserve sentinel matching")
	}
	if contains(scrubbed.Error(), "secret-123") {
		t.Fatalf("scrubbed message still contains token: %q", scrubbed.Error())
	}
	if !contains(scrubbed.Error(), "***") {
		t.Fatalf("scrubbed message missing replacement marker: %q", scrubbed.Error())
	}
}

func TestScrubError_NoMatch(t *testing.T) {
	original := errors.New("plain message")
	got := ScrubError(original, "absent-token")
	if errors.Unwrap(got) != nil {
		t.Fatalf("scrubber must not wrap when token is absent: unwrap = %v", errors.Unwrap(got))
	}
	if got.Error() != original.Error() {
		t.Fatalf("scrubber must not change message when token is absent, got %q", got.Error())
	}
}
