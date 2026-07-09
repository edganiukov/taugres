// Package toolenv holds small helpers shared by tool-manager integrations
// (mise, pip, npm): a common progress Reporter type and filesystem checks.
//
// Tools are exposed on PATH by prepending their real install bin directories
// (the way `mise activate` works) — no symlink/wrapper farm — so binaries run
// from their true location and resolve their own files correctly.
package toolenv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edganiukov/taugres/internal/ui"
)

// interruptGrace is how long a child gets to exit after SIGINT before it is
// force-killed when the context is cancelled (Ctrl+C during sync).
const interruptGrace = 3 * time.Second

// Run executes cmd, streaming its combined output to out (when non-nil) while
// also capturing it. On failure it returns an error naming `what`; when out was
// nil (quiet mode) the captured output is included, prefixed, so it is not lost.
// Shared by the mise/pip/npm integrations.
//
// If ctx is cancelled (Ctrl+C), the child is sent SIGINT, then SIGKILL after a
// short grace period, and ctx.Err() is returned — so a long install stops
// promptly instead of running to completion.
func Run(ctx context.Context, cmd *exec.Cmd, out io.Writer, prefix, what string) error {
	var captured bytes.Buffer
	w := io.Writer(&captured)
	if out != nil {
		w = io.MultiWriter(&captured, out)
	}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(interruptGrace):
			_ = cmd.Process.Kill()
			<-done
		}
		return ctx.Err()
	case err := <-done:
		if err != nil {
			if out != nil {
				return fmt.Errorf("%s failed", what)
			}
			if msg := strings.TrimSpace(captured.String()); msg != "" {
				return fmt.Errorf("%s failed:\n%s", what, ui.PrefixLines(msg, prefix))
			}
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
}

// Reporter observes install steps. It is called with a package/tool reference
// before installation begins and returns a function to invoke when that step
// finishes (ok reports whether it succeeded). It may be nil.
type Reporter func(name string) func(ok bool)

// IsDir reports whether p is a directory (following symlinks).
func IsDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// IsExecutable resolves symlinks and reports whether the target is an
// executable regular file.
func IsExecutable(p string) bool {
	info, err := os.Stat(p) // follows symlinks
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// Resolve returns the first of names found as an executable in the given bin
// dirs (each dir is checked for every name in order). If none is found it
// returns names[0] as a bare fallback to be looked up on PATH. Shared by the
// pip/uv/npm integrations to locate their mise-provisioned interpreters.
func Resolve(bins []string, names ...string) string {
	for _, dir := range bins {
		for _, n := range names {
			if p := filepath.Join(dir, n); IsExecutable(p) {
				return p
			}
		}
	}
	return names[0]
}

// ScrapeVersion extracts the value of the first "Version:" line from `pip show`
// / `uv pip show` output, or "" if absent.
func ScrapeVersion(out []byte) string {
	for line := range strings.SplitSeq(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "Version:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
