// Package mise integrates with the `mise` tool manager. During `tau sync` it
// installs declared tools and reports the directories holding their
// executables; the caller prepends those real store bin dirs to the activation
// PATH (the way `mise activate` works), so no symlink/wrapper farm is needed.
//
// This work is deliberately confined to `tau sync`: it may run mise and touch
// the network. Activation never calls mise.
package mise

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/tools/toolenv"
)

// Binary is the mise executable name; overridable in tests.
var Binary = "mise"

// outputPrefix labels mise's own output so its origin is clear.
const outputPrefix = "mise: "

// Available reports whether the mise binary is on PATH.
func Available() bool {
	_, err := exec.LookPath(Binary)
	return err == nil
}

// ref returns the "name@version" reference (or just "name" when unversioned).
func ref(t model.MiseTool) string {
	if t.Version == "" {
		return t.Name
	}
	return t.Name + "@" + t.Version
}

// install runs `mise install ref...` for all tools in one command, so mise can
// resolve and download them in parallel. jobs caps that parallelism (--jobs);
// <= 0 leaves it to mise. When out is non-nil, mise's raw output is written to
// it live (the caller prefixes lines); it is also captured so a failure can
// surface the output in the error.
func install(refs []string, jobs int, out io.Writer) error {
	args := []string{"install"}
	if jobs > 0 {
		args = append(args, "--jobs", strconv.Itoa(jobs))
	}
	args = append(args, refs...)
	cmd := exec.Command(Binary, args...)
	return toolenv.Run(cmd, out, outputPrefix, "mise install "+strings.Join(refs, " "))
}

// binDir finds the directory holding a tool's executables within its install
// dir. Layouts vary by backend: most tools use <install>/bin, some put the
// binary at the root, and archive backends (ubi) may extract into a nested dir
// with no bin/ (e.g. uv -> <install>/uv-x86_64-unknown-linux-musl/uv). Keyed on
// the tool name so there is no per-tool special case.
func binDir(install, name string) string {
	if b := filepath.Join(install, "bin"); toolenv.IsDir(b) {
		return b
	}
	if toolenv.IsExecutable(filepath.Join(install, name)) {
		return install
	}
	// Look one level down for <sub>/<name> or <sub>/bin.
	entries, _ := os.ReadDir(install)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(install, e.Name())
		if toolenv.IsExecutable(filepath.Join(sub, name)) {
			return sub
		}
		if b := filepath.Join(sub, "bin"); toolenv.IsDir(b) {
			return b
		}
	}
	return install
}

// where returns the install directory for a tool via `mise where`.
func where(t model.MiseTool) (string, error) {
	out, err := exec.Command(Binary, "where", ref(t)).Output()
	if err != nil {
		return "", fmt.Errorf("mise where %s: %w", ref(t), err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("mise where %s: empty result", ref(t))
	}
	return dir, nil
}

// Reporter observes install steps. It is called with a tool reference before
// installation begins and returns a function to invoke when that step finishes
// (ok reports whether it succeeded). It may be nil.
type Reporter = toolenv.Reporter

// Installed describes a tool after installation.
type Installed struct {
	Name     string // tool name
	Resolved string // concrete resolved version (e.g. "22.11.0")
	BinDir   string // directory holding its executables (for PATH)
}

// Install installs the given tools (each MiseTool.Version is the exact spec to
// install — a locked concrete version, a partial pin, or "" for latest) in a
// single mise invocation so downloads run in parallel, then returns per tool
// the resolved concrete version and bin dir. When out is non-nil, mise's output
// is streamed to it live.
func Install(tools []model.MiseTool, jobs int, out io.Writer, report Reporter) ([]Installed, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	if !Available() {
		return nil, fmt.Errorf("mise.tool needs mise — install it with `curl https://mise.run | sh` (https://mise.jdx.dev)")
	}

	refs := make([]string, len(tools))
	for i, t := range tools {
		refs[i] = ref(t)
	}
	finish := func(bool) {}
	if report != nil {
		finish = report(strings.Join(refs, " "))
	}
	// Fast path: install everything in one invocation so downloads run in
	// parallel (capped at jobs). If that fails (e.g. one bad ref, or a
	// rate-limited backend), fall back to installing each tool on its own so a
	// single bad tool does not prevent the others — critically the pip/npm
	// toolchain — from landing.
	if batchErr := install(refs, jobs, out); batchErr != nil {
		for _, t := range tools {
			_ = install([]string{ref(t)}, jobs, out)
		}
	}

	// Resolve whatever actually installed; report the rest as failures without
	// discarding the successes.
	var installed []Installed
	var missing []string
	for _, t := range tools {
		dir, err := where(t)
		if err != nil {
			missing = append(missing, ref(t))
			continue
		}
		installed = append(installed, Installed{
			Name:     t.Name,
			Resolved: filepath.Base(dir), // mise stores installs as <tool>/<version>
			BinDir:   binDir(dir, t.Name),
		})
	}
	finish(len(missing) == 0)
	if len(missing) > 0 {
		return installed, fmt.Errorf("mise: could not install %s", strings.Join(missing, ", "))
	}
	return installed, nil
}
