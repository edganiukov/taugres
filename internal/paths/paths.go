// Package paths implements Taugres root-anchored path resolution.
//
// Taugres config must never depend on the process working directory. Project
// paths are anchored at the repository root with a leading "//". Absolute
// filesystem paths are also allowed. Bare relative paths are invalid.
package paths

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ErrRelative is returned (wrapped) when a bare relative path is used.
type ErrRelative struct {
	Input string
}

func (e *ErrRelative) Error() string {
	return fmt.Sprintf(`path %q is not root-anchored; use "//%s" for repo-relative paths or an absolute path`,
		e.Input, strings.TrimLeft(e.Input, "./"))
}

// Resolve turns a Taugres path into an absolute filesystem path.
//
//   - "//foo/bar" -> <repoRoot>/foo/bar
//   - "/abs/path" -> "/abs/path" (unchanged, cleaned)
//   - anything else (e.g. "foo", "./foo", "../foo") is an error.
func Resolve(input, repoRoot string) (string, error) {
	if input == "" {
		return "", fmt.Errorf("empty path")
	}
	if rel, ok := strings.CutPrefix(input, "//"); ok {
		rel = strings.TrimLeft(rel, "/")
		return filepath.Join(repoRoot, filepath.FromSlash(rel)), nil
	}
	if filepath.IsAbs(input) {
		return filepath.Clean(input), nil
	}
	return "", &ErrRelative{Input: input}
}

// IsRootAnchored reports whether input is a "//"-anchored or absolute path,
// without resolving it.
func IsRootAnchored(input string) bool {
	return strings.HasPrefix(input, "//") || filepath.IsAbs(input)
}
