// Package validate checks a normalized plan for correctness (valid names,
// shells, and reachable shell.fn/shell.hook files) and surfaces warnings.
package validate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.gnkv.dev/taugres/internal/model"
)

var (
	envNameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	nameRe      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	validShells = map[string]bool{"bash": true, "zsh": true, "fish": true}
)

// Report is the result of validating a plan.
type Report struct {
	Errors   []string
	Warnings []string
}

// HasErrors reports whether any hard errors were found.
func (r *Report) HasErrors() bool { return len(r.Errors) > 0 }

// Err returns a combined error if there are errors, else nil.
func (r *Report) Err() error {
	if !r.HasErrors() {
		return nil
	}
	return errors.New(r.Errors[0])
}

// Validate checks the plan and returns a report.
func Validate(p *model.Plan) *Report {
	r := &Report{}

	// Env var names.
	for _, name := range sortedKeys(p.EnvSet) {
		if !envNameRe.MatchString(name) {
			r.Errors = append(r.Errors, fmt.Sprintf("invalid environment variable name: %q", name))
		}
	}
	for _, name := range p.EnvUnset {
		if !envNameRe.MatchString(name) {
			r.Errors = append(r.Errors, fmt.Sprintf("invalid environment variable name in unset: %q", name))
		}
		if _, ok := p.EnvSet[name]; ok {
			r.Errors = append(r.Errors, fmt.Sprintf("environment variable %q is both set and unset", name))
		}
	}

	// Alias names.
	for _, name := range sortedKeys(p.Aliases) {
		if !nameRe.MatchString(name) {
			r.Errors = append(r.Errors, fmt.Sprintf("invalid alias name: %q", name))
		}
	}

	// Mise tools: names must be non-empty and free of whitespace.
	for _, t := range p.MiseTools {
		if strings.TrimSpace(t.Name) == "" {
			r.Errors = append(r.Errors, "mise.tool: empty tool name")
			continue
		}
		if strings.ContainsAny(t.Name, " \t") || strings.ContainsAny(t.Version, " \t") {
			r.Errors = append(r.Errors, fmt.Sprintf("mise.tool %q: name/version must not contain whitespace", t.Name))
		}
	}

	// Pip packages: names must be non-empty and free of whitespace.
	for _, pkg := range p.PipPackages {
		if strings.TrimSpace(pkg.Name) == "" {
			r.Errors = append(r.Errors, "pip.install: empty package name")
			continue
		}
		if strings.ContainsAny(pkg.Name, " \t") || strings.ContainsAny(pkg.Version, " \t") {
			r.Errors = append(r.Errors, fmt.Sprintf("pip.install %q: name/version must not contain whitespace", pkg.Name))
		}
	}

	// Npm packages: names must be non-empty and free of whitespace.
	for _, pkg := range p.NpmPackages {
		if strings.TrimSpace(pkg.Name) == "" {
			r.Errors = append(r.Errors, "npm.install: empty package name")
			continue
		}
		if strings.ContainsAny(pkg.Name, " \t") || strings.ContainsAny(pkg.Version, " \t") {
			r.Errors = append(r.Errors, fmt.Sprintf("npm.install %q: name/version must not contain whitespace", pkg.Name))
		}
	}

	// Source function names, shells, and files.
	for _, name := range sortedFuncKeys(p.SourceFuncs) {
		if !nameRe.MatchString(name) {
			r.Errors = append(r.Errors, fmt.Sprintf("invalid function name: %q", name))
		}
		for _, sf := range p.SourceFuncs[name] {
			if len(sf.Shells) == 0 {
				r.Errors = append(r.Errors, fmt.Sprintf("shell.fn %q: no shells specified", name))
			}
			for _, sh := range sf.Shells {
				if !validShells[sh] {
					r.Errors = append(r.Errors, fmt.Sprintf("shell.fn %q: unsupported shell %q", name, sh))
				}
			}
			// Inline-content functions have no file to check.
			if sf.Content != "" {
				continue
			}
			if !filepath.IsAbs(sf.File) {
				r.Errors = append(r.Errors, fmt.Sprintf("shell.fn %q: file path not resolved to absolute: %q", name, sf.File))
				continue
			}
			if !fileExists(sf.File) {
				r.Errors = append(r.Errors, fmt.Sprintf("shell.fn %q: file not found: %s", name, sf.File))
			}
		}
	}

	for i, h := range p.Hooks {
		if len(h.Shells) == 0 {
			r.Errors = append(r.Errors, fmt.Sprintf("shell.hook #%d: no shells specified", i+1))
		}
		for _, sh := range h.Shells {
			if !validShells[sh] {
				r.Errors = append(r.Errors, fmt.Sprintf("shell.hook #%d: unsupported shell %q", i+1, sh))
			}
		}
		if h.Content != "" {
			continue
		}
		if !filepath.IsAbs(h.File) {
			r.Errors = append(r.Errors, fmt.Sprintf("shell.hook #%d: file path not resolved to absolute: %q", i+1, h.File))
			continue
		}
		if !fileExists(h.File) {
			r.Errors = append(r.Errors, fmt.Sprintf("shell.hook #%d: file not found: %s", i+1, h.File))
		}
	}

	// pip and uv both create Python venvs (.taugres/tools/pip and
	// .taugres/tools/uv), each with its own `python`; mixing them splits packages
	// across two disjoint environments. Steer toward one.
	if len(p.PipPackages) > 0 && len(p.UvPackages) > 0 {
		r.Warnings = append(r.Warnings,
			"both pip.install and uv.install are used: they create separate venvs with separate `python`s, so packages are split across two environments — prefer one")
	}

	return r
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedFuncKeys(m map[string][]model.SourceFunc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
