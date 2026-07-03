package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/edganiukov/taugres/internal/config"
	"github.com/edganiukov/taugres/internal/discover"
	"github.com/edganiukov/taugres/internal/lock"
	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/render"
	"github.com/edganiukov/taugres/internal/shellhook"
	"github.com/edganiukov/taugres/internal/state"
	"github.com/edganiukov/taugres/internal/tools/mise"
	"github.com/edganiukov/taugres/internal/tools/npm"
	"github.com/edganiukov/taugres/internal/tools/pip"
	"github.com/edganiukov/taugres/internal/tools/uv"
	"github.com/edganiukov/taugres/internal/trust"
	"github.com/edganiukov/taugres/internal/ui"
	"github.com/edganiukov/taugres/internal/validate"
)

// --- init ---

const workspaceTemplate = `# Taugres workspace config. Language: Starlark + Taugres API.
project("%s")

# Environment variables.
# shell.env("DATABASE_URL", "postgres://localhost/app")

# Tools/packages install via mise/pip/npm and are added to PATH automatically
# on activation (their real bin dirs are prepended, like mise activate). Each
# takes a name, a "name@version" spec, or a list of specs.
# mise.tool(["node@22.11.0", "ripgrep"])   # also mise backends: go:/cargo:/npm:/ubi:/aqua:
# pip.install(["ruff@0.6.9", "rich"])       # or uv.install([...]) (faster)
# npm.install("typescript")

# Paths are repository-root anchored with //.
# shell.path.prepend("//node_modules/.bin")

# Aliases.
# shell.alias("ll", "ls -lah")

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
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
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

// --- sync ---

func runSync(e *Env, args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	ifStale := fs.Bool("if-stale", false, "only sync if the config changed since the last sync (used by the shell hook)")
	verbose := fs.Bool("verbose", false, "print every sync step instead of a single updating line")
	update := fs.Bool("update", false, "re-resolve unpinned tools/packages to their latest versions and update .taugres.lock")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Discovery is cheap and needed before locking.
	d, err := discover.Discover(e.Wd)
	if err != nil {
		return fail(e, "%v", err)
	}
	stateDir := filepath.Join(d.ProjectRoot, ".taugres")

	// Trust gate first: an untrusted project can never sync (activation would
	// source shell.fn/shell.hook files and run installs), so bail before any
	// lock, Starlark eval, or tool work. In hook mode (--if-stale) stay silent —
	// `tau hook-env` is the single voice that tells the user to run `tau allow`
	// (it also pre-checks trust, so it normally never gets here); only a manual
	// `tau sync` says so itself.
	allowed, err := trust.IsAllowed(d.ConfigPath)
	if err != nil {
		return fail(e, "checking trust: %v", err)
	}
	if !allowed {
		if *ifStale {
			return 0
		}
		return fail(e, "project is not trusted; review the config, then run `tau allow`")
	}

	// Serialize syncs for this project: only one runs at a time; others wait.
	// Show the wait as a transient spinner line so it is cleared once we
	// proceed (and thus overwritten by the activation/synced message).
	var waitSpinner *ui.Spinner
	unlock, err := state.Lock(stateDir, func() {
		waitSpinner = ui.NewSpinner(e.Stderr)
		waitSpinner.Start("tau: waiting for another sync to finish…")
	})
	if err != nil {
		return fail(e, "acquiring sync lock: %v", err)
	}
	if waitSpinner != nil {
		waitSpinner.Stop()
	}
	defer unlock()

	// In hook mode, re-check under the lock so we don't redo work another
	// process just finished while we waited. The cheap mtime check is only a
	// trigger: timestamp granularity can miss a same-tick edit, and it does not
	// verify generated scripts. Confirm freshness with hashes before skipping.
	if *ifStale {
		need, err := state.NeedsSync(stateDir, d.ConfigPath)
		if err == nil {
			// The cheap mtime trigger also fires on a no-op touch (an editor save that
			// rewrites the file, `git checkout` bumping mtimes). If the thorough
			// hash/script/tool check says nothing actually changed, skip the whole
			// sync — no Starlark eval, no tool probing, no script regeneration. When
			// the cheap check did fire, re-anchor the manifest mtime so the hook stops
			// re-triggering on every prompt.
			if !state.CheckStale(stateDir, render.SupportedShells).Stale {
				if need {
					_ = state.TouchManifest(stateDir)
				}
				return 0
			}
		}
	}

	res, err := config.Evaluate(d)
	if err != nil {
		return fail(e, "evaluating %s:\n%v", d.ConfigPath, err)
	}
	report := validate.Validate(res.Plan)
	if report.HasErrors() {
		for _, errMsg := range report.Errors {
			fmt.Fprintf(e.Stderr, "tau: error: %s\n", errMsg)
		}
		return fail(e, "config has validation errors; run `tau check` for details")
	}

	plan := res.Plan
	rep := ui.NewReporter(e.Stderr, *verbose)
	defer rep.Done()

	// syncFail clears the progress line before printing an error, so error text
	// never gets appended to an active spinner line.
	syncFail := func(format string, args ...any) int {
		rep.Done()
		return fail(e, format, args...)
	}

	// Load the committed lockfile. Versions are pinned by default (reproducible);
	// --update re-resolves unpinned entries. GC prunes lock entries and
	// uninstalls packages that were removed from the config.
	lk, err := lock.Load(d.ProjectRoot)
	if err != nil {
		return syncFail("reading %s: %v", lock.FileName, err)
	}

	// Tool installs are best-effort: a failure is collected but does not abort
	// the sync, so the shell environment (env vars, PATH, aliases, functions) is
	// always generated even if a package fails to install.
	var toolErrs []string
	var toolErrsMu sync.Mutex
	addErr := func(msg string) {
		toolErrsMu.Lock()
		toolErrs = append(toolErrs, msg)
		toolErrsMu.Unlock()
	}

	// Split the sync into two concerns: installing tools (slow, may hit the
	// network) and preparing the shell (fast, always regenerated below). A tool
	// manager (mise/pip/npm/uv) is fresh when its signature — its declared
	// tools/packages joined with their locked versions — is unchanged since the
	// last sync and its bin dirs still exist; a fresh manager's install is
	// skipped. When every manager is fresh, the whole install phase is skipped
	// and the cached mise store bin dirs are reused for PATH without re-probing
	// mise, so editing an env var / alias / hook never spawns a tool subprocess.
	// --update forces a full install pass.
	//
	// Signatures are computed from the lock as loaded from disk, which in steady
	// state already carries the resolved versions from the last sync.
	sig := toolSigs(plan, lk)
	prior, _ := state.Load(plan.StateDir)
	fresh := freshness(prior, sig, plan, *update)

	var miseBinDirs, toolDirs []string
	if prior != nil && fresh.allFresh() {
		// Recover the mise store bin dirs (needed for PATH) from the recorded tool
		// dirs by dropping the package bin dirs, which are deterministic from the
		// plan — so no dir is cached twice.
		toolDirs = prior.ToolDirs
		miseBinDirs = miseBinDirsFrom(toolDirs, plan)
	} else {
		miseBinDirs, toolDirs = installTools(plan, lk, rep, *update, addErr, fresh)
		// Persist the lockfile (best effort; it is committed with the project).
		if err := lk.Save(d.ProjectRoot); err != nil {
			addErr("writing " + lock.FileName + ": " + err.Error())
		}
		// Recompute from the post-install lock so the stored signatures reflect the
		// now-resolved versions; the next sync's signatures (read from the same lock
		// on disk) then match and each manager takes the fast path.
		sig = toolSigs(plan, lk)
	}

	// Prepend mise tool bin dirs to the activation PATH (in front of the
	// project-local pip/npm dirs already in plan.PathPrepend).
	plan.PathPrepend = dedupStrings(append(append([]string{}, miseBinDirs...), plan.PathPrepend...))

	rep.Step("tau: generating shell scripts")
	genDir := state.GenDir(plan.StateDir)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return syncFail("creating %s: %v", genDir, err)
	}

	for _, sh := range render.SupportedShells {
		act, err := render.Activate(plan, sh)
		if err != nil {
			return syncFail("rendering activate.%s: %v", sh, err)
		}
		deact, err := render.Deactivate(plan, sh)
		if err != nil {
			return syncFail("rendering deactivate.%s: %v", sh, err)
		}
		if err := os.WriteFile(filepath.Join(genDir, "activate."+sh), []byte(act), 0o644); err != nil {
			return syncFail("writing activate.%s: %v", sh, err)
		}
		if err := os.WriteFile(filepath.Join(genDir, "deactivate."+sh), []byte(deact), 0o644); err != nil {
			return syncFail("writing deactivate.%s: %v", sh, err)
		}
	}

	// Write the manifest last so it is the newest file: the staleness checks
	// treat any recorded input newer than the manifest as "changed". It records
	// config inputs (config file, loaded modules, fn/hook source files), the tool
	// bin dirs that must exist, and the exists()/which() probe results — so a
	// changed input, a removed tool dir, or a flipped probe all trigger a resync.
	m, err := buildManifest(res, toolDirs, sig)
	if err != nil {
		return syncFail("building manifest: %v", err)
	}
	if err := m.Write(plan.StateDir); err != nil {
		return syncFail("writing manifest: %v", err)
	}

	ensureGitignore(plan.ProjectRoot)

	rep.Done()
	for _, warn := range report.Warnings {
		fmt.Fprintf(e.Stderr, "tau: warning: %s\n", warn)
	}
	// Surface any tool-install failures. The env is generated regardless, so the
	// shell still works; re-run `tau sync` to retry the failed installs.
	for _, te := range toolErrs {
		fmt.Fprintf(e.Stderr, "tau: %s\n", te)
	}

	// Confirm completion for a manual `tau sync`. On the hook's auto-sync
	// (--if-stale) the activation banner is the finale instead, so stay quiet.
	if !*ifStale {
		if len(toolErrs) > 0 {
			fmt.Fprintf(e.Stdout, "=^..^= tau synced %s (env only; some tools failed)\n", displayName(plan))
		} else {
			fmt.Fprintf(e.Stdout, "=^..^= tau synced %s\n", displayName(plan))
		}
	}
	if len(toolErrs) > 0 {
		return 1
	}
	return 0
}

// displayName returns the project's name, falling back to its directory name.
func displayName(plan *model.Plan) string {
	if plan.ProjectName != "" {
		return plan.ProjectName
	}
	return filepath.Base(plan.ProjectRoot)
}

// buildManifest hashes every config input (config file, loaded modules, fn/hook
// source files) and pairs them with the tool dirs, probe results, and the
// per-manager tool signatures into the single manifest.
func buildManifest(res *config.Result, toolDirs []string, toolSig map[string]string) (*state.Manifest, error) {
	plan := res.Plan
	inputs := map[string]string{}
	add := func(path string) error {
		h, err := state.HashFile(path)
		if err != nil {
			return err
		}
		inputs[path] = h
		return nil
	}
	if err := add(plan.ConfigPath); err != nil {
		return nil, err
	}
	for _, m := range res.LoadedModules {
		if err := add(m); err != nil {
			return nil, err
		}
	}
	for _, f := range sourceFiles(plan) {
		if err := add(f); err != nil {
			return nil, err
		}
	}
	return &state.Manifest{
		Inputs:   inputs,
		ToolDirs: toolDirs,
		Probes:   res.Probes,
		ToolSig:  toolSig,
	}, nil
}

// packageBinDirs returns the pip/npm/uv bin dirs recorded in the tool-dir list —
// one per manager that has packages. Unlike the mise store dirs (resolved via
// `mise where`), these are deterministic from the plan, so they are not cached
// separately: the fast path recovers the mise dirs by removing these from the
// recorded tool dirs.
func packageBinDirs(plan *model.Plan) []string {
	var dirs []string
	if len(plan.PipPackages) > 0 {
		dirs = append(dirs, filepath.Join(plan.PipDir, "bin"))
	}
	if len(plan.NpmPackages) > 0 {
		dirs = append(dirs, filepath.Join(plan.NpmDir, "bin"))
	}
	if len(plan.UvPackages) > 0 {
		dirs = append(dirs, filepath.Join(plan.UvDir, "bin"))
	}
	return dirs
}

// miseBinDirsFrom recovers the mise store bin dirs from a recorded tool-dir list
// by removing the deterministic package bin dirs.
func miseBinDirsFrom(toolDirs []string, plan *model.Plan) []string {
	pkg := map[string]bool{}
	for _, d := range packageBinDirs(plan) {
		pkg[d] = true
	}
	var out []string
	for _, d := range toolDirs {
		if !pkg[d] {
			out = append(out, d)
		}
	}
	return out
}

// allExist reports whether every path is an existing directory. It gates the
// install-skipping fast path: a wiped .taugres/tools forces a full reinstall.
func allExist(dirs []string) bool {
	for _, d := range dirs {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// toolSigs fingerprints each tool manager's install-relevant state: its declared
// tools/packages paired with their locked spec (requested + resolved). A manager
// with nothing declared is omitted. Comparing these per-manager lets a sync
// reinstall only the manager whose inputs changed: a changed declaration, a
// re-pin, or a dropped lock entry (e.g. from `tau update`) flips just that
// manager's signature. Entries are sorted so declaration order does not affect
// the hash.
func toolSigs(plan *model.Plan, lk *lock.File) map[string]string {
	sigs := map[string]string{}
	hash := func(mgr string, lines []string) {
		if len(lines) == 0 {
			return
		}
		sort.Strings(lines)
		sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		sigs[mgr] = hex.EncodeToString(sum[:])
	}
	line := func(name, ver string, sec map[string]lock.Entry) string {
		e := sec[name]
		return strings.Join([]string{name, ver, e.Requested, e.Resolved}, "\x1f")
	}
	var mise, pip, npm, uv []string
	for _, t := range plan.MiseTools {
		mise = append(mise, line(t.Name, t.Version, lk.Mise))
	}
	for _, p := range plan.PipPackages {
		pip = append(pip, line(p.Name, p.Version, lk.Pip))
	}
	for _, p := range plan.NpmPackages {
		npm = append(npm, line(p.Name, p.Version, lk.Npm))
	}
	for _, p := range plan.UvPackages {
		uv = append(uv, line(p.Name, p.Version, lk.Uv))
	}
	hash("mise", mise)
	hash("pip", pip)
	hash("npm", npm)
	hash("uv", uv)
	return sigs
}

// toolFreshness records, for one sync, which managers are already up to date (so
// their install is skipped) plus the mise store bin dirs cached from the last
// sync — reused for PATH when mise is fresh, so no `mise where` probe is needed.
type toolFreshness struct {
	miseStale, pipStale, npmStale, uvStale bool
	miseDirs                               []string // cached mise bin dirs, valid when !miseStale
}

// allFresh reports whether no manager needs installing, so the whole install
// phase can be skipped and only the shell scripts regenerated.
func (f toolFreshness) allFresh() bool {
	return !f.miseStale && !f.pipStale && !f.npmStale && !f.uvStale
}

// freshness compares the current per-manager signatures against the last sync's
// to decide which managers must reinstall. A manager is stale when --update is
// set, it was added or dropped since the last sync, its signature changed, or
// its recorded bin dirs are missing. The mise store dirs are recovered from the
// prior manifest (so freshness needs no `mise where`).
func freshness(prior *state.Manifest, cur map[string]string, plan *model.Plan, force bool) toolFreshness {
	var priorSig map[string]string
	var miseDirs []string
	if prior != nil {
		priorSig = prior.ToolSig
		miseDirs = miseBinDirsFrom(prior.ToolDirs, plan)
	}
	// A manager is stale when it was declared before XOR now (added/dropped), or —
	// when declared both times — --update forces it, its signature changed, or its
	// dirs vanished. A manager declared in neither sync is fresh (nothing to do).
	stale := func(mgr string, declared bool, dirs []string) bool {
		_, was := priorSig[mgr]
		if declared != was {
			return true
		}
		if !declared {
			return false
		}
		return force || cur[mgr] != priorSig[mgr] || !allExist(dirs)
	}
	return toolFreshness{
		miseStale: stale("mise", len(plan.MiseTools) > 0, miseDirs),
		pipStale:  stale("pip", len(plan.PipPackages) > 0, []string{filepath.Join(plan.PipDir, "bin")}),
		npmStale:  stale("npm", len(plan.NpmPackages) > 0, []string{filepath.Join(plan.NpmDir, "bin")}),
		uvStale:   stale("uv", len(plan.UvPackages) > 0, []string{filepath.Join(plan.UvDir, "bin")}),
		miseDirs:  miseDirs,
	}
}

// installTools runs the per-tool staleness + install pipeline: it installs only
// the managers whose declared set changed (or all, when force), GCs tools
// dropped from the config, and returns the mise store bin dirs (prepended to
// PATH) plus the full set of tool bin dirs (recorded for staleness). Install
// failures are reported via addErr rather than aborting, so the shell env is
// always built. It may hit the network and belongs to sync, never activation.
func installTools(plan *model.Plan, lk *lock.File, rep *ui.Reporter, force bool, addErr func(string), fresh toolFreshness) (miseBinDirs, toolDirs []string) {
	// Tool output is streamed through the reporter prefixed with the tool name.
	installReport := func(name string) func(bool) {
		rep.Step("tau: installing " + name)
		return func(ok bool) {
			if ok {
				rep.Step("tau: installed " + name)
			}
		}
	}

	var toolchainBinDirs, restBinDirs []string
	miseReinstalled := false
	var wg sync.WaitGroup

	switch {
	case len(plan.MiseTools) == 0:
		// Nothing declared: no store dirs on PATH (and any previously-installed
		// tools were dropped, so gcTools prunes their lock entries below).
	case !mise.Available():
		addErr("mise is required to install tools but is not installed — install it with `curl https://mise.run | sh` (see https://mise.jdx.dev; the mise binary on PATH is all tau needs)")
	case !fresh.miseStale:
		// Unchanged: reuse the store bin dirs cached from the last sync (no
		// `mise where`) for PATH and as the toolchain for pip/uv/npm.
		miseBinDirs = fresh.miseDirs
		toolchainBinDirs = fresh.miseDirs
	default:
		miseReinstalled = true
		// Effective versions (locked unless --update / spec changed).
		effMise := make([]model.MiseTool, len(plan.MiseTools))
		reqByName := map[string]string{}
		for i, t := range plan.MiseTools {
			e, ok := lk.Mise[t.Name]
			effMise[i] = model.MiseTool{Name: t.Name, Version: lock.InstallVersion(t.Version, e, ok, force)}
			reqByName[t.Name] = t.Version
		}
		// recordMise writes lock entries and returns the tools' bin dirs. Each
		// caller touches only its own tools' entries, so concurrent calls are safe.
		recordMise := func(installed []mise.Installed) []string {
			var dirs []string
			for _, ins := range installed {
				lk.Mise[ins.Name] = lock.Entry{Requested: reqByName[ins.Name], Resolved: ins.Resolved}
				dirs = append(dirs, ins.BinDir)
			}
			return dirs
		}

		// The package toolchain (node for npm; python for pip/uv; uv for uv) is
		// installed first and in isolation, so a failure of any *other* mise tool
		// can never stop pip/uv/npm from getting their runtime. finalize
		// guarantees these implicit tools whenever the packages are declared.
		needNode := len(plan.NpmPackages) > 0
		needPython := len(plan.PipPackages) > 0 || len(plan.UvPackages) > 0
		needUv := len(plan.UvPackages) > 0
		var toolchain, rest []model.MiseTool
		for _, t := range effMise {
			if (needNode && t.Name == "node") || (needPython && t.Name == "python") || (needUv && t.Name == "uv") {
				toolchain = append(toolchain, t)
			} else {
				rest = append(rest, t)
			}
		}

		if len(toolchain) > 0 {
			installed, err := mise.Install(toolchain, plan.MiseJobs, rep.Stream("mise: "), installReport)
			if err != nil {
				addErr(err.Error())
			}
			toolchainBinDirs = recordMise(installed)
		}
		// The rest of the mise tools install concurrently with pip/npm/uv below.
		if len(rest) > 0 {
			wg.Go(func() {
				installed, err := mise.Install(rest, plan.MiseJobs, rep.Stream("mise: "), installReport)
				if err != nil {
					addErr(err.Error())
				}
				restBinDirs = recordMise(installed)
			})
		}
	}

	// The pip/uv/npm integrations are uniform, so describe them once and let
	// install and GC iterate instead of repeating near-identical blocks. Each
	// closure captures its manager's prefix dir, the toolchain bin dirs, and its
	// output stream. dir is the canonical prefix (known even with no packages, so
	// GC can remove a fully-dropped manager); its bin/ is auto-prepended to PATH.
	pkgDir := func(name string) string { return filepath.Join(plan.StateDir, "tools", name) }
	managers := []packageManager{
		{
			label: "pip", stale: fresh.pipStale, pkgs: plan.PipPackages, dir: pkgDir("pip"), section: lk.Pip,
			install: func(p []model.Package) (map[string]string, error) {
				return pip.Install(p, pkgDir("pip"), toolchainBinDirs, rep.Stream("pip: "), installReport)
			},
			uninstall: func(n []string) error { return pip.Uninstall(pkgDir("pip"), n, rep.Stream("pip: ")) },
		},
		{
			label: "npm", stale: fresh.npmStale, pkgs: plan.NpmPackages, dir: pkgDir("npm"), section: lk.Npm,
			install: func(p []model.Package) (map[string]string, error) {
				return npm.Install(p, pkgDir("npm"), toolchainBinDirs, rep.Stream("npm: "), installReport)
			},
			uninstall: func(n []string) error { return npm.Uninstall(pkgDir("npm"), n, toolchainBinDirs, rep.Stream("npm: ")) },
		},
		{
			label: "uv", stale: fresh.uvStale, pkgs: plan.UvPackages, dir: pkgDir("uv"), section: lk.Uv,
			install: func(p []model.Package) (map[string]string, error) {
				return uv.Install(p, pkgDir("uv"), toolchainBinDirs, rep.Stream("uv: "), installReport)
			},
			uninstall: func(n []string) error { return uv.Uninstall(pkgDir("uv"), n, toolchainBinDirs, rep.Stream("uv: ")) },
		},
	}
	// Install stale managers concurrently (with the rest of mise, above). They use
	// the toolchain bin dirs to find python/node/uv, so never wait on other tools.
	for i := range managers {
		m := managers[i]
		if !m.stale {
			continue
		}
		wg.Go(func() {
			resolved, err := m.install(effectiveVersions(m.pkgs, m.section, force))
			if err != nil {
				addErr(err.Error())
			}
			recordResolved(m.section, m.pkgs, resolved)
		})
	}
	wg.Wait()
	if miseReinstalled {
		miseBinDirs = append(toolchainBinDirs, restBinDirs...)
	}

	// GC: uninstall packages and prune lock entries that were removed from the
	// config.
	gcTools(plan, lk, managers, rep)

	// Tool bin dirs recorded for staleness: mise store bins followed by each
	// package manager's prefix bin (present only when it has packages). The order
	// and derivation must match miseBinDirsFrom, which recovers the mise subset by
	// removing the package bin dirs.
	toolDirs = append(append([]string{}, miseBinDirs...), packageBinDirs(plan)...)
	return miseBinDirs, toolDirs
}

