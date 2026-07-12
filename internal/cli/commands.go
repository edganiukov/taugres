package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edganiukov/taugres/internal/atomicfile"
	"github.com/edganiukov/taugres/internal/config"
	"github.com/edganiukov/taugres/internal/discover"
	"github.com/edganiukov/taugres/internal/environment"
	"github.com/edganiukov/taugres/internal/lock"
	"github.com/edganiukov/taugres/internal/model"
	shellreg "github.com/edganiukov/taugres/internal/shell"
	"github.com/edganiukov/taugres/internal/shellhook"
	"github.com/edganiukov/taugres/internal/state"
	"github.com/edganiukov/taugres/internal/toolmgr"
	"github.com/edganiukov/taugres/internal/tools/mise"
	"github.com/edganiukov/taugres/internal/trust"
	"github.com/edganiukov/taugres/internal/ui"
	"github.com/edganiukov/taugres/internal/validate"
)

// --- init ---

const workspaceTemplate = `# Taugres workspace config. Language: Starlark + Taugres API.
project("%s")

# Environment variables.
# shell.env("FOO", "BAR")

# Tools/packages install via mise/pip/npm and are added to PATH automatically
# on activation (their real bin dirs are prepended, like mise activate).
# Each takes a name, a "name@version" spec, or a list of specs.
# mise.tool(["node@22.11.0", "ripgrep"])   # also mise backends: go:/cargo:/npm:/ubi:/aqua:

# pip/uv/npm run on a mise-provided python/node (added implicitly at latest).
# To pin that runtime, declare it: mise.tool("python@3.12.7"), mise.tool("node@22.11.0").
# pip.install(["ruff@0.6.9", "rich"])      # or uv.install([...]) (faster)
# npm.install("typescript")

# Paths are repository-root anchored with //.
# shell.path.prepend("//node_modules/.bin")

# Aliases.
# shell.alias("ll", "ls -lh")

# Sourced shell functions. The body can live in a file...
# shell.fn("croot", shells = ["bash", "zsh"], file = "//bin/croot.sh")
# ...or be given inline:
# shell.fn("hi", shells = ["bash", "zsh"], content = "echo hello $1")

# Raw setup run at activation (like flake.nix's shellHook):
# shell.hook(shells = ["bash", "zsh"], content = "mkdir -p .cache")
`

const projectTemplate = `# Taugres nested project config.
project("%s")

# See the workspace.tg at the repo root for shared setup, or load a helper:
# load("//taugres/lib/common.tg", "common")
`

func runInit(e *Env, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	nested := fs.Bool("nested", false, "create a nested project.tg instead of workspace.tg")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fileName := discover.WorkspaceFile
	tmpl := workspaceTemplate
	if *nested {
		fileName = discover.ProjectFile
		tmpl = projectTemplate
	}

	target := filepath.Join(e.Wd, fileName)
	if _, err := os.Stat(target); err == nil {
		return fail(e, "%s already exists at %s", fileName, target)
	}

	// Guard the both-files invariant.
	other := discover.ProjectFile
	if *nested {
		other = discover.WorkspaceFile
	}
	if _, err := os.Stat(filepath.Join(e.Wd, other)); err == nil {
		return fail(e, "directory already contains %s; a directory may contain only one config file", other)
	}

	name := filepath.Base(e.Wd)
	content := fmt.Sprintf(tmpl, name)
	if err := atomicfile.Write(target, []byte(content), 0o644); err != nil {
		return fail(e, "writing %s: %v", target, err)
	}
	fmt.Fprintf(e.Stdout, "tau: created %s\n", target)

	fmt.Fprintf(e.Stdout, "tau: review the config, then run `tau allow` and `tau sync`\n")
	return 0
}

// --- shared discovery+eval helper ---

type evalResult struct {
	disc   *discover.Discovery
	res    *config.Result
	report *validate.Report
}

func discoverAndEval(e *Env) (*evalResult, int) {
	d, err := discover.Discover(e.Wd)
	if err != nil {
		return nil, fail(e, "%v", err)
	}
	res, err := config.Evaluate(d)
	if err != nil {
		return nil, fail(e, "evaluating %s:\n%v", d.ConfigPath, err)
	}
	report := validate.Validate(res.Plan)
	return &evalResult{disc: d, res: res, report: report}, 0
}

// --- check ---

func runCheck(e *Env, args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	er, code := discoverAndEval(e)
	if er == nil {
		return code
	}

	fmt.Fprintf(e.Stdout, "config:  %s\n", er.disc.ConfigPath)
	fmt.Fprintf(e.Stdout, "repo:    %s\n", er.disc.RepoRoot)
	fmt.Fprintf(e.Stdout, "project: %s\n", er.disc.ProjectRoot)

	for _, warn := range er.report.Warnings {
		fmt.Fprintf(e.Stdout, "tau: warning: %s\n", warn)
	}
	for _, errMsg := range er.report.Errors {
		fmt.Fprintf(e.Stderr, "tau: error: %s\n", errMsg)
	}

	if er.report.HasErrors() {
		return 1
	}
	fmt.Fprintln(e.Stdout, "\nOK")
	return 0
}

// --- update ---

// updManagers are the tool-manager qualifiers accepted as a "<manager>:name"
// prefix by `tau update`, in lock-section order.
var updManagers = slices.Clone(toolmgr.All)

