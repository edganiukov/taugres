// Package model defines the normalized environment plan produced by evaluating
// a Taugres config and consumed by the shell renderers.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
)

// SourceFunc describes a single shell function. Its body comes from either a
// file (File, sourced at call time) or an inline string (Content, embedded in
// the generated function). Exactly one of File/Content is set. A given function
// name may have multiple SourceFunc entries targeting different shells.
type SourceFunc struct {
	Name    string   `json:"name"`
	Shells  []string `json:"shells"`
	File    string   `json:"file,omitempty"`    // resolved absolute path
	Content string   `json:"content,omitempty"` // inline body
}

// HookScript is a raw shell snippet run at activation time (after env/PATH/
// aliases/functions are set), declared with shell.hook(...). Like SourceFunc,
// its body is either a file or inline content, and it targets specific shells.
// Hooks run in declaration order and are not undone on deactivation.
type HookScript struct {
	Shells  []string `json:"shells"`
	File    string   `json:"file,omitempty"`
	Content string   `json:"content,omitempty"`
}

// Segment kinds for a DeferredEnv value.
const (
	SegLiteral = "literal" // a plain string
	SegExec    = "exec"    // a command whose stdout is captured (shell.exec)
	SegWhere   = "where"   // a mise tool's bin dir (mise.where)
)

// Segment is one piece of a DeferredEnv value. Kind selects the meaning of the
// other fields: a literal (Value is the text), an exec (Value is the command,
// with Shell/Dynamic), or a where (Value is the mise tool name). A value like
// mise.where("go") + "/x" becomes [where "go", literal "/x"].
type Segment struct {
	Kind    string `json:"kind"`
	Value   string `json:"value"`             // literal text | exec command | mise tool name
	Shell   string `json:"shell,omitempty"`   // exec: interpreter ("" = local $SHELL, else sh)
	Dynamic bool   `json:"dynamic,omitempty"` // exec: run at activation instead of sync
}

// DeferredEnv is an environment variable whose value is resolved after config
// evaluation, declared with shell.env(name, <deferred>) where the deferred value
// comes from shell.exec(...) / mise.where(...) (composed with +). No segment runs
// during evaluation (that would let an untrusted config run code on inspection).
//
// A fully-static value (no dynamic exec) is resolved at sync — trust-gated, after
// tool installs, so provisioned tools are on PATH — and baked into the activation
// script as a normal (save/restored) variable. A value with a dynamic exec is
// rendered as a shell string mixing baked literals and `$(cmd)` substitutions
// that run on every activation.
type DeferredEnv struct {
	Name     string    `json:"name"`
	Segments []Segment `json:"segments"`
}

// IsDynamic reports whether any segment must run in the shell at activation (a
// dynamic exec), which means the value is rendered rather than baked into EnvSet.
func (d DeferredEnv) IsDynamic() bool {
	for _, s := range d.Segments {
		if s.Kind == SegExec && s.Dynamic {
			return true
		}
	}
	return false
}

// MiseTool is a tool/runtime to be installed via mise, declared with
// mise.tool(name, version). An empty Version means "latest".
type MiseTool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Package is a library to be installed by a package manager (pip, uv, or npm)
// into a project-local prefix, declared with pip.install/uv.install/npm.install.
// An empty Version means the latest release. The managing tool is conveyed by
// which Plan field holds it (PipPackages/UvPackages/NpmPackages), so one type
// serves all three.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Probe records one host-state observation made during evaluation
// (exists(path), which(name), or env(name)) and the result at that time. Sync
// persists these so the shell hook / staleness checks can notice when the
// observed state changes and trigger a resync.
//
// Kind is "exists", "which", or "env". Arg is the resolved absolute path
// (exists), the binary name (which), or the variable name (env). Result is
// "1"/"0" for exists, the resolved path (or "" when not found) for which, and
// EnvProbeResult (a value hash, or "" when unset) for env.
type Probe struct {
	Kind   string `json:"kind"`
	Arg    string `json:"arg"`
	Result string `json:"result"`
}

