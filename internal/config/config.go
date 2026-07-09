// Package config evaluates Taugres `.tg` files (Starlark) into a normalized
// plan. Builtins mutate only the in-memory plan and cannot run commands or write
// anything. The only host access is read-only probing for conditional config:
// exists(path) checks the filesystem and which(name) checks PATH.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"github.com/edganiukov/taugres/internal/discover"
	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/paths"
)

// defaultMiseJobs caps how many tools mise installs in parallel. A modest cap
// avoids bursts of unauthenticated GitHub API calls (aqua/ubi backends) that
// trigger rate limits; override with mise.jobs(n).
const defaultMiseJobs = 16

// Result is the output of evaluating a config, including the modules that were
// loaded (for stale detection).
type Result struct {
	Plan          *model.Plan
	LoadedModules []string      // absolute paths of loaded .tg modules
	DotenvFiles   []string      // absolute paths of shell.dotenv(...) files (config inputs)
	ReadFiles     []string      // absolute paths of read(...) files (config inputs)
	Probes        []model.Probe // exists()/which()/env() observations, for stale detection
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

	return &Result{Plan: plan, LoadedModules: b.loadedList(), DotenvFiles: b.dotenvFiles, ReadFiles: b.readFiles, Probes: b.probes}, nil
}

type loadEntry struct {
	globals starlark.StringDict
	err     error
}

// builder accumulates plan state during Starlark evaluation.
type builder struct {
	disc *discover.Discovery

	projectName string

	envSet      map[string]string
	envUnset    []string
	deferredEnv []model.DeferredEnv

	pathPrepend []string
	pathAppend  []string

	miseTools   []model.MiseTool
	miseJobs    int
	pipPackages []model.Package
	npmPackages []model.Package
	uvPackages  []model.Package

	aliases     map[string]string
	sourceFuncs map[string][]model.SourceFunc
	hooks       []model.HookScript
	loaded      map[string]bool
	loadCache   map[string]*loadEntry

	// dotenvFiles records shell.dotenv(...) file paths (deduped) so sync can hash
	// them as config inputs and re-sync when they change.
	dotenvFiles []string
	dotenvSeen  map[string]bool

	// readFiles records read(...) file paths (deduped), tracked as config inputs
	// so editing a read file re-syncs.
	readFiles []string
	readSeen  map[string]bool

	// probes records exists()/which()/env() observations for stale detection.
	probes    []model.Probe
	probeSeen map[string]bool
}

func newBuilder(d *discover.Discovery) *builder {
	return &builder{
		disc:        d,
		envSet:      map[string]string{},
		aliases:     map[string]string{},
		sourceFuncs: map[string][]model.SourceFunc{},
		loaded:      map[string]bool{},
		loadCache:   map[string]*loadEntry{},
		dotenvSeen:  map[string]bool{},
		readSeen:    map[string]bool{},
		probeSeen:   map[string]bool{},
		miseJobs:    defaultMiseJobs,
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
		"env":    b.builtin("shell.env", b.envFn),
		"unset":  b.builtin("shell.unset", b.unsetFn),
		"alias":  b.builtin("shell.alias", b.aliasFn),
		"path":   shellPath,
		"fn":     b.builtin("shell.fn", b.fnSourceFn),
		"hook":   b.builtin("shell.hook", b.hookFn),
		"dotenv": b.builtin("shell.dotenv", b.dotenvFn),
		"exec":   b.builtin("shell.exec", b.execFn),
	})

	miseModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"tool":  b.builtin("mise.tool", b.miseToolFn),
		"jobs":  b.builtin("mise.jobs", b.miseJobsFn),
		"where": b.builtin("mise.where", b.miseWhereFn),
	})

	pipModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"install": b.builtin("pip.install", b.pipInstallFn),
	})

	uvModule := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"install": b.builtin("uv.install", b.uvInstallFn),
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
		"exists":   b.builtin("exists", b.existsFn),
		"which":    b.builtin("which", b.whichFn),
		"env":      b.builtin("env", b.envProbeFn),
		"read":     b.builtin("read", b.readFn),
		"shell":    shellModule,
		"mise":     miseModule,
		"pip":      pipModule,
		"uv":       uvModule,
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

// existsFn implements exists(path): report whether a root-anchored ("//…") or
// absolute path exists on disk (file or directory). Handy for conditional
// config, e.g. `if exists("//go.mod"): mise.tool("go")`.
//
// Note: this makes evaluation depend on the host filesystem, so a config using
// it is only as reproducible as the paths it probes.
func (b *builder) existsFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "path", &path); err != nil {
		return nil, err
	}
	resolved, err := b.resolvePath(path)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Stat(resolved)
	ok := statErr == nil
	b.recordProbe("exists", resolved, boolResult(ok))
	return starlark.Bool(ok), nil
}