// splitManager splits an optional "<manager>:" qualifier off an update
// argument. Only mise/pip/npm/uv count as qualifiers; any other leading segment
// is left intact, so a mise backend spec like "go:goose" is treated as a bare
// name (and a backend-prefixed mise tool can still be qualified explicitly, as
// "mise:go:goose").
func splitManager(arg string) (manager, name string) {
	if i := strings.IndexByte(arg, ':'); i > 0 && slices.Contains(updManagers, arg[:i]) {
		return arg[:i], arg[i+1:]
	}
	return "", arg
}

// sectionOf returns the lock section for a manager qualifier.
func sectionOf(lk *lock.File, manager string) map[string]lock.Entry {
	return toolmgr.Section(lk, manager)
}

// updTarget locates a named tool/package: the manager it belongs to and whether
// the config pins its version (in which case updating is a no-op that should be
// steered to editing the config instead).
type updTarget struct {
	manager string
	pinned  bool
}

// targets returns the managers a name is declared under. With manager == "" it
// searches all; otherwise it is restricted to that one. A name may match more
// than one manager (e.g. the same package under both pip and uv).
func targets(p *model.Plan, manager, name string) []updTarget {
	var out []updTarget
	want := func(m string) bool { return manager == "" || manager == m }
	if want("mise") {
		for _, t := range p.MiseTools {
			if t.Name == name {
				out = append(out, updTarget{"mise", t.Version != ""})
				break
			}
		}
	}
	for _, descriptor := range toolmgr.PackageManagers {
		if !want(descriptor.ID) {
			continue
		}
		for _, pkg := range descriptor.Packages(p) {
			if pkg.Name == name {
				out = append(out, updTarget{descriptor.ID, pkg.Version != ""})
				break
			}
		}
	}
	return out
}

// runUpdate re-resolves specific unpinned tools/packages to their latest
// versions, leaving everything else at its locked version. It works by dropping
// the named entries from the lock and running a normal sync, which re-resolves
// only the now-missing entries. Names may be qualified as "<manager>:name"
// (mise/pip/npm/uv) to disambiguate a package declared under two managers. With
// no names it updates everything unpinned (equivalent to `tau sync --update`).
func runUpdate(e *Env, args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	verbose := fs.Bool("verbose", false, "print every step and tool output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	names := fs.Args()

	// No names: update everything unpinned.
	if len(names) == 0 {
		return syncProject(e, syncOptions{verbose: *verbose, update: true})
	}

	er, code := discoverAndEval(e)
	if er == nil {
		return code
	}
	lk, err := lock.Load(er.disc.ProjectRoot)
	if err != nil {
		return fail(e, "reading %s: %v", lock.FileName, err)
	}

	type cleared struct{ manager, name, old string }
	var updated []cleared
	for _, arg := range names {
		manager, name := splitManager(arg)
		ts := targets(er.res.Plan, manager, name)
		if len(ts) == 0 {
			if manager != "" {
				return fail(e, "update %s: %s is not a %s-managed tool or package", arg, name, manager)
			}
			return fail(e, "update %s: not a declared tool or package", arg)
		}
		for _, t := range ts {
			if t.pinned {
				fmt.Fprintf(e.Stdout, "tau: %s (%s) is pinned in the config — change its version there\n", name, t.manager)
				continue
			}
			sec := sectionOf(lk, t.manager)
			old := ""
			if entry, ok := sec[name]; ok {
				old = entry.Resolved
			}
			updated = append(updated, cleared{t.manager, name, old})
		}
	}
	if len(updated) == 0 {
		fmt.Fprintln(e.Stdout, "tau: nothing to update")
		return 0
	}
	labels := make([]string, len(updated))
	for i, u := range updated {
		labels[i] = u.name
	}
	fmt.Fprintf(e.Stdout, "tau: updating %s\n", strings.Join(labels, ", "))
	updateTargets := make([]string, 0, len(updated))
	for _, u := range updated {
		updateTargets = append(updateTargets, u.manager+":"+u.name)
	}
	if code := syncProject(e, syncOptions{verbose: *verbose, updateTarget: updateTargets}); code != 0 {
		return code
	}

	// Report old -> new from the freshly-written lock, per manager.
	if nlk, err := lock.Load(er.disc.ProjectRoot); err == nil {
		for _, u := range updated {
			now := sectionOf(nlk, u.manager)[u.name].Resolved
			if u.old != now {
				fmt.Fprintf(e.Stdout, "tau: %s (%s) %s -> %s\n", u.name, u.manager, orNone(u.old), orNone(now))
			}
		}
	}
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// --- exec ---

// runExec runs a command with the project's environment applied — env vars and a
// PATH that includes the provisioned tool bin dirs — without going through a
// shell hook. It is the shell-agnostic slice of an activation, for editors, CI,
// Makefiles, and one-off invocations; shell-only features (aliases, functions,
// shell.hook) are deliberately not applied.
//
// It is trust-gated like activation: env vars from an untrusted config could
// subvert the command (PATH, LD_PRELOAD, …), so an untrusted project is refused.
// It auto-syncs when stale so freshly-declared tools are present, then execs the
// command, propagating its exit code.
func runExec(e *Env, args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return fail(e, "usage: tau exec [--] <command> [args...]")
	}

	d, err := discover.Discover(e.Wd)
	if err != nil {
		return fail(e, "%v", err)
	}

	allowed, err := trust.IsAllowed(d.ConfigPath)
	if err != nil {
		return fail(e, "checking trust: %v", err)
	}
	if !allowed {
		return fail(e, "project is not trusted; run `tau allow`")
	}

	// Bring tools/scripts up to date so PATH reflects freshly-provisioned tool
	// bin dirs. Installs are best-effort (like the hook's auto-sync), so ignore
	// the result and run the command with whatever env was generated; sync output
	// goes to stderr to keep our stdout for the command.
	stateDir := filepath.Join(d.ProjectRoot, ".taugres")
	if need, nerr := state.NeedsSync(stateDir, d.ConfigPath); nerr == nil && need {
		code := syncProject(&Env{Stdout: e.Stderr, Stderr: e.Stderr, Wd: e.Wd, Ctx: e.ctx()}, syncOptions{ifStale: true})
		if code == 130 {
			return code
		}
	}

	res, err := config.Evaluate(d)
	if err != nil {
		return fail(e, "evaluating %s:\n%v", d.ConfigPath, err)
	}
	plan := res.Plan

	// Recover the mise store bin dirs from the manifest (resolved at sync); the
	// pip/uv/npm bin dirs are already deterministic in plan.PathPrepend.
	var miseBinDirs []string
	if m, lerr := state.Load(stateDir); lerr == nil {
		miseBinDirs = miseBinDirsFrom(m)
	}

	envMap := environment.Build(plan, miseBinDirs)

	// Resolve every deferred env var (shell.exec / mise.where) into the child env.
	// exec has no shell, so both static and dynamic segments run now, against the
	// assembled env (later entries see earlier ones). Failures are reported but
	// don't block the command.
	if len(plan.DeferredEnv) > 0 {
		lk, _ := lock.Load(d.ProjectRoot)
		for _, de := range plan.DeferredEnv {
			val, derr := resolveDeferred(e.ctx(), de.Segments, lk, plan, environment.Flatten(envMap))
			if derr != nil {
				fmt.Fprintf(e.Stderr, "tau: shell.env %s: %v\n", de.Name, derr)
				continue
			}
			envMap[de.Name] = val
		}
	}
	env := environment.Flatten(envMap)

	bin, err := lookPathIn(rest[0], env)
	if err != nil {
		return fail(e, "%v", err)
	}

	cmd := exec.Command(bin, rest[1:]...)
	cmd.Env = env
	cmd.Dir = e.Wd
	cmd.Stdin = e.Stdin
	cmd.Stdout = e.Stdout
	cmd.Stderr = e.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return ee.ExitCode()
		}
		return fail(e, "exec %s: %v", rest[0], err)
	}
	return 0
}