// EnvProbeResult encodes an env(name) observation for the manifest: "" when the
// variable is unset, otherwise a sha256 hash of its value. Hashing keeps secrets
// out of the on-disk manifest and ensures the recorded result never contains the
// '|' the manifest's probe line uses as a separator. A set-but-empty value
// hashes to a non-empty digest, so it stays distinct from unset.
func EnvProbeResult(value string, present bool) string {
	if !present {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Plan is the fully normalized, resolved environment plan. All paths are
// absolute. Renderers should be able to produce shell scripts from this struct
// without further resolution.
type Plan struct {
	// Discovery-derived roots.
	RepoRoot    string `json:"repoRoot"`
	ProjectRoot string `json:"projectRoot"`
	ConfigPath  string `json:"configPath"`
	StateDir    string `json:"stateDir"`

	// Optional project name from project(...).
	ProjectName string `json:"projectName,omitempty"`

	// Environment.
	EnvSet   map[string]string `json:"envSet"`
	EnvUnset []string          `json:"envUnset"`

	// DeferredEnv are variables resolved after evaluation (shell.exec/mise.where,
	// composed with +). Fully-static ones are baked into EnvSet at sync; ones with
	// a dynamic exec are rendered as command substitutions in the activation script.
	DeferredEnv []DeferredEnv `json:"deferredEnv,omitempty"`

	// PATH modifications, in user-specified order (post-dedup).
	PathPrepend []string `json:"pathPrepend"`
	PathAppend  []string `json:"pathAppend"`

	// Aliases keyed by alias name.
	Aliases map[string]string `json:"aliases"`

	// SourceFuncs keyed by function name.
	SourceFuncs map[string][]SourceFunc `json:"sourceFuncs"`

	// Hooks are raw activation snippets, in declaration order.
	Hooks []HookScript `json:"hooks,omitempty"`

	// MiseTools declared with mise.tool(...), in declaration order.
	MiseTools []MiseTool `json:"miseTools"`

	// MiseJobs caps how many tools mise installs in parallel (mise.jobs(n)).
	MiseJobs int `json:"miseJobs,omitempty"`

	// PipPackages declared with pip.install(...), in declaration order.
	PipPackages []Package `json:"pipPackages"`

	// NpmPackages declared with npm.install(...), in declaration order.
	NpmPackages []Package `json:"npmPackages"`

	// UvPackages declared with uv.install(...), in declaration order.
	UvPackages []Package `json:"uvPackages,omitempty"`

	// PackageManagers is the extensible manager-keyed declaration view. The
	// built-in fields above remain for lock/config JSON compatibility.
	PackageManagers map[string][]Package `json:"packageManagers,omitempty"`

	// PipDir is the project-local pip virtualenv (<stateDir>/tools/pip). It is
	// set when PipPackages is non-empty; its bin/ is auto-prepended to PATH.
	PipDir string `json:"pipDir,omitempty"`

	// NpmDir is the project-local npm prefix (<stateDir>/tools/npm). It is set
	// when NpmPackages is non-empty; its bin/ is auto-prepended to PATH.
	NpmDir string `json:"npmDir,omitempty"`

	// UvDir is the project-local uv virtualenv (<stateDir>/tools/uv). It is set
	// when UvPackages is non-empty; its bin/ is auto-prepended to PATH.
	UvDir string `json:"uvDir,omitempty"`
}

// EvaluatedPlan names the phase produced directly by Starlark evaluation.
// It remains an alias for source compatibility while APIs can document phase.
type EvaluatedPlan = Plan

// ResolvedPlan is a private mutable copy prepared by sync after versions,
// manager directories, and static deferred values have been resolved. Renderers
// accept this phase so evaluation output is never mutated in place.
type ResolvedPlan struct {
	*Plan
	ManagerDirs map[string][]string
}

// ResolvePlan deep-copies an evaluated plan into the sync/render phase.
func ResolvePlan(evaluated *EvaluatedPlan) *ResolvedPlan {
	plan := *evaluated
	plan.EnvSet = maps.Clone(evaluated.EnvSet)
	plan.EnvUnset = slices.Clone(evaluated.EnvUnset)
	plan.PathPrepend = slices.Clone(evaluated.PathPrepend)
	plan.PathAppend = slices.Clone(evaluated.PathAppend)
	plan.Aliases = maps.Clone(evaluated.Aliases)
	plan.MiseTools = slices.Clone(evaluated.MiseTools)
	plan.PipPackages = slices.Clone(evaluated.PipPackages)
	plan.NpmPackages = slices.Clone(evaluated.NpmPackages)
	plan.UvPackages = slices.Clone(evaluated.UvPackages)
	plan.PackageManagers = make(map[string][]Package, len(evaluated.PackageManagers))
	for manager, packages := range evaluated.PackageManagers {
		plan.PackageManagers[manager] = slices.Clone(packages)
	}

	plan.DeferredEnv = make([]DeferredEnv, len(evaluated.DeferredEnv))
	for i, deferred := range evaluated.DeferredEnv {
		plan.DeferredEnv[i] = deferred
		plan.DeferredEnv[i].Segments = slices.Clone(deferred.Segments)
	}
	plan.SourceFuncs = make(map[string][]SourceFunc, len(evaluated.SourceFuncs))
	for name, entries := range evaluated.SourceFuncs {
		cloned := make([]SourceFunc, len(entries))
		for i, entry := range entries {
			cloned[i] = entry
			cloned[i].Shells = slices.Clone(entry.Shells)
		}
		plan.SourceFuncs[name] = cloned
	}
	plan.Hooks = make([]HookScript, len(evaluated.Hooks))
	for i, hook := range evaluated.Hooks {
		plan.Hooks[i] = hook
		plan.Hooks[i].Shells = slices.Clone(hook.Shells)
	}

	return &ResolvedPlan{Plan: &plan, ManagerDirs: map[string][]string{}}
}

// NewPlan returns an empty evaluated plan with initialized maps.
func NewPlan() *Plan {
	return &Plan{
		EnvSet:          map[string]string{},
		Aliases:         map[string]string{},
		SourceFuncs:     map[string][]SourceFunc{},
		PackageManagers: map[string][]Package{},
	}
}

// Packages returns declarations for manager, preferring the extensible map and
// falling back to the original built-in fields for source compatibility.
func (p *Plan) Packages(manager string) []Package {
	if packages, ok := p.PackageManagers[manager]; ok {
		return packages
	}
	switch manager {
	case "pip":
		return p.PipPackages
	case "npm":
		return p.NpmPackages
	case "uv":
		return p.UvPackages
	}
	return nil
}
