package fsutil

import "os"

// AtomicWrite writes data to a temp file then renames it to the target path.
// This prevents partial reads during concurrent access.
func AtomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
