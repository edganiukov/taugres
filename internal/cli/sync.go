package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/edganiukov/taugres/internal/atomicfile"
	"github.com/edganiukov/taugres/internal/config"
	"github.com/edganiukov/taugres/internal/discover"
	"github.com/edganiukov/taugres/internal/lock"
	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/render"
	shellreg "github.com/edganiukov/taugres/internal/shell"
	"github.com/edganiukov/taugres/internal/state"
	"github.com/edganiukov/taugres/internal/toolmgr"
	"github.com/edganiukov/taugres/internal/tools/mise"
	"github.com/edganiukov/taugres/internal/trust"
	"github.com/edganiukov/taugres/internal/ui"
	"github.com/edganiukov/taugres/internal/validate"
)

// --- sync ---

type syncOptions struct {
	ifStale      bool
	verbose      bool
	update       bool
	forced       map[string]bool
	updateTarget []string
}

func runSync(e *Env, args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	ifStale := fs.Bool("if-stale", false, "only sync if the config changed since the last sync (used by tau hook-env)")
	verbose := fs.Bool("verbose", false, "print every sync step instead of a single updating line")
	update := fs.Bool("update", false, "re-resolve unpinned tools/packages to their latest versions and update .taugres.lock")
	force := fs.Bool("force", false, "reinstall tools/packages even if unchanged; limit to named managers, e.g. `tau sync --force mise`")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --force reinstalls even when a manager looks fresh: with no names it forces
	// all managers, otherwise just the named ones (mise|pip|npm|uv). Positional
	// args are only meaningful with --force.
	forced := map[string]bool{}
	if *force {
		names := fs.Args()
		if len(names) == 0 {
			names = updManagers
		}
		for _, n := range names {
			if strings.HasPrefix(n, "-") {
				return fail(e, "sync: put flags before manager names, e.g. `tau sync --verbose --force mise` (got %q)", n)
			}
			if !slices.Contains(updManagers, n) {
				return fail(e, "sync --force: unknown manager %q (want one of %s)", n, strings.Join(updManagers, ", "))
			}
			forced[n] = true
		}
	} else if rest := fs.Args(); len(rest) > 0 {
		return fail(e, "sync: unexpected argument %q (did you mean `tau sync --force %s`?)", rest[0], strings.Join(rest, " "))
	}

	return syncProject(e, syncOptions{
		ifStale: *ifStale,
		verbose: *verbose,
		update:  *update,
		forced:  forced,
	})
}

