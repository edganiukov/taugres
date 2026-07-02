// Package config evaluates Taugres `.tg` files (Starlark) into a normalized
// plan. Builtins mutate only the in-memory plan; Starlark has no ambient access
// to the filesystem, network, or shell.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"go.gnkv.dev/taugres/internal/discover"
	"go.gnkv.dev/taugres/internal/model"
	"go.gnkv.dev/taugres/internal/paths"
)

// Result is the output of evaluating a config, including the modules that were
// loaded (for stale detection).
type Result struct {
	Plan          *model.Plan
	LoadedModules []string // absolute paths of loaded .tg modules
}

// Evaluate evaluates the active config file into a normalized plan. Imports may
// be root-anchored (//…) or relative (./ ../) to the importing file.
func Evaluate(d *discover.Discovery) (*Result, error) {
	b := newBuilder(d)

	src, err := os.ReadFile(d.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	thread := &starlark.Thread{
		Name: "tau",
		Load: b.loaderFor(filepath.Dir(d.ConfigPath)),
	}
	if _, err := starlark.ExecFileOptions(fileOptions(), thread, d.ConfigPath, src, b.predeclared()); err != nil {
		return nil, decorateStarlarkErr(err)
	}

	plan, err := b.finalize()
	if err != nil {
		return nil, err
	}

	return &Result{Plan: plan, LoadedModules: b.loadedList()}, nil
}

func fileOptions() *syntax.FileOptions {
	// Allow common ergonomic features in config files.
	return &syntax.FileOptions{
		Set:             true,
		While:           false,
		TopLevelControl: true,
		GlobalReassign:  false,
	}
}

// builder accumulates plan state during Starlark evaluation.
type builder struct {
	disc *discover.Discovery

	projectName string

	envSet   map[string]string
	envUnset []string

	pathPrepend []string
	pathAppend  []string

	miseTools   []model.MiseTool
	pipPackages []model.PipPackage
	npmPackages []model.NpmPackage

	aliases     map[string]string
	sourceFuncs map[string][]model.SourceFunc
	hooks       []model.HookScript
	loaded      map[string]bool
	loadCache   map[string]*loadEntry
}

type loadEntry struct {
	globals starlark.StringDict
	err     error
}

func newBuilder(d *discover.Discovery) *builder {
	return &builder{
		disc:        d,
		envSet:      map[string]string{},
		aliases:     map[string]string{},
		sourceFuncs: map[string][]model.SourceFunc{},
		loaded:      map[string]bool{},
		loadCache:   map[string]*loadEntry{},
	}
}

func (b *builder) loadedList() []string {
	out := make([]string, 0, len(b.loaded))
	for p := range b.loaded {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// predeclared returns the global environment exposed to config files.
func (b *builder) predeclared() starlark.StringDict {
	// All shell-facing configuration is grouped under the `shell` namespace.
	shellPath := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"prepend": b.builtin("shell.path.prepend", b.pathPrependFn),
		"append":  b.builtin("shell.path.append", b.pathAppendFn),
	})
	shellModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"env":   b.builtin("shell.env", b.envFn),
		"unset": b.builtin("shell.unset", b.unsetFn),
		"alias": b.builtin("shell.alias", b.aliasFn),
		"path":  shellPath,
		"fn":    b.builtin("shell.fn", b.fnSourceFn),
		"hook":  b.builtin("shell.hook", b.hookFn),
	})

	miseModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"tool": b.builtin("mise.tool", b.miseToolFn),
	})

	pipModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"install": b.builtin("pip.install", b.pipInstallFn),
	})

	npmModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"install": b.builtin("npm.install", b.npmInstallFn),
	})

	platformModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"os":   starlark.String(platformOS()),
		"arch": starlark.String(platformArch()),
	})

	return starlark.StringDict{
		"project":  b.builtin("project", b.projectFn),
		"shell":    shellModule,
		"mise":     miseModule,
		"pip":      pipModule,
		"npm":      npmModule,
		"platform": platformModule,
	}
}

