// Package testutil provides helpers for building temporary Taugres workspaces
// in tests.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TempWorkspace creates a temporary directory and returns its path. It is
// removed automatically at test end.
func TempWorkspace(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tau-test-*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Resolve symlinks (macOS /tmp) so path comparisons are stable.
	resolved, err := filepath.EvalSymlinks(dir)
	if err == nil {
		dir = resolved
	}
	return dir
}

// WriteFile writes content to a path relative to dir, creating parents.
func WriteFile(t testing.TB, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// WriteExec writes an executable file (e.g. a bin/ command).
func WriteExec(t testing.TB, dir, rel, content string) string {
	t.Helper()
	p := WriteFile(t, dir, rel, content)
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", p, err)
	}
	return p
}