// whichFn implements which(name): return the absolute path of an executable
// found on PATH, or None if it is not present. Composes as a truthy check
// (`if which("git"): …`) while also exposing the resolved path.
//
// Like exists(), this depends on the host environment (the PATH in effect when
// tau runs), so use it as an escape hatch, not for reproducible pins.
func (b *builder) whichFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &name); err != nil {
		return nil, err
	}

	path, err := exec.LookPath(name)
	if err != nil {
		b.recordProbe("which", name, "")
		return starlark.None, nil
	}

	b.recordProbe("which", name, path)
	return starlark.String(path), nil
}

// envProbeFn implements env(name, default=""): read a process environment
// variable for conditional config, returning its value, or default when the
// variable is unset. Composes as a truthy check (`if env("CI"): …`) since an
// unset/empty value is falsy.
//
// Like exists()/which() it is a read-only probe recorded for stale detection, so
// changing the observed variable re-syncs on the next prompt. The recorded value
// is hashed (see model.EnvProbeResult), so secrets never land in the manifest.
// Beware probing a variable tau itself mutates (e.g. PATH): it would flip on
// every activation and re-sync in a loop.
func (b *builder) envProbeFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, def string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &name, "default?", &def); err != nil {
		return nil, err
	}
	val, ok := os.LookupEnv(name)
	b.recordProbe("env", name, model.EnvProbeResult(val, ok))
	if !ok {
		return starlark.String(def), nil
	}
	return starlark.String(val), nil
}

// readFn implements read(path, default=...): return the contents of a file at a
// root-anchored ("//…") or absolute path, as a string, for use anywhere in the
// config. It only *reads* (no code runs), so it is safe during evaluation. The
// file's presence is recorded as an exists() probe (so it appearing/disappearing
// re-syncs) and, when present, tracked as a config input (so editing it
// re-syncs). A missing file returns default if one is given, else it is an error.
// Contents are returned raw; use .strip() to drop a trailing newline.
func (b *builder) readFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	var def starlark.Value
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "path", &path, "default?", &def); err != nil {
		return nil, err
	}
	resolved, err := b.resolvePath(path)
	if err != nil {
		return nil, err
	}
	data, readErr := os.ReadFile(resolved)
	b.recordProbe("exists", resolved, boolResult(readErr == nil))
	if readErr != nil {
		if s, ok := starlark.AsString(def); ok {
			return starlark.String(s), nil
		}
		return nil, fmt.Errorf("%s: %v", fn.Name(), readErr)
	}
	if !b.readSeen[resolved] {
		b.readSeen[resolved] = true
		b.readFiles = append(b.readFiles, resolved)
	}
	return starlark.String(data), nil
}

// dotenvFn implements shell.dotenv(path): load KEY=VALUE pairs from a .env file
// into the environment, as if each were a shell.env(...). The path is
// root-anchored (//…) or absolute. Values are taken literally (no $VAR
// expansion), so secrets containing $ round-trip unchanged; wrap a value in
// single or double quotes to include surrounding spaces. The file is a config
// input — editing it triggers a resync — and it must exist (a missing file is an
// error, surfaced at evaluation).
func (b *builder) dotenvFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "path", &path); err != nil {
		return nil, err
	}

	resolved, err := b.resolvePath(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}

	pairs, err := parseDotenv(data)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", fn.Name(), resolved, err)
	}

	for _, kv := range pairs {
		b.envSet[kv.key] = kv.value
	}
	if !b.dotenvSeen[resolved] {
		b.dotenvSeen[resolved] = true
		b.dotenvFiles = append(b.dotenvFiles, resolved)
	}

	return starlark.None, nil
}

// recordProbe remembers a host-state observation (deduped by kind+arg) so sync
// can persist it for stale detection.
func (b *builder) recordProbe(kind, arg, result string) {
	key := kind + "\x00" + arg
	if b.probeSeen[key] {
		return
	}

	b.probeSeen[key] = true
	b.probes = append(b.probes, model.Probe{Kind: kind, Arg: arg, Result: result})
}

// boolResult renders a probe boolean as the "1"/"0" recorded form.
func boolResult(ok bool) string {
	if ok {
		return "1"
	}
	return "0"
}

func (b *builder) envFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	var value starlark.Value
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &name, "value", &value); err != nil {
		return nil, err
	}
	switch v := value.(type) {
	case starlark.String:
		// Expand $VAR / ${VAR} references, preferring vars set earlier in this
		// config, then the process environment.
		b.envSet[name] = os.Expand(string(v), b.envLookup)
	case deferredValue:
		// A deferred value (shell.exec / mise.where, composed with +): resolved
		// after evaluation, never during it.
		b.deferredEnv = append(b.deferredEnv, model.DeferredEnv{Name: name, Segments: v.segments})
	default:
		return nil, fmt.Errorf("%s: value must be a string, shell.exec(...), or mise.where(...), got %s", fn.Name(), value.Type())
	}
	return starlark.None, nil
}