func (b *builder) builtin(name string, fn func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error)) *starlark.Builtin {
	return starlark.NewBuiltin(name, fn)
}

// --- builtin implementations ---

func (b *builder) projectFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &name); err != nil {
		return nil, err
	}
	b.projectName = name
	return starlark.None, nil
}

func (b *builder) envFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, value string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &name, "value", &value); err != nil {
		return nil, err
	}
	// Expand $VAR / ${VAR} references, preferring vars set earlier in this
	// config, then the process environment.
	b.envSet[name] = os.Expand(value, b.envLookup)
	return starlark.None, nil
}

// envLookup resolves a variable name for os.Expand: earlier env() values win
// over the ambient process environment.
func (b *builder) envLookup(key string) string {
	if v, ok := b.envSet[key]; ok {
		return v
	}
	return os.Getenv(key)
}

func (b *builder) unsetFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &name); err != nil {
		return nil, err
	}
	b.envUnset = append(b.envUnset, name)
	return starlark.None, nil
}

func (b *builder) aliasFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, value string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &name, "value", &value); err != nil {
		return nil, err
	}
	b.aliases[name] = value
	return starlark.None, nil
}

func (b *builder) pathPrependFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var entry string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "entry", &entry); err != nil {
		return nil, err
	}
	resolved, err := b.resolvePath(entry)
	if err != nil {
		return nil, err
	}
	b.pathPrepend = append(b.pathPrepend, resolved)
	return starlark.None, nil
}

func (b *builder) pathAppendFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var entry string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "entry", &entry); err != nil {
		return nil, err
	}
	resolved, err := b.resolvePath(entry)
	if err != nil {
		return nil, err
	}
	b.pathAppend = append(b.pathAppend, resolved)
	return starlark.None, nil
}

func (b *builder) fnSourceFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, file, content string
	var shellsV *starlark.List
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"name", &name, "shells", &shellsV, "file?", &file, "content?", &content); err != nil {
		return nil, err
	}
	shells, err := stringList(shellsV)
	if err != nil {
		return nil, fmt.Errorf("%s shells: %w", fn.Name(), err)
	}
	if (file == "") == (content == "") {
		return nil, fmt.Errorf("%s %q: provide exactly one of file= or content=", fn.Name(), name)
	}

	sf := model.SourceFunc{Name: name, Shells: shells}
	if file != "" {
		resolved, err := b.resolvePath(file)
		if err != nil {
			return nil, err
		}
		sf.File = resolved
	} else {
		sf.Content = content
	}
	b.sourceFuncs[name] = append(b.sourceFuncs[name], sf)
	return starlark.None, nil
}

// hookFn implements shell.hook(shells=[...], content=... | file=...): a raw
// activation snippet, mirroring shell.fn but without a name and run inline at
// activation instead of being wrapped in a function.
func (b *builder) hookFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var file, content string
	var shellsV *starlark.List
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"shells", &shellsV, "file?", &file, "content?", &content); err != nil {
		return nil, err
	}
	shells, err := stringList(shellsV)
	if err != nil {
		return nil, fmt.Errorf("%s shells: %w", fn.Name(), err)
	}
	if (file == "") == (content == "") {
		return nil, fmt.Errorf("%s: provide exactly one of file= or content=", fn.Name())
	}
	h := model.HookScript{Shells: shells}
	if file != "" {
		resolved, err := b.resolvePath(file)
		if err != nil {
			return nil, err
		}
		h.File = resolved
	} else {
		h.Content = content
	}
	b.hooks = append(b.hooks, h)
	return starlark.None, nil
}

func (b *builder) miseToolFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	specs, err := unpackSpecs(fn.Name(), args, kwargs)
	if err != nil {
		return nil, err
	}
	for _, s := range specs {
		b.miseTools = append(b.miseTools, model.MiseTool{Name: s.name, Version: s.version})
	}
	return starlark.None, nil
}

