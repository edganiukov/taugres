//go:build unix

package state

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Lock acquires an exclusive lock for the project's state dir so that only one
// `tau sync` runs at a time; concurrent callers block until it is released. If
// the lock is already held, onWait (when non-nil) is called once before
// blocking, so callers can tell the user they are waiting. The returned function
// releases the lock.
func Lock(stateDir string, onWait func()) (func() error, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(filepath.Join(stateDir, "sync.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())

	// Try without blocking first so we can report that we are waiting.
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		if onWait != nil {
			onWait()
		}
		err = syscall.Flock(fd, syscall.LOCK_EX) // blocks until released
	}
	if err != nil {
		f.Close()
		return nil, err
	}

	return func() error {
		defer f.Close()
		return syscall.Flock(fd, syscall.LOCK_UN)
	}, nil
}