// deferredValue is a lazily-resolved string: an ordered list of segments, each a
// literal, an exec (shell.exec), or a where (mise.where). Producers return a
// one-segment value; `+` concatenates. It never runs anything during evaluation,
// so inspecting an untrusted config runs no code. shell.env consumes it; because
// it is deferred it cannot be branched on at eval — use exists()/which()/env().
type deferredValue struct{ segments []model.Segment }

var (
	_ starlark.Value     = deferredValue{}
	_ starlark.HasBinary = deferredValue{}
)

func (v deferredValue) String() string        { return "deferred value" }
func (v deferredValue) Type() string          { return "deferred" }
func (v deferredValue) Freeze()               {}
func (v deferredValue) Truth() starlark.Bool  { return starlark.True }
func (v deferredValue) Hash() (uint32, error) { return 0, fmt.Errorf("deferred value is unhashable") }

// Binary implements `+` for building composite values: deferred+string,
// string+deferred, and deferred+deferred all concatenate segment lists (a string
// becomes a literal segment). The join is literal (as `+` implies), so include
// any separator. Other ops/operands return the default "unknown binary op" error.
func (v deferredValue) Binary(op syntax.Token, y starlark.Value, side starlark.Side) (starlark.Value, error) {
	if op != syntax.PLUS {
		return nil, nil
	}
	other, ok := toSegments(y)
	if !ok {
		return nil, nil
	}
	if side == starlark.Left { // v + y
		return deferredValue{segments: concatSegments(v.segments, other)}, nil
	}
	return deferredValue{segments: concatSegments(other, v.segments)}, nil // y + v
}

// toSegments coerces a `+` operand into segments: a string becomes one literal
// segment, a deferredValue contributes its own segments. Anything else is not a
// supported operand.
func toSegments(v starlark.Value) ([]model.Segment, bool) {
	switch t := v.(type) {
	case starlark.String:
		return []model.Segment{{Kind: model.SegLiteral, Value: string(t)}}, true
	case deferredValue:
		return t.segments, true
	}
	return nil, false
}

func concatSegments(a, b []model.Segment) []model.Segment {
	out := make([]model.Segment, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

// execFn implements shell.exec(command, dynamic=False, shell=""): return a
// deferred value whose stdout becomes an environment value via shell.env. The
// command runs at sync time (dynamic=False, baked into the activation script) or
// in the shell on each activation (dynamic=True) — never during evaluation.
//
// shell picks the interpreter: "" (default) means the local shell ($SHELL, else
// sh) for static resolution and the activating shell for a dynamic entry; a
// value like "bash" runs the command via `<shell> -c`.
func (b *builder) execFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var command, shell string
	var dynamic bool
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "command", &command, "dynamic?", &dynamic, "shell?", &shell); err != nil {
		return nil, err
	}
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("%s: command must not be empty", fn.Name())
	}
	return deferredValue{segments: []model.Segment{{Kind: model.SegExec, Value: command, Shell: shell, Dynamic: dynamic}}}, nil
}

// miseWhereFn implements mise.where(name): return a deferred value for the
// directory where mise installed the tool (the same dir added to PATH), resolved
// at sync time. Compose with `+` to append a subpath. Pass it to shell.env; the
// tool must be a declared mise tool (validated in the plan).
func (b *builder) miseWhereFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%s: tool name must not be empty", fn.Name())
	}
	return deferredValue{segments: []model.Segment{{Kind: model.SegWhere, Value: name}}}, nil
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

// miseJobsFn implements mise.jobs(n): cap how many tools mise installs in
// parallel (passed to `mise install --jobs n`).
func (b *builder) miseJobsFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var n int
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "n", &n); err != nil {
		return nil, err
	}

	if n < 1 {
		return nil, fmt.Errorf("%s: must be >= 1, got %d", fn.Name(), n)
	}

	b.miseJobs = n
	return starlark.None, nil
}

func (b *builder) pipInstallFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	specs, err := unpackSpecs(fn.Name(), args, kwargs)
	if err != nil {
		return nil, err
	}

	for _, s := range specs {
		b.pipPackages = append(b.pipPackages, model.Package{Name: s.name, Version: s.version})
	}
	return starlark.None, nil
}

func (b *builder) npmInstallFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	specs, err := unpackSpecs(fn.Name(), args, kwargs)
	if err != nil {
		return nil, err
	}

	for _, s := range specs {
		b.npmPackages = append(b.npmPackages, model.Package{Name: s.name, Version: s.version})
	}
	return starlark.None, nil
}

