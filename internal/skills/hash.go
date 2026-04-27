package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ContentHash returns a stable sha256 hash of the skill content. The bytes are
// NFC-normalized and CRLF→LF converted before hashing so a skill checked out
// on different platforms produces the same hash.
func ContentHash(b []byte) string {
	normalized := strings.ReplaceAll(norm.NFC.String(string(b)), "\r\n", "\n")
	sum := sha256.Sum256([]byte(normalized))
	return "sha256:" + hex.EncodeToString(sum[:])
}
