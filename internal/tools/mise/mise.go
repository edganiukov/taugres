// Package mise integrates with the `mise` tool manager. During `tau sync` it
// installs declared tools and reports the directories holding their
// executables; the caller prepends those real store bin dirs to the activation
// PATH (the way `mise activate` works), so no symlink/wrapper farm is needed.
//
// This work is deliberately confined to `tau sync`: it may run mise and touch
// the network. Activation never calls mise.
package mise

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

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
// it live (the caller prefixes lines); a bounded tail is retained so quiet
// failures can surface diagnostics.
func install(ctx context.Context, refs []string, jobs int, force bool, out io.Writer) error {
	args := []string{"install"}
	if force {
		args = append(args, "--force") // reinstall even if the version is already present
	}
	if jobs > 0 {
		args = append(args, "--jobs", strconv.Itoa(jobs))
	}

	args = append(args, refs...)
	cmd := exec.Command(Binary, args...)
	return toolenv.Run(ctx, cmd, out, outputPrefix, "mise install "+strings.Join(refs, " "))
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

// ToolBinDir returns the directory holding a tool's executables — the same dir
// Install prepends to PATH — resolved via `mise where`. version is the concrete
// version to look up (a locked/resolved version, a pin, or "" for the active
// one). It runs mise, so it belongs to sync, never activation.
func ToolBinDir(name, version string) (string, error) {
	return ToolBinDirContext(context.Background(), name, version)
}

// ToolBinDirContext is ToolBinDir with cancellation for sync/exec resolution.
func ToolBinDirContext(ctx context.Context, name, version string) (string, error) {
	if !Available() {
		return "", fmt.Errorf("mise.where(%q) needs mise — install it with `curl https://mise.run | sh` (https://mise.jdx.dev)", name)
	}
	dir, err := where(ctx, model.MiseTool{Name: name, Version: version})
	if err != nil {
		return "", err
	}
	return binDir(dir, name), nil
}

// where returns the install directory for a tool via `mise where`.
func where(ctx context.Context, t model.MiseTool) (string, error) {
	out, err := exec.CommandContext(ctx, Binary, "where", ref(t)).Output()
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
// is streamed to it live. force passes `mise install --force`, reinstalling even
// tools already present in the store.
func Install(ctx context.Context, tools []model.MiseTool, jobs int, force bool, out io.Writer, report Reporter) ([]Installed, error) {
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
	if batchErr := install(ctx, refs, jobs, force, out); batchErr != nil {
		for _, t := range tools {
			_ = install(ctx, []string{ref(t)}, jobs, force, out)
		}
	}

	// Resolve whatever actually installed. `mise where` calls are independent,
	// so run them concurrently (bounded by the configured install parallelism)
	// while preserving declaration order in the result.
	resolved := make([]Installed, len(tools))
	found := make([]bool, len(tools))
	limit := jobs
	if limit <= 0 || limit > len(tools) {
		limit = len(tools)
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, tool := range tools {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			dir, err := where(ctx, tool)
			if err != nil {
				return
			}
			resolved[i] = Installed{
				Name:     tool.Name,
				Resolved: filepath.Base(dir), // mise stores installs as <tool>/<version>
				BinDir:   binDir(dir, tool.Name),
			}
			found[i] = true
		})
	}
	wg.Wait()

	var installed []Installed
	var missing []string
	for i, tool := range tools {
		if found[i] {
			installed = append(installed, resolved[i])
		} else {
			missing = append(missing, ref(tool))
		}
	}

	finish(len(missing) == 0)
	if len(missing) > 0 {
		return installed, fmt.Errorf("mise: could not install %s", strings.Join(missing, ", "))
	}

	return installed, nil
}
