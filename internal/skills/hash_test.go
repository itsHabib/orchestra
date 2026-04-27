package skills

import (
	"os"
	"path/filepath"
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
	decomposed := []byte("café")
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

func TestDirHashStableAcrossRuns(t *testing.T) {
	t.Parallel()
	root := writeSkillDir(t, map[string]string{
		"SKILL.md":        "# ship-feature\nbody\n",
		"helpers/foo.sh":  "#!/bin/sh\necho hi\n",
		"helpers/bar.txt": "literal\n",
	})
	first, err := DirHash(root)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	second, err := DirHash(root)
	if err != nil {
		t.Fatalf("hash second: %v", err)
	}
	if first != second {
		t.Fatalf("hash not stable: %s vs %s", first, second)
	}
}

func TestDirHashChangesOnContentChange(t *testing.T) {
	t.Parallel()
	root := writeSkillDir(t, map[string]string{
		"SKILL.md": "# v1\n",
	})
	before, _ := DirHash(root)
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("# v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	after, _ := DirHash(root)
	if before == after {
		t.Fatalf("hash unchanged after content change")
	}
}

func TestDirHashChangesOnFileAdded(t *testing.T) {
	t.Parallel()
	root := writeSkillDir(t, map[string]string{
		"SKILL.md": "# body\n",
	})
	before, _ := DirHash(root)
	if err := os.WriteFile(filepath.Join(root, "extra.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write extra: %v", err)
	}
	after, _ := DirHash(root)
	if before == after {
		t.Fatalf("hash unchanged after file added")
	}
}

func TestDirHashSkipsHiddenFiles(t *testing.T) {
	t.Parallel()
	rootA := writeSkillDir(t, map[string]string{
		"SKILL.md": "# body\n",
	})
	rootB := writeSkillDir(t, map[string]string{
		"SKILL.md":  "# body\n",
		".DS_Store": "junk\n",
		".git/HEAD": "ref\n",
	})
	a, _ := DirHash(rootA)
	b, _ := DirHash(rootB)
	if a != b {
		t.Fatalf("hidden files perturbed hash: %s vs %s", a, b)
	}
}

func TestDirHashRequiresSkillMD(t *testing.T) {
	t.Parallel()
	root := writeSkillDir(t, map[string]string{
		"NOTES.md": "no SKILL.md here\n",
	})
	if _, err := DirHash(root); err == nil {
		t.Fatalf("expected error when SKILL.md missing")
	}
}

func TestDirHashCRLFNormalized(t *testing.T) {
	t.Parallel()
	rootLF := writeSkillDir(t, map[string]string{
		"SKILL.md": "line\nline\n",
	})
	rootCRLF := writeSkillDir(t, map[string]string{
		"SKILL.md": "line\r\nline\r\n",
	})
	lf, _ := DirHash(rootLF)
	crlf, _ := DirHash(rootCRLF)
	if lf != crlf {
		t.Fatalf("CRLF not normalized in DirHash: %s vs %s", lf, crlf)
	}
}

func writeSkillDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}
