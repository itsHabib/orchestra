package skills

import (
	"strings"
	"testing"
)

func TestContentHashDeterministic(t *testing.T) {
	t.Parallel()
	in := []byte("hello\nworld\n")
	got1 := ContentHash(in)
	got2 := ContentHash([]byte(string(in)))
	if got1 != got2 {
		t.Fatalf("hash not stable: %s vs %s", got1, got2)
	}
	if !strings.HasPrefix(got1, "sha256:") {
		t.Fatalf("hash missing sha256: prefix: %s", got1)
	}
}

func TestContentHashCRLFNormalized(t *testing.T) {
	t.Parallel()
	lf := []byte("hello\nworld\n")
	crlf := []byte("hello\r\nworld\r\n")
	if ContentHash(lf) != ContentHash(crlf) {
		t.Fatalf("CRLF not normalized: lf=%s crlf=%s",
			ContentHash(lf), ContentHash(crlf))
	}
}

func TestContentHashNFCNormalized(t *testing.T) {
	t.Parallel()
	// "café" composed (single NFC code point) vs decomposed (e + combining acute).
	composed := []byte("café")
	decomposed := []byte("café")
	if ContentHash(composed) != ContentHash(decomposed) {
		t.Fatalf("NFC not normalized: composed=%s decomposed=%s",
			ContentHash(composed), ContentHash(decomposed))
	}
}

func TestContentHashSensitiveToBody(t *testing.T) {
	t.Parallel()
	a := ContentHash([]byte("alpha"))
	b := ContentHash([]byte("beta"))
	if a == b {
		t.Fatalf("different bodies hashed equal: %s", a)
	}
}
