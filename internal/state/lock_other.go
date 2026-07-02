//go:build !unix

package state

import "os"

// Lock is a best-effort no-op on platforms without flock. tau targets
// Linux/macOS; concurrent-sync protection is provided there.
func Lock(stateDir string, onWait func()) (func() error, error) {
	_ = os.MkdirAll(stateDir, 0o755)
	return func() error { return nil }, nil
}
