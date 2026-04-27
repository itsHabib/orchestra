package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ContentHash returns a stable sha256 hash of one skill file's bytes. The
// bytes are NFC-normalized and CRLF→LF converted before hashing so a skill
// checked out on different platforms produces the same hash.
func ContentHash(b []byte) string {
	normalized := normalizeBytes(b)
	sum := sha256.Sum256(normalized)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DirHash returns a stable sha256 hash of an entire skill directory. Files are
// walked in sorted order by forward-slash relative path; each contribution is
// `<rel>\0<normalized-bytes>\0` so a rename or a content change both perturb
// the hash. Hidden files (those whose any path segment starts with ".") are
// skipped — they tend to be editor or VCS artifacts that don't belong in an
// uploaded skill bundle. Non-regular files (symlinks, directories themselves)
// are skipped silently for the same reason.
//
// Returns an error if root is missing, unreadable, or empty (no files at all
// would produce an opaque empty-hash that's hard to debug). The caller is
// expected to ensure root contains a SKILL.md at its top before calling.
func DirHash(root string) (string, error) {
	files, err := walkSkillFiles(root)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("skills: hash %s: directory contains no files", root)
	}
	h := sha256.New()
	for i := range files {
		h.Write([]byte(files[i].RelPath))
		h.Write([]byte{0})
		h.Write(normalizeBytes(files[i].Content))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// SkillFile is one regular file in a skill directory, with its path expressed
// relative to the directory root (forward-slash separators) so callers can
// upload it under a stable name regardless of host OS.
type SkillFile struct {
	RelPath string
	Content []byte
}

// WalkSkillFiles returns every regular non-hidden file under root, sorted by
// RelPath. Used by both DirHash (for drift detection) and the registration
// service (to compose the multipart upload). Errors out if root is missing or
// not a directory; the caller can decide whether that's a "source missing"
// case or a configuration error.
func WalkSkillFiles(root string) ([]SkillFile, error) {
	return walkSkillFiles(root)
}

func walkSkillFiles(root string) ([]SkillFile, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skills: %s is not a directory", root)
	}

	var files []SkillFile
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, innerErr error) error {
		if innerErr != nil {
			return innerErr
		}
		return appendSkillFileIfRegular(path, d, root, &files)
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })

	if !hasSkillMD(files) {
		return nil, errors.New("skills: missing SKILL.md at the root of " + root)
	}
	return files, nil
}

// appendSkillFileIfRegular reads the file at path (relative to root) and
// appends it to files when it's a regular non-hidden file. Hidden directories
// short-circuit via fs.SkipDir; non-regular files (symlinks, sockets) and
// hidden files are silently skipped.
func appendSkillFileIfRegular(path string, d fs.DirEntry, root string, files *[]SkillFile) error {
	if path == root {
		return nil
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if hasHiddenSegment(rel) {
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	}
	if d.IsDir() || !d.Type().IsRegular() {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	*files = append(*files, SkillFile{RelPath: rel, Content: content})
	return nil
}

func hasHiddenSegment(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func hasSkillMD(files []SkillFile) bool {
	for i := range files {
		if files[i].RelPath == "SKILL.md" {
			return true
		}
	}
	return false
}

func normalizeBytes(b []byte) []byte {
	normalized := strings.ReplaceAll(norm.NFC.String(string(b)), "\r\n", "\n")
	return []byte(normalized)
}