func (b *builder) pipInstallFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	specs, err := unpackSpecs(fn.Name(), args, kwargs)
	if err != nil {
		return nil, err
	}
	for _, s := range specs {
		b.pipPackages = append(b.pipPackages, model.PipPackage{Name: s.name, Version: s.version})
	}
	return starlark.None, nil
}

func (b *builder) npmInstallFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	specs, err := unpackSpecs(fn.Name(), args, kwargs)
	if err != nil {
		return nil, err
	}
	for _, s := range specs {
		b.npmPackages = append(b.npmPackages, model.NpmPackage{Name: s.name, Version: s.version})
	}
	return starlark.None, nil
}

// nameVersion is a parsed tool/package spec.
type nameVersion struct{ name, version string }

// unpackSpecs accepts either a single "name@version" spec or a list of them
// (bare "name" = latest), shared by mise.tool/pip.install/npm.install. Versions
// use "@" uniformly in config; each manager translates to its native ref at
// install time.
func unpackSpecs(fnName string, args starlark.Tuple, kwargs []starlark.Tuple) ([]nameVersion, error) {
	var spec starlark.Value
	if err := starlark.UnpackArgs(fnName, args, kwargs, "spec", &spec); err != nil {
		return nil, err
	}
	switch t := spec.(type) {
	case starlark.String:
		name, ver := splitSpec(string(t))
		return []nameVersion{{name, ver}}, nil
	case *starlark.List:
		out := make([]nameVersion, 0, t.Len())
		it := t.Iterate()
		defer it.Done()
		var e starlark.Value
		for it.Next(&e) {
			s, ok := starlark.AsString(e)
			if !ok {
				return nil, fmt.Errorf("%s: list entries must be strings, got %s", fnName, e.Type())
			}
			name, ver := splitSpec(s)
			out = append(out, nameVersion{name, ver})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s: expected a string or list of strings, got %s", fnName, spec.Type())
	}
}

// splitSpec splits "name@version" on the last "@" whose index is > 0, so npm
// scoped names like "@scope/pkg" (leading "@") stay intact. Bare names return
// an empty version.
func splitSpec(s string) (name, version string) {
	if i := strings.LastIndex(s, "@"); i > 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// hasMiseTool reports whether tools already declares a tool with the given name.
func hasMiseTool(tools []model.MiseTool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// resolvePath resolves a Taugres path against the repo root, returning a
// friendly error on failure.
func (b *builder) resolvePath(p string) (string, error) {
	resolved, err := paths.Resolve(p, b.disc.RepoRoot)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// loaderFor returns a Starlark load(...) resolver for a file located in baseDir.
// Relative imports (./ ../) resolve against baseDir; root-anchored (//…) against
// the repo root.
func (b *builder) loaderFor(baseDir string) func(*starlark.Thread, string) (starlark.StringDict, error) {
	return func(_ *starlark.Thread, module string) (starlark.StringDict, error) {
		return b.loadModule(module, baseDir)
	}
}

// resolveModuleLoad turns a load() argument into an absolute file path, given
// the importing file's directory.
func (b *builder) resolveModuleLoad(module, baseDir string) (string, error) {
	switch {
	case strings.HasPrefix(module, "//"):
		return paths.Resolve(module, b.disc.RepoRoot)
	case strings.HasPrefix(module, "./") || strings.HasPrefix(module, "../"):
		return filepath.Clean(filepath.Join(baseDir, filepath.FromSlash(module))), nil
	case strings.HasPrefix(module, "http://") || strings.HasPrefix(module, "https://"):
		return "", fmt.Errorf("remote imports are not supported yet: %q", module)
	case strings.HasPrefix(module, "@"):
		return "", fmt.Errorf("built-in module imports are not supported yet: %q", module)
	default:
		return "", fmt.Errorf("load path %q must be root-anchored (//…) or relative (./ ../)", module)
	}
}

// loadModule resolves, caches, and executes a loaded module.
func (b *builder) loadModule(module, baseDir string) (starlark.StringDict, error) {
	abs, err := b.resolveModuleLoad(module, baseDir)
	if err != nil {
		return nil, err
	}

	if e, ok := b.loadCache[abs]; ok {
		if e == nil {
			return nil, fmt.Errorf("cycle detected loading %q", module)
		}
		return e.globals, e.err
	}
	// Mark in-progress to detect cycles.
	b.loadCache[abs] = nil

	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("loading module %q: %w", module, err)
	}
	b.loaded[abs] = true

	// Relative imports within the module resolve against its own directory.
	modThread := &starlark.Thread{Name: "tau:" + module, Load: b.loaderFor(filepath.Dir(abs))}
	globals, err := starlark.ExecFileOptions(fileOptions(), modThread, abs, src, b.predeclared())
	entry := &loadEntry{globals: globals, err: err}
	b.loadCache[abs] = entry
	return globals, err
}

// finalize resolves the accumulated state into a normalized plan, applying
// PATH ordering and dedup rules.
func (b *builder) finalize() (*model.Plan, error) {
	p := model.NewPlan()
	p.RepoRoot = b.disc.RepoRoot
	p.ProjectRoot = b.disc.ProjectRoot
	p.ConfigPath = b.disc.ConfigPath
	p.StateDir = filepath.Join(b.disc.ProjectRoot, ".taugres")
	p.ProjectName = b.projectName

	p.EnvSet = b.envSet
	p.EnvUnset = b.envUnset
	p.Aliases = b.aliases
	p.SourceFuncs = b.sourceFuncs
	p.Hooks = b.hooks
	p.PipPackages = b.pipPackages
	p.NpmPackages = b.npmPackages

	// pip/npm run on a toolchain that tau provisions via mise, so declaring
	// packages implies the matching runtime: pip -> python, npm -> node. Add
	// these implicitly unless the user already pinned them.
	tools := append([]model.MiseTool{}, b.miseTools...)
	if len(b.pipPackages) > 0 && !hasMiseTool(tools, "python") {
		tools = append(tools, model.MiseTool{Name: "python"})
	}
	if len(b.npmPackages) > 0 && !hasMiseTool(tools, "node") {
		tools = append(tools, model.MiseTool{Name: "node"})
	}
	p.MiseTools = tools

	// Tool bin directories are auto-prepended so their executables resolve first
	// on PATH. pip and npm install into per-tool prefixes under
	// <stateDir>/tools/{pip,npm} with deterministic paths, prepended here. mise
	// tool bin dirs live in mise's store and are prepended at sync time (their
	// versioned paths are only known after installation), the way `mise
	// activate` exposes tools — no symlink/wrapper farm.
	var prepend []string
	if len(b.pipPackages) > 0 {
		p.PipDir = filepath.Join(p.StateDir, "tools", "pip")
		prepend = append(prepend, filepath.Join(p.PipDir, "bin"))
	}
	if len(b.npmPackages) > 0 {
		p.NpmDir = filepath.Join(p.StateDir, "tools", "npm")
		prepend = append(prepend, filepath.Join(p.NpmDir, "bin"))
	}
	prepend = append(prepend, b.pathPrepend...)

	// PATH otherwise preserves user order; entries are de-duplicated.
	p.PathPrepend = dedup(prepend)
	p.PathAppend = dedup(b.pathAppend)

	return p, nil
}

func dedup(in []string) []string {
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

func stringList(l *starlark.List) ([]string, error) {
	if l == nil {
		return nil, fmt.Errorf("expected a list of strings, got None")
	}
	out := make([]string, 0, l.Len())
	iter := l.Iterate()
	defer iter.Done()
	var v starlark.Value
	for iter.Next(&v) {
		s, ok := starlark.AsString(v)
		if !ok {
			return nil, fmt.Errorf("expected string, got %s", v.Type())
		}
		out = append(out, s)
	}
	return out, nil
}

func platformOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}

func platformArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

func decorateStarlarkErr(err error) error {
	if evalErr, ok := err.(*starlark.EvalError); ok {
		return fmt.Errorf("%s", evalErr.Backtrace())
	}
	return err
}
