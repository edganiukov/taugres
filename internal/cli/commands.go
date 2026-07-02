package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
	// process just finished while we waited.
	if *ifStale {
		if need, err := state.NeedsSync(stateDir, d.ConfigPath); err == nil && !need {
			return 0
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

	// Trust gate: refuse to (re)generate scripts for untrusted projects, since
	// activation sources fn.source files and runs mise installs.
	allowed, err := trust.IsAllowed(d.ConfigPath)
	if err != nil {
		return fail(e, "tau: checking trust: %v", err)
	}
	if !allowed {
		return fail(e, "tau: project is not trusted\nreview the config, then run `tau allow`")
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

	// Install declared tools. This may hit the network; it belongs to sync,
	// never activation. Tool output is streamed through the reporter prefixed
	// with the tool name ("mise: "/"pip: "/"npm: ").
	installReport := func(name string) func(bool) {
		rep.Step("tau: installing " + name)
		return func(ok bool) {
			if ok {
				rep.Step("tau: installed " + name)
			}
		}
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
	//
	// mise tools are exposed the way `mise activate` does — by prepending their
	// real install bin dirs to PATH (no symlink/wrapper farm). pip/npm then use
	// node/python from those dirs, and install into their own project-local
	// prefixes.
	var toolErrs []string
	var toolErrsMu sync.Mutex
	addErr := func(msg string) {
		toolErrsMu.Lock()
		toolErrs = append(toolErrs, msg)
		toolErrsMu.Unlock()
	}
	var miseBinDirs []string
	if len(plan.MiseTools) > 0 && !mise.Available() {
		addErr("mise is required to install tools but is not installed — install it with `curl https://mise.run | sh` (see https://mise.jdx.dev; the mise binary on PATH is all tau needs)")
	} else {
		// Effective versions (locked unless --update / spec changed).
		effMise := make([]model.MiseTool, len(plan.MiseTools))
		reqByName := map[string]string{}
		for i, t := range plan.MiseTools {
			e, ok := lk.Mise[t.Name]
			effMise[i] = model.MiseTool{Name: t.Name, Version: lock.InstallVersion(t.Version, e, ok, *update)}
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

		var toolchainBinDirs []string
		if len(toolchain) > 0 {
			installed, err := mise.Install(toolchain, plan.MiseJobs, rep.Stream("mise: "), installReport)
			if err != nil {
				addErr(err.Error())
			}
			toolchainBinDirs = recordMise(installed)
		}

		// With the toolchain ready, the rest of the mise tools and the pip/npm
		// installs are independent — run them concurrently. pip/npm use only the
		// toolchain dirs, so they never wait on (or fail because of) other tools.
		var restBinDirs []string
		var wg sync.WaitGroup
		if len(rest) > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				installed, err := mise.Install(rest, plan.MiseJobs, rep.Stream("mise: "), installReport)
				if err != nil {
					addErr(err.Error())
				}
				restBinDirs = recordMise(installed)
			}()
		}
		if len(plan.PipPackages) > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				eff := make([]model.PipPackage, len(plan.PipPackages))
				for i, p := range plan.PipPackages {
					e, ok := lk.Pip[p.Name]
					eff[i] = model.PipPackage{Name: p.Name, Version: lock.InstallVersion(p.Version, e, ok, *update)}
				}
				resolved, err := pip.Install(eff, plan.PipDir, toolchainBinDirs, rep.Stream("pip: "), installReport)
				if err != nil {
					addErr(err.Error())
				}
				for i, p := range plan.PipPackages {
					if v, ok := resolved[p.Name]; ok {
						lk.Pip[p.Name] = lock.Entry{Requested: plan.PipPackages[i].Version, Resolved: v}
					}
				}
			}()
		}
		if len(plan.NpmPackages) > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				eff := make([]model.NpmPackage, len(plan.NpmPackages))
				for i, p := range plan.NpmPackages {
					e, ok := lk.Npm[p.Name]
					eff[i] = model.NpmPackage{Name: p.Name, Version: lock.InstallVersion(p.Version, e, ok, *update)}
				}
				resolved, err := npm.Install(eff, plan.NpmDir, toolchainBinDirs, rep.Stream("npm: "), installReport)
				if err != nil {
					addErr(err.Error())
				}
				for i, p := range plan.NpmPackages {
					if v, ok := resolved[p.Name]; ok {
						lk.Npm[p.Name] = lock.Entry{Requested: plan.NpmPackages[i].Version, Resolved: v}
					}
				}
			}()
		}
		if len(plan.UvPackages) > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				eff := make([]model.UvPackage, len(plan.UvPackages))
				for i, p := range plan.UvPackages {
					e, ok := lk.Uv[p.Name]
					eff[i] = model.UvPackage{Name: p.Name, Version: lock.InstallVersion(p.Version, e, ok, *update)}
				}
				resolved, err := uv.Install(eff, plan.UvDir, toolchainBinDirs, rep.Stream("uv: "), installReport)
				if err != nil {
					addErr(err.Error())
				}
				for i, p := range plan.UvPackages {
					if v, ok := resolved[p.Name]; ok {
						lk.Uv[p.Name] = lock.Entry{Requested: plan.UvPackages[i].Version, Resolved: v}
					}
				}
			}()
		}
		wg.Wait()
		miseBinDirs = append(toolchainBinDirs, restBinDirs...)
	}

	// GC: uninstall packages and prune lock entries that were removed from the
	// config. mise installs live in the shared store, so we only drop their lock
	// entries (mise manages its own store).
	gcTools(plan, lk, miseBinDirs, rep)

	// Persist the lockfile (best effort; it is committed with the project).
	if err := lk.Save(d.ProjectRoot); err != nil {
		toolErrs = append(toolErrs, "writing "+lock.FileName+": "+err.Error())
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

	// Record the config inputs (config file, loaded modules, fn.source files) so
	// the hook/staleness checks re-sync when any of them changes — not just the
	// active config file.
	sources := append([]string{plan.ConfigPath}, res.LoadedModules...)
	sources = append(sources, sourceFiles(plan)...)
	if err := state.WriteSources(plan.StateDir, sources); err != nil {
		return syncFail("writing sources: %v", err)
	}

	// Record tool dirs (mise store bin dirs + project-local pip/npm bins) so the
	// hook/staleness checks can detect if one is later removed (e.g. a mise
	// version pruned, or `rm -rf .taugres/tools/pip`) and trigger a resync.
	toolDirs := append(append([]string{}, miseBinDirs...), plan.ProjectToolBinDirs()...)
	if err := state.WriteToolDirs(plan.StateDir, toolDirs); err != nil {
		return syncFail("writing tool dirs: %v", err)
	}

	// Write the manifest last so it is the newest file: the staleness check
	// treats any recorded source newer than the manifest as "changed".
	m, err := buildManifest(res, render.SupportedShells)
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

func buildManifest(res *config.Result, shells []string) (*state.Manifest, error) {
	plan := res.Plan
	configHash, err := state.HashFile(plan.ConfigPath)
	if err != nil {
		return nil, err
	}
	moduleHashes := map[string]string{}
	for _, m := range res.LoadedModules {
		h, err := state.HashFile(m)
		if err != nil {
			return nil, err
		}
		moduleHashes[m] = h
	}
	sourceHashes := map[string]string{}
	for _, f := range sourceFiles(plan) {
		h, err := state.HashFile(f)
		if err != nil {
			return nil, err
		}
		sourceHashes[f] = h
	}
	var tools map[string]string
	if len(plan.MiseTools) > 0 {
		tools = map[string]string{}
		for _, t := range plan.MiseTools {
			tools[t.Name] = t.Version
		}
	}
	var pipPkgs map[string]string
	if len(plan.PipPackages) > 0 {
		pipPkgs = map[string]string{}
		for _, p := range plan.PipPackages {
			pipPkgs[p.Name] = p.Version
		}
	}
	var npmPkgs map[string]string
	if len(plan.NpmPackages) > 0 {
		npmPkgs = map[string]string{}
		for _, p := range plan.NpmPackages {
			npmPkgs[p.Name] = p.Version
		}
	}
	return &state.Manifest{
		TauVersion:   Version,
		ConfigPath:   plan.ConfigPath,
		RepoRoot:     plan.RepoRoot,
		ProjectRoot:  plan.ProjectRoot,
		ConfigHash:   configHash,
		ModuleHashes: moduleHashes,
		SourceHashes: sourceHashes,
		Shells:       state.SortedShells(shells),
		MiseTools:    tools,
		PipPackages:  pipPkgs,
		NpmPackages:  npmPkgs,
	}, nil
}

// gcTools removes packages and lock entries that were dropped from the config.
// PATH entries need no cleanup — they are regenerated from the current config.
// mise installs live in mise's shared store, so only their lock entries are
// pruned; pip/npm packages are uninstalled from their project-local prefixes.
func gcTools(plan *model.Plan, lk *lock.File, toolchainBins []string, rep *ui.Reporter) {
	// mise: prune lock entries for tools no longer declared.
	keep := map[string]bool{}
	for _, t := range plan.MiseTools {
		keep[t.Name] = true
	}
	for name := range lk.Mise {
		if !keep[name] {
			delete(lk.Mise, name)
		}
	}

	// pip: if no packages remain, drop the whole venv; otherwise uninstall the
	// packages that were removed.
	pipDir := filepath.Join(plan.StateDir, "tools", "pip")
	if len(plan.PipPackages) == 0 {
		_ = os.RemoveAll(pipDir)
		lk.Pip = map[string]lock.Entry{}
	} else if removed := removedKeys(lk.Pip, pipNames(plan)); len(removed) > 0 {
		rep.Step("tau: removing " + strings.Join(removed, ", "))
		_ = pip.Uninstall(pipDir, removed, rep.Stream("pip: "))
		for _, n := range removed {
			delete(lk.Pip, n)
		}
	}

	// npm: symmetric with pip.
	npmDir := filepath.Join(plan.StateDir, "tools", "npm")
	if len(plan.NpmPackages) == 0 {
		_ = os.RemoveAll(npmDir)
		lk.Npm = map[string]lock.Entry{}
	} else if removed := removedKeys(lk.Npm, npmNames(plan)); len(removed) > 0 {
		rep.Step("tau: removing " + strings.Join(removed, ", "))
		_ = npm.Uninstall(npmDir, removed, toolchainBins, rep.Stream("npm: "))
		for _, n := range removed {
			delete(lk.Npm, n)
		}
	}

	// uv: symmetric with pip.
	uvDir := filepath.Join(plan.StateDir, "tools", "uv")
	if len(plan.UvPackages) == 0 {
		_ = os.RemoveAll(uvDir)
		lk.Uv = map[string]lock.Entry{}
	} else if removed := removedKeys(lk.Uv, uvNames(plan)); len(removed) > 0 {
		rep.Step("tau: removing " + strings.Join(removed, ", "))
		_ = uv.Uninstall(uvDir, removed, toolchainBins, rep.Stream("uv: "))
		for _, n := range removed {
			delete(lk.Uv, n)
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

func pipNames(p *model.Plan) map[string]bool {
	m := map[string]bool{}
	for _, x := range p.PipPackages {
		m[x.Name] = true
	}
	return m
}

func npmNames(p *model.Plan) map[string]bool {
	m := map[string]bool{}
	for _, x := range p.NpmPackages {
		m[x.Name] = true
	}
	return m
}

func uvNames(p *model.Plan) map[string]bool {
	m := map[string]bool{}
	for _, x := range p.UvPackages {
		m[x.Name] = true
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
		if containsLine(string(data), ".taugres/") {
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

func containsLine(content, line string) bool {
	for _, l := range splitLines(content) {
		if l == line {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
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
		return fail(e, "usage: tau hook <shell> (bash|zsh)")
	}
	// Bake in the absolute path to this tau binary so the hook invokes exactly
	// this executable for auto-sync.
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

// --- activate ---

func runActivate(e *Env, args []string) int {
	if len(args) != 1 {
		return fail(e, "usage: tau activate <shell> (bash|zsh|fish)")
	}
	shell := args[0]
	if !slices.Contains(render.SupportedShells, shell) {
		return fail(e, "unsupported shell %q (supported: %s)", shell, strings.Join(render.SupportedShells, ", "))
	}
	d, err := discover.Discover(e.Wd)
	if err != nil {
		return fail(e, "%v", err)
	}

	// Trust gate: the shell hook sources this command's stdout, so refusing to
	// print the script for an untrusted project is the security boundary. Trust
	// lives outside the repo (a repo cannot forge it), so a freshly-cloned
	// project activates nothing until the user runs `tau allow` on this machine.
	allowed, err := trust.IsAllowed(d.ConfigPath)
	if err != nil {
		return fail(e, "checking trust: %v", err)
	}
	if !allowed {
		return fail(e, "project is not trusted; run `tau allow`")
	}

	stateDir := filepath.Join(d.ProjectRoot, ".taugres")
	activate := filepath.Join(state.GenDir(stateDir), "activate."+shell)
	data, err := os.ReadFile(activate)
	if err != nil {
		return fail(e, "no generated activation script for %s; run `tau sync`", shell)
	}
	// This runs on the activation hot path (every project entry). It deliberately
	// does no staleness check: the shell hook already re-syncs on change before
	// calling this, and `tau status` reports staleness. Just emit the script to
	// stdout so `eval "$(tau activate zsh)"` works.
	fmt.Fprint(e.Stdout, string(data))
	return 0
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
