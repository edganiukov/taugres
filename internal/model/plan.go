// Package model defines the normalized environment plan produced by evaluating
// a Taugres config and consumed by the shell renderers.
package model

import "path/filepath"

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

// MiseTool is a tool/runtime to be installed via mise, declared with
// mise.tool(name, version). An empty Version means "latest".
type MiseTool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// PipPackage is a Python package to be installed via pip into the project-local
// virtualenv, declared with pip.install(name, version). An empty Version means
// the latest compatible release.
type PipPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// NpmPackage is a Node package to be installed via npm into a project-local
// prefix, declared with npm.install(name, version). An empty Version means the
// latest release. Its executables become runnable on PATH (like npx, but
// resolved directly).
type NpmPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// UvPackage is a Python package installed via uv into a project-local venv,
// declared with uv.install(name, version). Like PipPackage but backed by uv
// (faster; manages the venv itself). An empty Version means latest.
type UvPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
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
	PipPackages []PipPackage `json:"pipPackages"`

	// NpmPackages declared with npm.install(...), in declaration order.
	NpmPackages []NpmPackage `json:"npmPackages"`

	// UvPackages declared with uv.install(...), in declaration order.
	UvPackages []UvPackage `json:"uvPackages,omitempty"`

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

// ProjectToolBinDirs returns the project-local tool bin directories (pip venv,
// npm prefix). mise tool bin dirs live in mise's store and are added at sync
// time, since their versioned paths are only known after installation.
func (p *Plan) ProjectToolBinDirs() []string {
	var dirs []string
	if p.PipDir != "" {
		dirs = append(dirs, filepath.Join(p.PipDir, "bin"))
	}
	if p.NpmDir != "" {
		dirs = append(dirs, filepath.Join(p.NpmDir, "bin"))
	}
	if p.UvDir != "" {
		dirs = append(dirs, filepath.Join(p.UvDir, "bin"))
	}
	return dirs
}

// NewPlan returns an empty plan with initialized maps.
func NewPlan() *Plan {
	return &Plan{
		EnvSet:      map[string]string{},
		Aliases:     map[string]string{},
		SourceFuncs: map[string][]SourceFunc{},
	}
}