// gcTools removes packages and lock entries that were dropped from the config.
// PATH entries need no cleanup — they are regenerated from the current config.
// mise installs live in mise's shared store, so only their lock entries are
// pruned; pip/npm packages are uninstalled from their project-local prefixes.
func gcTools(plan *model.Plan, lk *lock.File, managers []packageManager, rep *ui.Reporter) {
	// mise: prune lock entries for tools no longer declared (installs live in
	// mise's shared store, so only the lock entry is dropped).
	keep := nameSet(plan.MiseTools, func(t model.MiseTool) string { return t.Name })
	for name := range lk.Mise {
		if !keep[name] {
			delete(lk.Mise, name)
		}
	}

	// pip/uv/npm: drop the whole prefix if the manager has no packages left;
	// otherwise uninstall just the ones removed from the config.
	for _, m := range managers {
		if len(m.pkgs) == 0 {
			_ = os.RemoveAll(m.dir)
			clear(m.section)
			continue
		}
		removed := removedKeys(m.section, nameSet(m.pkgs, func(p model.Package) string { return p.Name }))
		if len(removed) > 0 {
			rep.Step("tau: removing " + strings.Join(removed, ", "))
			_ = m.uninstall(removed)
			for _, n := range removed {
				delete(m.section, n)
			}
		}
	}
}