// resolveDeferredEnvForSync resolves each plan.DeferredEnv at sync time. A fully-static value (no dynamic exec) is
// joined and written to EnvSet, so it activates instantly; a value with a dynamic exec has its static segments baked to
// literals in place and is left for the renderer to emit as `$(cmd)`. Failures are reported via onErr (best-effort),
// never fatal. Each entry runs against the current env (including EnvSet updated by earlier static entries).
func resolveDeferredEnvForSync(ctx context.Context, plan *model.Plan, lk *lock.File, onErr func(string)) {
	for i := range plan.DeferredEnv {
		de := &plan.DeferredEnv[i]
		if de.IsDynamic() {
			// Bake the static segments (mise.where, static exec) to literals; leave
			// dynamic exec segments for the renderer.
			env := environment.Flatten(environment.Build(plan, nil))
			for j := range de.Segments {
				s := &de.Segments[j]
				if s.Kind == model.SegExec && s.Dynamic {
					continue
				}
				val, err := resolveSegment(ctx, *s, lk, plan, env)
				if err != nil {
					onErr("shell.env " + de.Name + ": " + err.Error())
					// Never pass an unresolved static exec/where segment to the renderer: it would otherwise be
					// mistaken for activation-time code or literal tool text.
					*s = model.Segment{Kind: model.SegLiteral}
					continue
				}
				*s = model.Segment{Kind: model.SegLiteral, Value: val}
			}
			continue
		}
		env := environment.Flatten(environment.Build(plan, nil))
		val, err := resolveDeferred(ctx, de.Segments, lk, plan, env)
		if err != nil {
			onErr("shell.env " + de.Name + ": " + err.Error())
			continue
		}
		plan.EnvSet[de.Name] = val
	}
}

// resolveDeferred resolves every segment (including dynamic exec) and joins them.  Used by `tau exec`, which has no
// shell to defer to.
func resolveDeferred(ctx context.Context, segs []model.Segment, lk *lock.File, plan *model.Plan, env []string) (string, error) {
	var b strings.Builder
	for _, s := range segs {
		val, err := resolveSegment(ctx, s, lk, plan, env)
		if err != nil {
			return "", err
		}
		b.WriteString(val)
	}
	return b.String(), nil
}

// resolveSegment resolves a single segment to its string value: a literal as-is, a mise.where to the tool's bin dir
// (looked up via the locked version so it matches PATH), or an exec to its command's trimmed stdout.
func resolveSegment(ctx context.Context, s model.Segment, lk *lock.File, plan *model.Plan, env []string) (string, error) {
	switch s.Kind {
	case model.SegLiteral:
		return s.Value, nil
	case model.SegWhere:
		return mise.ToolBinDirContext(ctx, s.Value, miseVersion(s.Value, lk, plan))
	case model.SegExec:
		return captureCommand(ctx, s.Shell, s.Value, plan.ProjectRoot, env)
	default:
		return "", fmt.Errorf("unknown segment kind %q", s.Kind)
	}
}

