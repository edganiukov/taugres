// Package atomicfile writes files without exposing partially-written contents.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write replaces path atomically with data and mode. The temporary file is
// written in the destination directory so rename remains atomic.
func Write(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()

	if err = f.Chmod(mode); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("writing temporary file: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("syncing temporary file: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing destination: %w", err)
	}

	return nil
}