// syncProject is the application service behind manual sync, hook auto-sync,
// exec auto-sync, and targeted update. Callers pass typed options rather than
// recursively invoking the CLI parser.
func syncProject(e *Env, opts syncOptions) int {
	if opts.forced == nil {
		opts.forced = map[string]bool{}
	}
	ctx := e.ctx() // cancelled on Ctrl+C so tool installs stop promptly

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
		if opts.ifStale {
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
	// process just finished while we waited. Cheap metadata is only a trigger and
	// does not verify generated scripts. Confirm freshness with hashes before
	// skipping.
	if opts.ifStale {
		need, err := state.NeedsSync(stateDir, d.ConfigPath)
		if err == nil {
			// The cheap mtime trigger also fires on a no-op touch (an editor save that
			// rewrites the file, `git checkout` bumping mtimes). If the thorough
			// hash/script/tool check says nothing actually changed, skip the whole
			// sync — no Starlark eval, no tool probing, no script regeneration. When
			// metadata did change, refresh it from stable snapshots. If a file races
			// that refresh, continue into a full sync instead of marking it fresh.
			if !state.CheckStale(stateDir, shellreg.Supported).Stale {
				if !need || state.TouchManifest(stateDir) == nil {
					return 0
				}
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

	resolvedPlan := model.ResolvePlan(res.Plan)
	plan := resolvedPlan.Plan
	rep := ui.NewReporter(e.Stderr, opts.verbose)
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
	type lockBackup struct {
		manager string
		name    string
		entry   lock.Entry
		present bool
	}
	var updateBackups []lockBackup
	for _, target := range opts.updateTarget {
		manager, name := splitManager(target)
		section := sectionOf(lk, manager)
		if manager == "" || name == "" || section == nil {
			return syncFail("invalid update target %q", target)
		}
		entry, present := section[name]
		updateBackups = append(updateBackups, lockBackup{manager: manager, name: name, entry: entry, present: present})
		delete(section, name)
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
	// A manifest written by a different tau build may embed differently-derived
	// state (bin dir resolution, script rendering), so don't trust it: treat
	// this as a first sync. Installs are not forced — present tools no-op — but
	// every manager re-derives its dirs and this build rewrites the scripts.
	if prior != nil && prior.TauBuild != state.BuildStamp() {
		prior = nil
	}
	fresh := freshness(prior, sig, plan, opts.update, opts.forced)

	var miseBinDirs, toolDirs []string
	failedManagers := map[string]bool{}
	if prior != nil && fresh.allFresh() {
		// Recover the mise store bin dirs (needed for PATH) from the recorded tool
		// dirs by dropping the package bin dirs, which are deterministic from the
		// plan — so no dir is cached twice.
		toolDirs = prior.ToolDirs
		miseBinDirs = miseBinDirsFrom(prior, plan)
	} else {
		miseBinDirs, toolDirs, failedManagers = installTools(ctx, plan, lk, rep, opts.update, opts.forced, addErr, fresh)
		// A targeted update is transactional per manager: if resolution/install
		// failed, preserve its previous lock entry rather than committing a hole.
		for _, backup := range updateBackups {
			if !failedManagers[backup.manager] {
				continue
			}
			section := sectionOf(lk, backup.manager)
			if backup.present {
				section[backup.name] = backup.entry
			} else {
				delete(section, backup.name)
			}
		}
		// The lock and manifest form one state transition. If the lock cannot be
		// committed, stop before writing a manifest that claims those versions are
		// fresh; the next sync will retry from the old lock.
		if err := lk.Save(d.ProjectRoot); err != nil {
			return syncFail("writing %s: %v", lock.FileName, err)
		}
		// Recompute from the post-install lock so the stored signatures reflect the
		// now-resolved versions. A failed manager deliberately gets no committed
		// signature and remains pending in the manifest, so normal sync retries it.
		sig = toolSigs(plan, lk)
		for mgr := range failedManagers {
			delete(sig, mgr)
		}
	}

	// If the user interrupted (Ctrl+C) during installs, stop here without writing
	// scripts or the manifest, so the environment is not marked fresh and the next
	// sync retries.
	interrupted := func() bool {
		if ctx.Err() == nil {
			return false
		}
		rep.Done()
		fmt.Fprintln(e.Stderr, "tau: sync interrupted")
		return true
	}
	if interrupted() {
		return 130
	}

	// Prepend mise tool bin dirs to the activation PATH (in front of the
	// project-local pip/npm dirs already in plan.PathPrepend).
	plan.PathPrepend = dedupStrings(append(append([]string{}, miseBinDirs...), plan.PathPrepend...))
	resolvedPlan.ManagerDirs = managerToolDirs(plan, miseBinDirs)

	// Resolve deferred env vars (shell.exec / mise.where). Sync is trust-gated and
	// tools are on PATH, so fully-static values are run/looked-up now and baked
	// into EnvSet; values with a dynamic exec have their static segments baked in
	// place and are rendered as command substitutions at activation. Best-effort:
	// a failure is reported but never aborts the sync.
	resolveDeferredEnvForSync(ctx, plan, lk, addErr)
	if interrupted() {
		return 130
	}

	rep.Step("tau: generating shell scripts")
	genDir := state.GenDir(plan.StateDir)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return syncFail("creating %s: %v", genDir, err)
	}

	for _, sh := range shellreg.Supported {
		act, err := render.Activate(resolvedPlan, sh)
		if err != nil {
			return syncFail("rendering activate.%s: %v", sh, err)
		}
		deact, err := render.Deactivate(resolvedPlan, sh)
		if err != nil {
			return syncFail("rendering deactivate.%s: %v", sh, err)
		}
		if err := atomicfile.Write(filepath.Join(genDir, "activate."+sh), []byte(act), 0o644); err != nil {
			return syncFail("writing activate.%s: %v", sh, err)
		}
		if err := atomicfile.Write(filepath.Join(genDir, "deactivate."+sh), []byte(deact), 0o644); err != nil {
			return syncFail("writing deactivate.%s: %v", sh, err)
		}
	}

	// Write the manifest last so it is the newest file: the staleness checks
	// treat any recorded input newer than the manifest as "changed". It records
	// config inputs (config file, loaded modules, fn/hook source files), the tool
	// bin dirs that must exist, and the exists()/which() probe results — so a
	// changed input, a removed tool dir, or a flipped probe all trigger a resync.
	m, err := buildManifest(res, toolDirs, resolvedPlan.ManagerDirs, sig, sortedSet(failedManagers))
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
	if !opts.ifStale {
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
func buildManifest(res *config.Result, toolDirs []string, managerDirs map[string][]string, toolSig map[string]string, pendingManagers []string) (*state.Manifest, error) {
	plan := res.Plan
	inputs := make(map[string]string, len(res.InputHashes)+2)
	metadata := make(map[string]state.InputMetadata, len(res.InputMetadata)+2)
	for path, hash := range res.InputHashes {
		inputs[path] = hash
	}
	for path, value := range res.InputMetadata {
		metadata[path] = value
	}
	add := func(path string) error {
		if _, ok := inputs[path]; ok {
			return nil
		}
		_, hash, value, err := state.ReadInput(path)
		if err != nil {
			return err
		}
		inputs[path] = hash
		metadata[path] = value
		return nil
	}
	if err := add(plan.ConfigPath); err != nil {
		return nil, err
	}
	// A committed lock changed by git checkout or another developer must trigger
	// the same auto-sync as a config edit.
	if err := add(lock.Path(plan.ProjectRoot)); err != nil {
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
	// shell.dotenv(...) and read(...) files are config inputs too: editing one
	// changes the resulting env.
	for _, f := range res.DotenvFiles {
		if err := add(f); err != nil {
			return nil, err
		}
	}
	for _, f := range res.ReadFiles {
		if err := add(f); err != nil {
			return nil, err
		}
	}
	return &state.Manifest{
		Inputs:          inputs,
		InputMetadata:   metadata,
		ToolDirs:        toolDirs,
		ManagerDirs:     managerDirs,
		Probes:          res.Probes,
		ToolSig:         toolSig,
		PendingManagers: pendingManagers,
	}, nil
}

// packageBinDirs returns the pip/npm/uv bin dirs recorded in the tool-dir list —
// one per manager that has packages. Unlike the mise store dirs (resolved via
// `mise where`), these are deterministic from the plan, so they are not cached
// separately: the fast path recovers the mise dirs by removing these from the
// recorded tool dirs.
func packageBinDirs(plan *model.Plan) []string {
	var dirs []string
	for _, manager := range toolmgr.PackageManagers {
		if len(manager.Packages(plan)) > 0 {
			dirs = append(dirs, manager.BinDir(plan))
		}
	}
	return dirs
}

// managerToolDirs records tool-directory ownership explicitly in the manifest.
func managerToolDirs(plan *model.Plan, miseDirs []string) map[string][]string {
	dirs := map[string][]string{}
	if len(miseDirs) > 0 {
		dirs[toolmgr.Mise] = append([]string(nil), miseDirs...)
	}
	for _, manager := range toolmgr.PackageManagers {
		if len(manager.Packages(plan)) > 0 {
			dirs[manager.ID] = []string{manager.BinDir(plan)}
		}
	}
	return dirs
}

// miseBinDirsFrom reads explicit ownership from new manifests and falls back to
// subtracting deterministic package dirs for legacy manifests.
func miseBinDirsFrom(manifest *state.Manifest, plan *model.Plan) []string {
	if dirs, ok := manifest.ManagerDirs[toolmgr.Mise]; ok {
		return append([]string(nil), dirs...)
	}
	pkg := map[string]bool{}
	for _, dir := range packageBinDirs(plan) {
		pkg[dir] = true
	}
	var out []string
	for _, dir := range manifest.ToolDirs {
		if !pkg[dir] {
			out = append(out, dir)
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
	var miseLines []string
	for _, tool := range plan.MiseTools {
		miseLines = append(miseLines, line(tool.Name, tool.Version, lk.Mise))
	}
	hash(toolmgr.Mise, miseLines)
	for _, manager := range toolmgr.PackageManagers {
		var lines []string
		section := manager.Section(lk)
		for _, pkg := range manager.Packages(plan) {
			lines = append(lines, line(pkg.Name, pkg.Version, section))
		}
		hash(manager.ID, lines)
	}
	return sigs
}

// toolFreshness records, for one sync, which managers are already up to date (so
// their install is skipped) plus the mise store bin dirs cached from the last
// sync — reused for PATH when mise is fresh, so no `mise where` probe is needed.
type toolFreshness struct {
	stale    map[string]bool
	miseDirs []string // cached mise bin dirs, valid when mise is fresh
}

// allFresh reports whether no manager needs installing, so the whole install
// phase can be skipped and only the shell scripts regenerated.
func (f toolFreshness) allFresh() bool {
	for _, manager := range toolmgr.All {
		if f.stale[manager] {
			return false
		}
	}
	return true
}

// freshness compares the current per-manager signatures against the last sync's
// to decide which managers must reinstall. A manager is stale when --update is
// set, it was added or dropped since the last sync, its signature changed, or
// its recorded bin dirs are missing. The mise store dirs are recovered from the
// prior manifest (so freshness needs no `mise where`).
func freshness(prior *state.Manifest, cur map[string]string, plan *model.Plan, update bool, forced map[string]bool) toolFreshness {
	var priorSig map[string]string
	var miseDirs []string
	pending := map[string]bool{}
	if prior != nil {
		priorSig = prior.ToolSig
		miseDirs = miseBinDirsFrom(prior, plan)
		for _, manager := range prior.PendingManagers {
			pending[manager] = true
		}
	}
	// A manager is stale when it was declared before XOR now (added/dropped), or —
	// when declared both times — --update or --force targets it, its signature
	// changed, or its dirs vanished. A manager declared in neither sync is fresh.
	stale := func(mgr string, declared bool, dirs []string) bool {
		if pending[mgr] {
			return true
		}
		_, was := priorSig[mgr]
		if declared != was {
			return true
		}
		if !declared {
			return false
		}
		return update || forced[mgr] || cur[mgr] != priorSig[mgr] || !allExist(dirs)
	}
	result := toolFreshness{stale: map[string]bool{}, miseDirs: miseDirs}
	result.stale[toolmgr.Mise] = stale(toolmgr.Mise, len(plan.MiseTools) > 0, miseDirs)
	for _, manager := range toolmgr.PackageManagers {
		result.stale[manager.ID] = stale(manager.ID, len(manager.Packages(plan)) > 0, []string{manager.BinDir(plan)})
	}
	return result
}

// installTools runs the per-tool staleness + install pipeline: it installs only
// the managers whose declared set changed (or all, when force), GCs tools
// dropped from the config, and returns the mise store bin dirs (prepended to
// PATH) plus the full set of tool bin dirs (recorded for staleness). Install
// failures are reported via addErr rather than aborting, so the shell env is
// always built. It may hit the network and belongs to sync, never activation.
func installTools(ctx context.Context, plan *model.Plan, lk *lock.File, rep *ui.Reporter, update bool, forced map[string]bool, addErr func(string), fresh toolFreshness) (miseBinDirs, toolDirs []string, failedManagers map[string]bool) {
	failedManagers = map[string]bool{}
	var failedMu sync.Mutex
	managerFailed := func(manager string, err error) {
		failedMu.Lock()
		failedManagers[manager] = true
		failedMu.Unlock()
		addErr(err.Error())
	}
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
		managerFailed("mise", errors.New("mise is required to install tools but is not installed — install it with `curl https://mise.run | sh` (see https://mise.jdx.dev; the mise binary on PATH is all tau needs)"))
	case !fresh.stale[toolmgr.Mise]:
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
			effMise[i] = model.MiseTool{Name: t.Name, Version: lock.InstallVersion(t.Version, e, ok, update)}
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
		requirements := map[string]bool{}
		for _, manager := range toolmgr.PackageManagers {
			if len(manager.Packages(plan)) == 0 {
				continue
			}
			for _, requirement := range manager.Requirements {
				requirements[requirement] = true
			}
		}
		var toolchain, rest []model.MiseTool
		for _, tool := range effMise {
			if requirements[tool.Name] {
				toolchain = append(toolchain, tool)
			} else {
				rest = append(rest, tool)
			}
		}

		// --force mise reinstalls even already-present store versions.
		miseForce := forced["mise"]
		if len(toolchain) > 0 {
			installed, err := mise.Install(ctx, toolchain, plan.MiseJobs, miseForce, rep.Stream("mise: "), installReport)
			if err != nil {
				managerFailed("mise", err)
			}
			toolchainBinDirs = recordMise(installed)
		}
		// The rest of the mise tools install concurrently with pip/npm/uv below.
		if len(rest) > 0 {
			wg.Go(func() {
				installed, err := mise.Install(ctx, rest, plan.MiseJobs, miseForce, rep.Stream("mise: "), installReport)
				if err != nil {
					managerFailed("mise", err)
				}
				restBinDirs = recordMise(installed)
			})
		}
	}

	// Build runtime manager adapters from the compile-time registry. Adding a
	// package integration extends the registry rather than every sync concern.
	managers := make([]packageManager, 0, len(toolmgr.PackageManagers))
	for _, descriptor := range toolmgr.PackageManagers {
		descriptor := descriptor
		dir := descriptor.Dir(plan)
		managers = append(managers, packageManager{
			label:   descriptor.ID,
			stale:   fresh.stale[descriptor.ID],
			pkgs:    descriptor.Packages(plan),
			dir:     dir,
			section: descriptor.Section(lk),
			install: func(packages []model.Package) (map[string]string, error) {
				return descriptor.Install(ctx, packages, dir, toolchainBinDirs, rep.Stream(descriptor.ID+": "), installReport)
			},
			uninstall: func(names []string) error {
				return descriptor.Uninstall(ctx, dir, names, toolchainBinDirs, rep.Stream(descriptor.ID+": "))
			},
		})
	}
	// Install stale managers concurrently (with the rest of mise, above). They use
	// the toolchain bin dirs to find python/node/uv, so never wait on other tools.
	for i := range managers {
		m := managers[i]
		if !m.stale {
			continue
		}
		wg.Go(func() {
			// --force <mgr> does a clean reinstall: tau owns these prefixes, so wipe
			// and rebuild from the (locked) versions.
			if forced[m.label] {
				_ = os.RemoveAll(m.dir)
			}
			resolved, err := m.install(effectiveVersions(m.pkgs, m.section, update))
			if err == nil {
				for _, pkg := range m.pkgs {
					if resolved[pkg.Name] == "" {
						err = fmt.Errorf("%s: could not determine installed version of %s", m.label, pkg.Name)
						break
					}
				}
			}
			if err != nil {
				managerFailed(m.label, err)
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
	gcTools(plan, lk, managers, rep, managerFailed)

	// Tool bin dirs recorded for staleness: mise store bins followed by each
	// package manager's prefix bin (present only when it has packages). The order
	// and derivation must match miseBinDirsFrom, which recovers the mise subset by
	// removing the package bin dirs.
	toolDirs = append(append([]string{}, miseBinDirs...), packageBinDirs(plan)...)
	return miseBinDirs, toolDirs, failedManagers
}

// gcTools removes packages and lock entries that were dropped from the config.
// PATH entries need no cleanup — they are regenerated from the current config.
// mise installs live in mise's shared store, so only their lock entries are
// pruned; pip/npm packages are uninstalled from their project-local prefixes.
func gcTools(plan *model.Plan, lk *lock.File, managers []packageManager, rep *ui.Reporter, managerFailed func(string, error)) {
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
			if err := os.RemoveAll(m.dir); err != nil {
				managerFailed(m.label, fmt.Errorf("removing %s prefix: %w", m.label, err))
				continue
			}
			clear(m.section)
			continue
		}
		removed := removedKeys(m.section, nameSet(m.pkgs, func(p model.Package) string { return p.Name }))
		if len(removed) > 0 {
			rep.Step("tau: removing " + strings.Join(removed, ", "))
			if err := m.uninstall(removed); err != nil {
				managerFailed(m.label, err)
				continue
			}
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

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
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
		_ = atomicfile.Write(gi, []byte(".taugres/\n"), 0o644)
	}
}