// miseVersion returns the concrete version to look up for a mise tool: the locked resolved version if present, else the
// version declared in the config.
func miseVersion(tool string, lk *lock.File, plan *model.Plan) string {
	if lk != nil {
		if v := lk.Mise[tool].Resolved; v != "" {
			return v
		}
	}
	for _, t := range plan.MiseTools {
		if t.Name == tool {
			return t.Version
		}
	}
	return ""
}

// resolveShell picks the interpreter for a shell.exec command: the explicit shell if given, else the local login shell
// ($SHELL), else sh.
func resolveShell(shell string) string {
	if shell != "" {
		return shell
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "sh"
}

// captureCommand runs `<shell> -c command` in dir with env and returns its stdout with trailing newlines trimmed (like
// shell command substitution). shell is the interpreter name ("" resolves to the local $SHELL, else sh). Backs
// shell.exec.
func captureCommand(ctx context.Context, shell, command, dir string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, resolveShell(shell), "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// lookPathIn resolves cmd against the PATH carried in env (not the parent process's PATH), so `tau exec` finds tools
// from the project environment. A cmd containing a path separator is returned as-is.
func lookPathIn(cmd string, env []string) (string, error) {
	if strings.ContainsRune(cmd, os.PathSeparator) {
		return cmd, nil
	}
	var path string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, cmd)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("exec: %q not found in the project PATH", cmd)
}

// --- status ---

func runStatus(e *Env, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	er, code := discoverAndEval(e)
	if er == nil {
		return code
	}
	plan := er.res.Plan

	fmt.Fprintf(e.Stdout, "config:  %s\n", plan.ConfigPath)
	fmt.Fprintf(e.Stdout, "repo:    %s\n", plan.RepoRoot)
	fmt.Fprintf(e.Stdout, "project: %s\n", plan.ProjectRoot)

	stale := state.CheckStale(plan.StateDir, shellreg.Supported)
	if _, err := state.Load(plan.StateDir); err != nil {
		fmt.Fprintf(e.Stdout, "synced:  no (run `tau sync`)\n")
	} else if stale.Stale {
		fmt.Fprintf(e.Stdout, "synced:  stale — %s\n", stale.Reason)
	} else {
		fmt.Fprintf(e.Stdout, "synced:  yes\n")
	}

	// Trust status.
	if allowed, err := trust.IsAllowed(plan.ConfigPath); err == nil {
		if allowed {
			fmt.Fprintf(e.Stdout, "trust:   trusted\n")
		} else {
			fmt.Fprintf(e.Stdout, "trust:   untrusted (run `tau allow`)\n")
		}
	}

	if len(plan.MiseTools) > 0 {
		fmt.Fprintf(e.Stdout, "\nmise tools:\n")
		for _, t := range plan.MiseTools {
			ver := t.Version
			if ver == "" {
				ver = "latest"
			}
			fmt.Fprintf(e.Stdout, "  %s@%s\n", t.Name, ver)
		}
	}

	for _, manager := range toolmgr.PackageManagers {
		packages := manager.Packages(plan)
		if len(packages) == 0 {
			continue
		}
		fmt.Fprintf(e.Stdout, "\n%s packages:\n", manager.ID)
		for _, pkg := range packages {
			fmt.Fprintf(e.Stdout, "  %s\n", manager.Display(pkg))
		}
	}

	for _, warn := range er.report.Warnings {
		fmt.Fprintf(e.Stdout, "tau: warning: %s\n", warn)
	}
	return 0
}

// --- hook ---

func runHook(e *Env, args []string) int {
	if len(args) != 1 {
		return fail(e, "usage: tau hook <shell> (bash|zsh|fish)")
	}
	// Bake in the absolute path to this tau binary so the hook invokes exactly this executable via `tau hook-env`.
	tauBin := "tau"
	if p, err := os.Executable(); err == nil {
		tauBin = p
	}
	script, err := shellhook.Hook(args[0], tauBin)
	if err != nil {
		return fail(e, "%v", err)
	}
	fmt.Fprint(e.Stdout, script)
	return 0
}

// --- setup ---

// runSetup installs the tau shell hook into the user's shell startup file (~/.bashrc, ~/.zshrc, or
// ~/.config/fish/config.fish). The shell defaults to the current one ($SHELL) and can be overridden, e.g. `tau setup
// bash`.
func runSetup(e *Env, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	assumeYes := fs.Bool("yes", false, "assume yes to prompts (install mise non-interactively if it is missing)")
	fs.BoolVar(assumeYes, "y", false, "shorthand for --yes")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rest := fs.Args()
	var shell string
	switch len(rest) {
	case 0:
		sh := os.Getenv("SHELL")
		if sh == "" {
			return fail(e, "$SHELL is not set; pass a shell: tau setup <shell> (bash|zsh|fish)")
		}
		shell = filepath.Base(sh)
	case 1:
		shell = rest[0]
	default:
		return fail(e, "usage: tau setup [shell] (bash|zsh|fish)")
	}

	if !shellreg.IsSupported(shell) {
		return fail(e, "unsupported shell %q (supported: %s)", shell, strings.Join(shellreg.Supported, ", "))
	}

	rc, err := shellRCPath(shell)
	if err != nil {
		return fail(e, "%v", err)
	}

	existing, _ := os.ReadFile(rc)
	// Idempotent: skip the hook append if the hook is already present — whether a previous `tau setup` added it or the
	// user installed it by hand from the README. Detect the `tau hook <shell>` invocation itself (shared by every form:
	// `eval "$(tau hook bash)"` and `tau hook fish | source`).
	if strings.Contains(string(existing), "tau hook "+shell) {
		fmt.Fprintf(e.Stdout, "tau: shell hook already installed in %s\n", rc)
	} else {
		if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil {
			return fail(e, "creating %s: %v", filepath.Dir(rc), err)
		}

		// Append in place rather than atomically replacing the file: a startup file is often a symlink managed by
		// a dotfiles tool (chezmoi, stow, bare repo), and a temp-file rename would detach it from its source and reset
		// its mode.  Appending preserves the inode, symlink target, and permissions.
		var b strings.Builder
		if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
			b.WriteByte('\n')
		}

		b.WriteByte('\n')
		b.WriteString("# taugres shell integration (added by `tau setup`)\n")
		b.WriteString(hookInstallLine(shell))
		b.WriteString("\n")

		f, err := os.OpenFile(rc, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fail(e, "updating %s: %v", rc, err)
		}

		if _, err := f.WriteString(b.String()); err != nil {
			f.Close()
			return fail(e, "updating %s: %v", rc, err)
		}

		if err := f.Close(); err != nil {
			return fail(e, "updating %s: %v", rc, err)
		}

		fmt.Fprintf(e.Stdout, "tau: installed the %s hook in %s\n", shell, rc)
		fmt.Fprintf(e.Stdout, "tau: restart your shell or run: source %s\n", rc)
	}

	// The hook activates environments; mise provisions the tools those environments put on PATH. Offer to install it
	// when it is missing so a fresh machine is ready in one command. This never fails setup — the hook is already in
	// place — so any problem is reported and shrugged off.
	if !mise.Available() {
		maybeInstallMise(e, *assumeYes)
	}
	return 0
}