// packageManager describes one of the uniform pip/uv/npm integrations so a sync
// can drive install and GC from a table instead of near-identical blocks. The
// install/uninstall closures capture the manager's prefix dir, toolchain bin
// dirs, and output stream.
type packageManager struct {
	label     string
	stale     bool
	pkgs      []model.Package
	dir       string // canonical prefix (<stateDir>/tools/<label>); its bin/ is on PATH
	section   map[string]lock.Entry
	install   func(pkgs []model.Package) (map[string]string, error)
	uninstall func(names []string) error
}

// effectiveVersions maps declared packages to the versions to install: the
// locked version unless --update or the spec changed re-resolves them.
func effectiveVersions(pkgs []model.Package, locked map[string]lock.Entry, update bool) []model.Package {
	eff := make([]model.Package, len(pkgs))
	for i, p := range pkgs {
		e, ok := locked[p.Name]
		eff[i] = model.Package{Name: p.Name, Version: lock.InstallVersion(p.Version, e, ok, update)}
	}
	return eff
}

// recordResolved writes each package's resolved concrete version back to the
// lock section.
func recordResolved(locked map[string]lock.Entry, pkgs []model.Package, resolved map[string]string) {
	for _, p := range pkgs {
		if v, ok := resolved[p.Name]; ok {
			locked[p.Name] = lock.Entry{Requested: p.Version, Resolved: v}
		}
	}
}

