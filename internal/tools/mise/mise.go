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
	"errors"
	"fmt"
	"io"
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

// cmdErr wraps a mise command failure, surfacing the stderr captured by
// Output() so failures are diagnosable (exec.ExitError alone only says
// "exit status N").
func cmdErr(what string, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return fmt.Errorf("%s: %w: %s", what, err, msg)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// binPaths asks mise for the exact bin dirs it puts on PATH for the tool
// (`mise bin-paths --silent <ref>`). This is authoritative — mise resolves it
// from backend metadata, matching `mise activate` — but it is not an installed
// check: for a ref it cannot resolve it prints nothing and still exits 0
// (--silent drops the warning noise). An empty, error-free result therefore
// means "unknown"; only `mise where` reliably reports missing tools.
func binPaths(ctx context.Context, t model.MiseTool) ([]string, error) {
	out, err := exec.CommandContext(ctx, Binary, "bin-paths", "--silent", ref(t)).Output()
	if err != nil {
		return nil, cmdErr("mise bin-paths "+ref(t), err)
	}

	var dirs []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			dirs = append(dirs, line)
		}
	}

	return dirs, nil
}

// ToolBinDir returns the directory holding a tool's executables — the same dir
// Install prepends to PATH — resolved via `mise where` + `mise bin-paths`.
// version is the concrete version to look up (a locked/resolved version, a
// pin, or "" for the active one). It runs mise, so it belongs to sync, never
// activation.
func ToolBinDir(name, version string) (string, error) {
	return ToolBinDirContext(context.Background(), name, version)
}

// ToolBinDirContext is ToolBinDir with cancellation for sync/exec resolution.
func ToolBinDirContext(ctx context.Context, name, version string) (string, error) {
	if !Available() {
		return "", fmt.Errorf("mise.where(%q) needs mise — install it with `curl https://mise.run | sh`", name)
	}

	ins, err := resolve(ctx, model.MiseTool{Name: name, Version: version})
	if err != nil {
		return "", err
	}

	return ins.BinDir, nil
}

// where returns the install directory for a tool via `mise where`.
func where(ctx context.Context, t model.MiseTool) (string, error) {
	out, err := exec.CommandContext(ctx, Binary, "where", ref(t)).Output()
	if err != nil {
		return "", cmdErr("mise where "+ref(t), err)
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

// resolve locates an installed tool using only mise itself: `mise where` is
// the authority on whether the tool is installed — unlike bin-paths it exits
// non-zero when missing — and yields the concrete version; `mise bin-paths`
// names the exact bin dir. bin-paths cannot always resolve a bare or partial
// ref (empty output, exit 0), so it is retried with the concrete version from
// where; if it still yields nothing, the install dir itself is used. Extra
// bin-paths dirs (rare, asdf plugins) are dropped — single-dir contract.
func resolve(ctx context.Context, t model.MiseTool) (Installed, error) {
	dir, err := where(ctx, t)
	if err != nil {
		return Installed{}, err
	}
	version := filepath.Base(dir) // mise stores installs as <tool>/<version>

	paths, err := binPaths(ctx, t)
	if err != nil {
		return Installed{}, err
	}
	if len(paths) == 0 && t.Version != version {
		if paths, err = binPaths(ctx, model.MiseTool{Name: t.Name, Version: version}); err != nil {
			return Installed{}, err
		}
	}

	bin := dir
	if len(paths) > 0 {
		bin = paths[0]
	}

	return Installed{Name: t.Name, Resolved: version, BinDir: bin}, nil
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
		return nil, fmt.Errorf("mise.tool needs mise — install it with `curl https://mise.run | sh`")
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

	// Resolve whatever actually installed. The per-tool resolutions are
	// independent, so run them concurrently (bounded by the configured install
	// parallelism) while preserving declaration order in the result.
	resolved := make([]Installed, len(tools))
	errs := make([]error, len(tools))
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
			resolved[i], errs[i] = resolve(ctx, tool)
		})
	}
	wg.Wait()

	var installed []Installed
	var missing []string
	var failures []error
	for i, tool := range tools {
		if errs[i] == nil {
			installed = append(installed, resolved[i])
		} else {
			missing = append(missing, ref(tool))
			failures = append(failures, errs[i])
		}
	}

	finish(len(missing) == 0)
	if len(missing) > 0 {
		return installed, fmt.Errorf("mise: could not install %s: %w", strings.Join(missing, ", "), errors.Join(failures...))
	}

	return installed, nil
}