// miseInstallURL is mise's official installer endpoint. tau fetches it directly (rather than shelling out to curl) so
// the installer works without curl/wget, network failures surface as typed errors, and the whole script is downloaded
// before it runs — a dropped connection can't execute a truncated installer.
const miseInstallURL = "https://mise.run"

// miseInstallCommand is the equivalent one-liner shown as a manual fallback when tau declines to, or cannot, install
// mise itself.
const miseInstallCommand = "curl " + miseInstallURL + " | sh"

// miseFetchTimeout bounds only the installer download, not the install itself (which downloads the mise binary and can
// legitimately take a while).
const miseFetchTimeout = 30 * time.Second

// installMise downloads mise's installer from miseInstallURL and runs it with sh, streaming output to out. The script
// is read fully into memory first, then fed to sh via stdin — the same semantics as `curl | sh` minus the pipe, so
// a partial download never executes. It is a variable so tests can stub it instead of hitting the network.
var installMise = func(ctx context.Context, out io.Writer) error {
	fetchCtx, cancel := context.WithTimeout(ctx, miseFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, miseInstallURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading mise installer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading mise installer: %s", resp.Status)
	}
	// Cap the read so a misbehaving endpoint can't stream unbounded data into
	// memory; the real installer is a few KB.
	script, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("downloading mise installer: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh")
	cmd.Stdin = bytes.NewReader(script)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// maybeInstallMise offers to install mise when it is missing from PATH. mise provisions the tools tau declares, so
// a machine without it can activate environments but not install anything. With assumeYes it installs without prompting
// (CI/scripts); otherwise it names the installer source and installs only on an explicit yes. It is best-effort: every
// failure is reported with the manual command and returns without aborting setup.
func maybeInstallMise(e *Env, assumeYes bool) {
	ctx := e.ctx()
	fmt.Fprintln(e.Stdout, "tau: mise is not on PATH — it's needed to install tools.")
	if !assumeYes {
		prompt := "\tinstall it now? tau will download and run the installer from: " + miseInstallURL
		if !ui.Confirm(ctx, e.Stdout, e.Stdin, prompt) {
			if ctx.Err() != nil { // Ctrl+C at the prompt
				fmt.Fprintln(e.Stdout)
				fmt.Fprintln(e.Stderr, "tau: setup cancelled")
				return
			}
			fmt.Fprintf(e.Stdout, "tau: skipped; install mise later with:\n\t%s\n", miseInstallCommand)
			return
		}
	}
	if err := installMise(ctx, e.Stderr); err != nil {
		if ctx.Err() != nil { // Ctrl+C during download/install
			fmt.Fprintln(e.Stderr, "tau: setup cancelled")
			return
		}
		fmt.Fprintf(e.Stderr, "tau: installing mise failed: %v\n\tinstall it manually: %s\n", err, miseInstallCommand)
		return
	}
	// mise.run installs to ~/.local/bin, which may not be on PATH in this or the
	// next shell. Re-probe so we tell the user whether it's actually usable yet.
	if mise.Available() {
		fmt.Fprintln(e.Stdout, "tau: mise installed.")
	} else {
		fmt.Fprintln(e.Stdout, "tau: mise installed, but it is not on your PATH yet —")
		fmt.Fprintln(e.Stdout, "\tyour shell, or add mise's bin dir (usually ~/.local/bin) to PATH.")
	}
}

// hookInstallLine is the line that activates the hook at shell startup.
func hookInstallLine(shell string) string {
	if shell == shellreg.Fish {
		return "tau hook fish | source"
	}

	return fmt.Sprintf("eval \"$(tau hook %s)\"", shell)
}

// shellRCPath returns the startup file `tau setup` appends the hook to, honoring ZDOTDIR (zsh) and XDG_CONFIG_HOME
// (fish).
func shellRCPath(shell string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch shell {
	case shellreg.Bash:
		return filepath.Join(home, ".bashrc"), nil
	case shellreg.Zsh:
		dir := os.Getenv("ZDOTDIR")
		if dir == "" {
			dir = home
		}
		return filepath.Join(dir, ".zshrc"), nil
	case shellreg.Fish:
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		return filepath.Join(cfg, "fish", "config.fish"), nil
	}
	return "", fmt.Errorf("no known startup file for shell %q", shell)
}

// --- activate / deactivate ---

func runActivate(e *Env, args []string) int   { return emitGenScript(e, args, "activate") }
func runDeactivate(e *Env, args []string) int { return emitGenScript(e, args, "deactivate") }

// emitGenScript prints the generated activate/deactivate script for the current project to stdout, so a user can `eval`
// it. It does no staleness check: `tau status` reports staleness.
//
// Both kinds are trust-gated: the caller sources this stdout, so tau must only emit a script it generated itself during
// a trusted sync — never repo bytes.  A cloned untrusted repo can commit its own .taugres/gen/deactivate.<shell>, and
// even a teardown script runs arbitrary shell, so refusing an untrusted project is the security boundary (trust lives
// outside the repo, so a clone can't forge it). The hook's own auto-teardown does not go through here; it reads the
// deactivate script directly for a project it activated while trusted.
func emitGenScript(e *Env, args []string, kind string) int {
	var shell string
	switch len(args) {
	case 0:
		// Default to the current shell via $SHELL. The hook passes the shell
		// explicitly; this default is for manual `eval "$(tau activate)"`.
		sh := os.Getenv("SHELL")
		if sh == "" {
			return fail(e, "$SHELL is not set; pass a shell: tau %s <shell> (bash|zsh|fish)", kind)
		}
		shell = filepath.Base(sh)
	case 1:
		shell = args[0]
	default:
		return fail(e, "usage: tau %s [shell] (bash|zsh|fish)", kind)
	}
	if !shellreg.IsSupported(shell) {
		return fail(e, "unsupported shell %q (supported: %s)", shell, strings.Join(shellreg.Supported, ", "))
	}
	d, err := discover.Discover(e.Wd)
	if err != nil {
		return fail(e, "%v", err)
	}

	allowed, err := trust.IsAllowed(d.ConfigPath)
	if err != nil {
		return fail(e, "checking trust: %v", err)
	}
	if !allowed {
		return fail(e, "project is not trusted; run `tau allow`")
	}

	stateDir := filepath.Join(d.ProjectRoot, ".taugres")
	script := filepath.Join(state.GenDir(stateDir), kind+"."+shell)
	data, err := os.ReadFile(script)
	if err != nil {
		return fail(e, "no generated %s script for %s; run `tau sync`", kind, shell)
	}
	fmt.Fprint(e.Stdout, string(data))
	return 0
}

// --- hook-env ---

// hookToken is the per-shell session state runHookEnv round-trips through the TAUGRES_HOOK env var, so the shell holds
// no state machine and tau writes no state files. Its string form is "<applied>|<stamp>|<fp>|<proj>":
//
//	applied  1 when this shell has proj's activate script sourced, else 0
//	stamp    proj's activate-script mtime in ns ("" when there is no script)
//	fp       retry fingerprint ("" once synced; non-empty after a failed sync)
//	proj     project root (may contain '|', so it is the trailing field)
//
// It is exported so `tau hook-env` (a subprocess) can read it — which means a child shell inherits it, while the
// aliases and functions the activate script defined do not survive the fork. The shim therefore keeps an UNEXPORTED
// _TAU_APPLIED flag alongside (set by the same eval'd output) and passes it back as an argument: an inherited token
// whose shell lacks the flag has its applied claim reconciled to false, so the child re-activates. An empty or foreign
// token value parses as "nothing recorded" and is treated as a clean reset.
type hookToken struct {
	applied bool
	stamp   string
	fp      string
	proj    string
}

// parseHookToken parses a token; ok is false for an empty or non-conforming
// value (e.g. one written by a different tau version), signalling a clean reset.
func parseHookToken(s string) (t hookToken, ok bool) {
	applied, rest, ok := strings.Cut(s, "|")
	if !ok || (applied != "0" && applied != "1") {
		return hookToken{}, false
	}
	stamp, rest, ok := strings.Cut(rest, "|")
	if !ok {
		return hookToken{}, false
	}
	fp, proj, ok := strings.Cut(rest, "|")
	if !ok {
		return hookToken{}, false
	}
	return hookToken{applied: applied == "1", stamp: stamp, fp: fp, proj: proj}, true
}

func (t hookToken) String() string {
	applied := "0"
	if t.applied {
		applied = "1"
	}
	return applied + "|" + t.stamp + "|" + t.fp + "|" + t.proj
}

// runHookEnv is the hook backend: the shell shim evals this command's stdout on every in-project prompt, and ALL hook
// logic — staleness, the retry guard, auto-sync, trust, activation/deactivation — lives here in Go. It computes the
// desired state first and emits at most one transition (tear down the applied project, then activate the target), so
// the sourced env can never be left torn down or doubly applied.
func runHookEnv(e *Env, args []string) int {
	if len(args) < 1 || len(args) > 3 {
		return fail(e, "usage: tau hook-env <shell> [applied] [config-dir]")
	}
	shell := args[0]
	if !shellreg.IsSupported(shell) {
		return fail(e, "unsupported shell %q (supported: %s)", shell, strings.Join(shellreg.Supported, ", "))
	}

	prev, _ := parseHookToken(os.Getenv("TAUGRES_HOOK"))
	// Reconcile the token's applied claim against shell reality: the shim passes its unexported _TAU_APPLIED, which
	// a child shell does not inherit (nor the aliases/functions), so an inherited "applied" token re-activates there.
	// When the arg is absent (a shim from an older tau), trust the claim.
	if len(args) >= 2 {
		prev.applied = prev.applied && args[1] == "1"
	}

	deactivateScript := func(proj string) []byte {
		data, _ := os.ReadFile(filepath.Join(state.GenDir(filepath.Join(proj, ".taugres")), "deactivate."+shell))
		return data
	}
	setToken := func(t hookToken) {
		// The token is exported so the next prompt's `tau hook-env` subprocess can
		// read it; _TAU_APPLIED is deliberately NOT exported so child shells (which
		// inherit the token but not aliases/functions) re-activate.
		if shell == "fish" {
			fmt.Fprintf(e.Stdout, "set -gx TAUGRES_HOOK %s\n", shellhook.FishSingleQuote(t.String()))
			if t.applied {
				fmt.Fprintln(e.Stdout, "set -g _TAU_APPLIED 1")
			} else {
				fmt.Fprintln(e.Stdout, "set -e _TAU_APPLIED")
			}
		} else {
			fmt.Fprintf(e.Stdout, "export TAUGRES_HOOK=%s\n", shellhook.SingleQuote(t.String()))
			if t.applied {
				fmt.Fprintln(e.Stdout, "_TAU_APPLIED=1")
			} else {
				fmt.Fprintln(e.Stdout, "unset _TAU_APPLIED")
			}
		}
	}

	// Capture the applied project's deactivate script NOW, before any sync can
	// regenerate it, so a teardown always matches how the env was applied (a
	// removed var/PATH entry is restored by the script that set it).
	var teardown []byte
	if prev.applied {
		teardown = deactivateScript(prev.proj)
	}

	var hint string
	if len(args) == 3 {
		hint = args[2]
	}

	d, err := discoverForHook(e.Wd, hint)
	if err != nil {
		// Outside any project: tear down whatever this shell applied and forget.
		if prev.applied {
			e.Stdout.Write(teardown)
		}
		if os.Getenv("TAUGRES_HOOK") != "" {
			if shell == "fish" {
				fmt.Fprintln(e.Stdout, "set -e TAUGRES_HOOK")
				fmt.Fprintln(e.Stdout, "set -e _TAU_APPLIED")
			} else {
				fmt.Fprintln(e.Stdout, "unset TAUGRES_HOOK _TAU_APPLIED")
			}
		}
		return 0
	}

	proj := d.ProjectRoot
	stateDir := filepath.Join(proj, ".taugres")

	// Trust decides everything downstream and is cheap to check in-process, so check it live on every prompt: an
	// untrusted project gets no sync attempt and no activation, and `tau allow` takes effect on the very next prompt
	// with no state to invalidate. A trust-store read error is fail-closed but surfaced below rather than silently
	// swallowed.
	allowed, terr := trust.IsAllowed(d.ConfigPath)
	trusted := allowed && terr == nil

	// Auto-sync when stale and trusted, guarded by the retry fingerprint: after a failed sync fp is non-empty and the
	// attempt is not repeated until the trigger state changes. InspectHook parses state once and reuses the activate
	// script stat as the token stamp.
	var fp string
	inspection := state.HookInspection{ActivationPath: filepath.Join(state.GenDir(stateDir), "activate."+shell)}
	if trusted {
		inspection = state.InspectHook(stateDir, d.ConfigPath, shell)
		if inspection.NeedsSync {
			fingerprint := state.SyncFingerprint(stateDir, d.ConfigPath, shellreg.Supported)
			last := ""
			if prev.proj == proj {
				last = prev.fp
			}
			if fingerprint == last {
				fp = last // same failing state: do not retry
			} else {
				syncProject(&Env{Stdout: e.Stderr, Stderr: e.Stderr, Wd: e.Wd, Ctx: e.ctx()}, syncOptions{ifStale: true})
				inspection = state.InspectHook(stateDir, d.ConfigPath, shell)
				if !inspection.NeedsSync {
					fp = "" // success
				} else {
					fp = state.SyncFingerprint(stateDir, d.ConfigPath, shellreg.Supported)
				}
			}
		}
	}

	// Target state: apply proj's activate script iff trusted and (after the
	// possible sync) the script exists.
	activate := inspection.ActivationPath
	stamp := inspection.ActivationStamp
	cur := hookToken{applied: trusted && stamp != "", stamp: stamp, fp: fp, proj: proj}
	if cur == prev {
		return 0 // nothing changed — the common case
	}

	// The sourced env is determined by (applied, proj, stamp); fp and the notice
	// do not touch it. Re-apply only when that triple changed — so a failed sync
	// (fp-only change) records the guard without disturbing a working env.
	if prev.applied != cur.applied || prev.proj != cur.proj || prev.stamp != cur.stamp {
		if prev.applied {
			e.Stdout.Write(teardown)
		}
		if cur.applied {
			if data, err := os.ReadFile(activate); err == nil {
				e.Stdout.Write(data)
			} else {
				cur.applied = false // raced: script vanished between stat and read
			}
		}
	}

	setToken(cur)
	// Explain why an in-project prompt did not activate — once per state, since we only reach here when the token
	// changed. A missing script while trusted (stamp == "") is governed by the fp retry guard and `tau status`, so stay
	// quiet.
	if !cur.applied {
		if terr != nil {
			fmt.Fprintf(e.Stderr, "tau: checking trust: %v\n", terr)
		} else if !allowed {
			fmt.Fprintf(e.Stderr, "tau: project is not trusted; run `tau allow`\n")
		}
	}

	return 0
}

// discoverForHook uses the config directory already found by the pure-shell gate, avoiding a second upward walk from
// a deeply nested working directory.  Invalid or out-of-tree hints fall back to normal discovery.
func discoverForHook(wd, hint string) (*discover.Discovery, error) {
	if hint == "" {
		return discover.Discover(wd)
	}
	absWD, wdErr := filepath.Abs(wd)
	absHint, hintErr := filepath.Abs(hint)
	if wdErr != nil || hintErr != nil {
		return discover.Discover(wd)
	}
	rel, err := filepath.Rel(absHint, absWD)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return discover.Discover(wd)
	}
	d, err := discover.Discover(absHint)
	if err != nil || d.ProjectRoot != filepath.Clean(absHint) {
		return discover.Discover(wd)
	}
	return d, nil
}