// removedKeys returns the sorted lock keys that are absent from keep.
func removedKeys(entries map[string]lock.Entry, keep map[string]bool) []string {
	var out []string
	for name := range entries {
		if !keep[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// nameSet builds a set of names from a package/tool slice.
func nameSet[T any](items []T, name func(T) string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[name(it)] = true
	}
	return m
}

// dedupStrings returns in with duplicate entries removed, preserving order.
func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// sourceFiles returns the sorted, de-duplicated set of external files
// referenced by shell.fn and shell.hook (file=). Inline content is skipped.
func sourceFiles(p *model.Plan) []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, entries := range p.SourceFuncs {
		for _, sf := range entries {
			add(sf.File)
		}
	}
	for _, h := range p.Hooks {
		add(h.File)
	}
	sort.Strings(out)
	return out
}

func ensureGitignore(projectRoot string) {
	gi := filepath.Join(projectRoot, ".gitignore")
	data, err := os.ReadFile(gi)
	if err == nil {
		if slices.Contains(strings.Split(string(data), "\n"), ".taugres/") {
			return
		}
		f, err := os.OpenFile(gi, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(".taugres/\n")
		return
	}
	if os.IsNotExist(err) {
		_ = os.WriteFile(gi, []byte(".taugres/\n"), 0o644)
	}
}

// --- update ---

// updManagers are the tool-manager qualifiers accepted as a "<manager>:name"
// prefix by `tau update`, in lock-section order.
var updManagers = []string{"mise", "pip", "npm", "uv"}

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
	switch manager {
	case "mise":
		return lk.Mise
	case "pip":
		return lk.Pip
	case "npm":
		return lk.Npm
	case "uv":
		return lk.Uv
	}
	return nil
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
	if want("pip") {
		for _, x := range p.PipPackages {
			if x.Name == name {
				out = append(out, updTarget{"pip", x.Version != ""})
				break
			}
		}
	}
	if want("npm") {
		for _, x := range p.NpmPackages {
			if x.Name == name {
				out = append(out, updTarget{"npm", x.Version != ""})
				break
			}
		}
	}
	if want("uv") {
		for _, x := range p.UvPackages {
			if x.Name == name {
				out = append(out, updTarget{"uv", x.Version != ""})
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

	syncArgs := []string{}
	if *verbose {
		syncArgs = append(syncArgs, "--verbose")
	}
	// No names: update everything unpinned.
	if len(names) == 0 {
		return runSync(e, append(syncArgs, "--update"))
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
				delete(sec, name)
			}
			updated = append(updated, cleared{t.manager, name, old})
		}
	}
	if len(updated) == 0 {
		fmt.Fprintln(e.Stdout, "tau: nothing to update")
		return 0
	}
	if err := lk.Save(er.disc.ProjectRoot); err != nil {
		return fail(e, "writing %s: %v", lock.FileName, err)
	}

	labels := make([]string, len(updated))
	for i, u := range updated {
		labels[i] = u.name
	}
	fmt.Fprintf(e.Stdout, "tau: updating %s\n", strings.Join(labels, ", "))
	if code := runSync(e, syncArgs); code != 0 {
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

	stale := state.CheckStale(plan.StateDir, render.SupportedShells)
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

	if len(plan.PipPackages) > 0 {
		fmt.Fprintf(e.Stdout, "\npip packages:\n")
		for _, p := range plan.PipPackages {
			ver := p.Version
			if ver == "" {
				ver = "latest"
			}
			fmt.Fprintf(e.Stdout, "  %s==%s\n", p.Name, ver)
		}
	}

	if len(plan.NpmPackages) > 0 {
		fmt.Fprintf(e.Stdout, "\nnpm packages:\n")
		for _, p := range plan.NpmPackages {
			ver := p.Version
			if ver == "" {
				ver = "latest"
			}
			fmt.Fprintf(e.Stdout, "  %s@%s\n", p.Name, ver)
		}
	}

	if len(plan.UvPackages) > 0 {
		fmt.Fprintf(e.Stdout, "\nuv packages:\n")
		for _, p := range plan.UvPackages {
			ver := p.Version
			if ver == "" {
				ver = "latest"
			}
			fmt.Fprintf(e.Stdout, "  %s==%s\n", p.Name, ver)
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
	// Bake in the absolute path to this tau binary so the hook invokes exactly
	// this executable via `tau hook-env`.
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

// --- activate / deactivate ---

func runActivate(e *Env, args []string) int   { return emitGenScript(e, args, "activate") }
func runDeactivate(e *Env, args []string) int { return emitGenScript(e, args, "deactivate") }

// emitGenScript prints the generated activate/deactivate script for the current
// project to stdout, so the shell hook (or a user) can `eval` it. It runs on the
// hot path (every project entry) and does no staleness check: the hook re-syncs
// on change before calling it, and `tau status` reports staleness.
//
// activate is trust-gated — the hook sources this stdout, so refusing an
// untrusted project is the security boundary (trust lives outside the repo, so a
// clone can't forge it and can't run code on cd). deactivate is not gated: it
// only restores saved env/PATH and removes tau-created aliases/functions (no user
// hook code), so tearing down must always work — even after `tau deny` — and its
// guards make sourcing it a no-op when nothing was active.
func emitGenScript(e *Env, args []string, kind string) int {
	var shell string
	switch len(args) {
	case 0:
		// Default to the current shell via $SHELL. The hook always passes the shell
		// explicitly (it knows it); this default is for manual `eval "$(tau activate)"`.
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
	if !slices.Contains(render.SupportedShells, shell) {
		return fail(e, "unsupported shell %q (supported: %s)", shell, strings.Join(render.SupportedShells, ", "))
	}
	d, err := discover.Discover(e.Wd)
	if err != nil {
		return fail(e, "%v", err)
	}

	if kind == "activate" {
		allowed, err := trust.IsAllowed(d.ConfigPath)
		if err != nil {
			return fail(e, "checking trust: %v", err)
		}
		if !allowed {
			return fail(e, "project is not trusted; run `tau allow`")
		}
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

// runHookEnv is the hook backend: the shell shim evals this command's stdout on
// every in-project prompt, and ALL hook logic — staleness, the retry guard,
// auto-sync, trust, activation/deactivation — lives here in Go. Session state
// round-trips through the TAUGRES_HOOK env var, which the eval'd output itself
// sets, so the shell holds no state machine and tau writes no state files.
//
// The token is "<proj>|<activate mtime ns>|<retry fingerprint>", prefixed with
// "-" when the state was recorded but nothing activated (untrusted, or no
// generated script). The dormant prefix keeps once-per-shell semantics for the
// "not trusted" notice — re-entering the project compares equal and stays
// silent — and lets the shim skip the tau invocation entirely on prompts
// outside any project. The fingerprint is set after a failed sync attempt so it
// is not re-run (and its error not re-printed) until the trigger state changes.
func runHookEnv(e *Env, args []string) int {
	if len(args) != 1 {
		return fail(e, "usage: tau hook-env <shell> (bash|zsh|fish)")
	}
	shell := args[0]
	if !slices.Contains(render.SupportedShells, shell) {
		return fail(e, "unsupported shell %q (supported: %s)", shell, strings.Join(render.SupportedShells, ", "))
	}

	// Previous state from our last eval'd output. A dormant ("-") token records
	// an attempt with nothing activated: nothing to tear down. The fingerprint
	// and mtime fields contain no '|', so the project (which may) parses from
	// the right.
	prev := os.Getenv("TAUGRES_HOOK")
	prevTok := strings.TrimPrefix(prev, "-")
	prevFP := ""
	if i := strings.LastIndexByte(prevTok, '|'); i >= 0 {
		prevFP = prevTok[i+1:]
	}
	activeProj := ""
	if active := prev != "" && prev == prevTok; active {
		if rest, ok := strings.CutSuffix(prevTok, "|"+prevFP); ok {
			if i := strings.LastIndexByte(rest, '|'); i >= 0 {
				activeProj = rest[:i]
			}
		}
	}
	// emitDeactivate prints a project's deactivate script (best effort; the
	// script's own guards make it a no-op if nothing was applied).
	emitDeactivate := func(proj string) {
		p := filepath.Join(state.GenDir(filepath.Join(proj, ".taugres")), "deactivate."+shell)
		if data, err := os.ReadFile(p); err == nil {
			fmt.Fprint(e.Stdout, string(data))
		}
	}
	// setState emits the shell command that records the session token.
	setState := func(tok string) {
		if shell == "fish" {
			fmt.Fprintf(e.Stdout, "set -gx TAUGRES_HOOK %s\n", fishQuote(tok))
		} else {
			fmt.Fprintf(e.Stdout, "export TAUGRES_HOOK=%s\n", posixQuote(tok))
		}
	}

	d, err := discover.Discover(e.Wd)
	if err != nil {
		// Outside any project: tear down whatever was active and forget it.
		if activeProj != "" {
			emitDeactivate(activeProj)
			if shell == "fish" {
				fmt.Fprintln(e.Stdout, "set -e TAUGRES_HOOK")
			} else {
				fmt.Fprintln(e.Stdout, "unset TAUGRES_HOOK")
			}
		}
		return 0
	}
	stateDir := filepath.Join(d.ProjectRoot, ".taugres")

	// Trust decides everything downstream and is cheap to check in-process, so
	// check it live on every prompt: an untrusted project gets no sync attempt
	// and no activation — and nothing written anywhere — while `tau allow` takes
	// effect on the very next prompt with no state to invalidate.
	allowed, terr := trust.IsAllowed(d.ConfigPath)
	allowed = allowed && terr == nil

	// Auto-sync when stale. The retry fingerprint carried in the session token
	// guards a failing sync: it is recorded after a failed attempt and the sync
	// is retried only when the trigger state (inputs, scripts/tool-dir presence,
	// probes) changes. The sync's own output goes to stderr; our stdout stays
	// eval-clean.
	fp := ""
	if need, err := state.NeedsSync(stateDir, d.ConfigPath); err == nil && need && allowed {
		fp = state.SyncFingerprint(stateDir, d.ConfigPath, render.SupportedShells)
		if fp != prevFP {
			if activeProj == d.ProjectRoot {
				// Tear down with the CURRENT deactivate script before the sync
				// regenerates it, so removed vars/PATH don't leak.
				emitDeactivate(activeProj)
				activeProj = ""
			}
			runSync(&Env{Args: nil, Stdout: e.Stderr, Stderr: e.Stderr, Wd: e.Wd}, []string{"--if-stale"})
			if need, err := state.NeedsSync(stateDir, d.ConfigPath); err == nil && !need {
				fp = "" // success: drop the guard
			} else {
				fp = state.SyncFingerprint(stateDir, d.ConfigPath, render.SupportedShells)
			}
		}
	}

	// Desired state: project + activate script mtime (ns) + retry fingerprint.
	// Unchanged state prints nothing — the common case.
	activate := filepath.Join(state.GenDir(stateDir), "activate."+shell)
	stamp := ""
	if fi, err := os.Stat(activate); err == nil {
		stamp = strconv.FormatInt(fi.ModTime().UnixNano(), 10)
	}
	cur := d.ProjectRoot + "|" + stamp + "|" + fp
	if cur == prevTok {
		return 0
	}
	// Only the retry fingerprint changed (a sync just failed, leaving the
	// generated env as it was): record the state but leave the active env alone
	// — no deactivate/reactivate churn.
	prevAct, _ := strings.CutSuffix(prevTok, "|"+prevFP)
	if d.ProjectRoot+"|"+stamp == prevAct {
		if prev != prevTok { // preserve dormancy
			setState("-" + cur)
		} else {
			setState(cur)
		}
		return 0
	}
	if activeProj != "" {
		emitDeactivate(activeProj)
	}

	// Trust is the boundary: refuse to emit repo-derived code for an untrusted
	// project (trust lives outside the repo and cannot be forged by its
	// contents). Record the state dormant so the notice prints once per shell,
	// not on every prompt.
	if !allowed {
		setState("-" + cur)
		fmt.Fprintf(e.Stderr, "tau: project is not trusted; run `tau allow`\n")
		return 0
	}
	data, err := os.ReadFile(activate)
	if err != nil {
		// No generated script (the sync failed); the fingerprint governs
		// re-attempts, so just record the state.
		setState("-" + cur)
		return 0
	}
	setState(cur)
	fmt.Fprint(e.Stdout, string(data))
	return 0
}

// posixQuote wraps s as a POSIX single-quoted literal.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fishQuote wraps s as a fish single-quoted literal.
func fishQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d, err := discover.Discover(e.Wd)
	if err != nil {
		return fail(e, "%v", err)
	}

	// Remove the regenerable project-local state. Trust (global) and the
	// mise store (shared) are intentionally left untouched.
	stateDir := filepath.Join(d.ProjectRoot, ".taugres")
	if err := os.RemoveAll(stateDir); err != nil {
		return fail(e, "removing %s: %v", stateDir, err)
	}
	fmt.Fprintf(e.Stdout, "tau: removed %s\n", stateDir)

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