func (b *builder) uvInstallFn(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	specs, err := unpackSpecs(fn.Name(), args, kwargs)
	if err != nil {
		return nil, err
	}

	for _, s := range specs {
		b.uvPackages = append(b.uvPackages, model.Package{Name: s.name, Version: s.version})
	}
	return starlark.None, nil
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
	p.DeferredEnv = b.deferredEnv
	p.Aliases = b.aliases
	p.SourceFuncs = b.sourceFuncs
	p.Hooks = b.hooks
	p.PipPackages = b.pipPackages
	p.NpmPackages = b.npmPackages
	p.UvPackages = b.uvPackages

	// pip/uv/npm run on a toolchain that tau provisions via mise, so declaring packages implies the matching runtime:
	// pip/uv -> python, npm -> node, and uv also needs the uv binary. Add these implicitly unless already pinned.
	tools := append([]model.MiseTool{}, b.miseTools...)
	if (len(b.pipPackages) > 0 || len(b.uvPackages) > 0) && !hasMiseTool(tools, "python") {
		tools = append(tools, model.MiseTool{Name: "python"})
	}

	if len(b.npmPackages) > 0 && !hasMiseTool(tools, "node") {
		tools = append(tools, model.MiseTool{Name: "node"})
	}

	if len(b.uvPackages) > 0 && !hasMiseTool(tools, "uv") {
		tools = append(tools, model.MiseTool{Name: "uv"})
	}

	p.MiseTools = tools
	p.MiseJobs = b.miseJobs

	// Tool bin directories are auto-prepended so their executables resolve first on PATH. pip and npm install into
	// per-tool prefixes under <stateDir>/tools/{pip,npm} with deterministic paths, prepended here. mise tool bin dirs
	// live in mise's store and are prepended at sync time (their versioned paths are only known after installation),
	// the way `mise activate` exposes tools — no symlink/wrapper farm.
	var prepend []string
	if len(b.pipPackages) > 0 {
		p.PipDir = filepath.Join(p.StateDir, "tools", "pip")
		prepend = append(prepend, filepath.Join(p.PipDir, "bin"))
	}

	if len(b.npmPackages) > 0 {
		p.NpmDir = filepath.Join(p.StateDir, "tools", "npm")
		prepend = append(prepend, filepath.Join(p.NpmDir, "bin"))
	}

	if len(b.uvPackages) > 0 {
		p.UvDir = filepath.Join(p.StateDir, "tools", "uv")
		prepend = append(prepend, filepath.Join(p.UvDir, "bin"))
	}

	prepend = append(prepend, b.pathPrepend...)

	// PATH otherwise preserves user order; entries are de-duplicated.
	p.PathPrepend = dedup(prepend)
	p.PathAppend = dedup(b.pathAppend)

	return p, nil
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

// dotenvPair is one parsed KEY=VALUE entry from a .env file.
type dotenvPair struct{ key, value string }

// parseDotenv parses a minimal .env format: KEY=VALUE lines with an optional
// `export ` prefix, `#` comment lines, and blank lines. A value may be wrapped
// in single quotes (literal) or double quotes (with \n \t \r \\ \" escapes); an
// unquoted value is taken verbatim after trimming surrounding whitespace.
func parseDotenv(data []byte) ([]dotenvPair, error) {
	var out []dotenvPair
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if rest, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimSpace(rest)
		}

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", i+1, raw)
		}

		key = strings.TrimSpace(key)
		if !validEnvName(key) {
			return nil, fmt.Errorf("line %d: invalid variable name %q", i+1, key)
		}

		out = append(out, dotenvPair{key, dotenvValue(strings.TrimSpace(val))})
	}

	return out, nil
}

// dotenvValue unwraps a .env value: single quotes are literal, double quotes
// honor a small set of escapes, and an unquoted value passes through.
func dotenvValue(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}

	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r", `\"`, `"`, `\\`, `\`).Replace(s[1 : len(s)-1])
	}

	return s
}

// validEnvName reports whether s is a POSIX-ish environment variable name
// (leading letter/underscore, then letters/digits/underscores).
func validEnvName(s string) bool {
	if s == "" {
		return false
	}

	for i, r := range s {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}

	return true
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

func fileOptions() *syntax.FileOptions {
	// Allow common ergonomic features in config files.
	return &syntax.FileOptions{
		Set:             true,
		While:           false,
		TopLevelControl: true,
		GlobalReassign:  false,
	}
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

// platformOS - linux, darwin, etc.
func platformOS() string {
	return runtime.GOOS
}

// platformArch - amd64, arm64, etc.
func platformArch() string {
	return runtime.GOARCH
}

func decorateStarlarkErr(err error) error {
	if evalErr, ok := err.(*starlark.EvalError); ok {
		return fmt.Errorf("%s", evalErr.Backtrace())
	}
	return err
}