// --- allow / deny ---

func runAllow(e *Env, args []string) int {
	er, code := discoverAndEval(e)
	if er == nil {
		return code
	}
	if er.report.HasErrors() {
		for _, errMsg := range er.report.Errors {
			fmt.Fprintf(e.Stderr, "tau: error: %s\n", errMsg)
		}
		return fail(e, "config has validation errors; fix them before trusting")
	}
	if err := trust.Allow(er.disc.ConfigPath); err != nil {
		return fail(e, "recording trust: %v", err)
	}
	// No state to invalidate: the hook re-checks trust live on every prompt, so
	// the very next one syncs and activates.
	fmt.Fprintf(e.Stdout, "tau: trusted %s\n", er.disc.ConfigPath)
	return 0
}

func runDeny(e *Env, args []string) int {
	d, err := discover.Discover(e.Wd)
	if err != nil {
		return fail(e, "%v", err)
	}
	if err := trust.Deny(d.ConfigPath); err != nil {
		return fail(e, "revoking trust: %v", err)
	}
	fmt.Fprintf(e.Stdout, "tau: revoked trust for %s\n", d.ConfigPath)
	return 0
}

// --- clean ---

func runClean(e *Env, args []string) int {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	dropLock := fs.Bool("lock", false, "also delete .taugres.lock (next sync re-resolves unpinned tools)")
	dropCache := fs.Bool("cache", false, "only drop the sync cache (the manifest); installed tools stay, the next sync re-derives all state")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d, err := discover.Discover(e.Wd)
	if err != nil {
		return fail(e, "%v", err)
	}

	// Remove the regenerable project-local state. Trust (global) and the
	// mise store (shared) are intentionally left untouched. --cache keeps the
	// installed pip/npm/uv prefixes and only forgets the derived state, so the
	// next sync recomputes everything without reinstalling what is present.
	stateDir := filepath.Join(d.ProjectRoot, ".taugres")
	if *dropCache {
		manifest := state.ManifestPath(stateDir)
		if err := os.Remove(manifest); err != nil && !os.IsNotExist(err) {
			return fail(e, "removing %s: %v", manifest, err)
		}
		fmt.Fprintf(e.Stdout, "tau: removed %s\n", manifest)
	} else {
		if err := os.RemoveAll(stateDir); err != nil {
			return fail(e, "removing %s: %v", stateDir, err)
		}
		fmt.Fprintf(e.Stdout, "tau: removed %s\n", stateDir)
	}

	if *dropLock {
		lockPath := lock.Path(d.ProjectRoot)
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			return fail(e, "removing %s: %v", lockPath, err)
		}
		fmt.Fprintf(e.Stdout, "tau: removed %s\n", lockPath)
	}

	fmt.Fprintf(e.Stdout, "tau: run `tau sync` (or cd back in) to rebuild\n")
	return 0
}

// --- prune ---

func runPrune(e *Env, args []string) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pruned, err := trust.Prune()
	if err != nil {
		return fail(e, "pruning trust records: %v", err)
	}
	if len(pruned) == 0 {
		fmt.Fprintln(e.Stdout, "tau: no orphaned trust records")
		return 0
	}
	for _, p := range pruned {
		fmt.Fprintf(e.Stdout, "tau: pruned trust for %s\n", p)
	}
	return 0
}
