// Package uv integrates with the uv Python package manager via a project-local
// virtualenv. During `tau sync` it creates a venv under .taugres/tools/uv and
// installs the declared packages into it with `uv pip`; its bin/ is prepended
// to PATH on activation, which never runs uv.
//
// uv and its Python come from the mise-provisioned toolchain (tau adds implicit
// `uv` and `python` tools for uv projects). uv is the faster, modern
// alternative to pip; the venv/PATH model is the same as the pip integration.
package uv

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"go.gnkv.dev/taugres/internal/model"
	"go.gnkv.dev/taugres/internal/tools/toolenv"
)

// outputPrefix labels uv's own output so its origin is clear.
const outputPrefix = "uv: "

// Reporter is the shared progress reporter type.
type Reporter = toolenv.Reporter

// ref returns the requirement string ("name==version" or "name").
func ref(p model.UvPackage) string {
	if p.Version == "" {
		return p.Name
	}
	return p.Name + "==" + p.Version
}

// BinDir returns the bin directory inside the venv (prepended to PATH).
func BinDir(venvDir string) string {
	return filepath.Join(venvDir, "bin")
}

// resolve picks an executable named tool from the mise toolchain bin dirs,
// falling back to the name on PATH.
func resolve(toolchainBins []string, tool string) string {
	for _, dir := range toolchainBins {
		if p := filepath.Join(dir, tool); toolenv.IsExecutable(p) {
			return p
		}
	}
	return tool
}

// Install ensures a uv venv exists at venvDir and installs the given packages
// into it (each UvPackage.Version is the exact spec), using uv/python from the
// mise toolchain dirs. It returns each package's resolved concrete version.
// When out is non-nil, uv's output is streamed live.
func Install(pkgs []model.UvPackage, venvDir string, toolchainBins []string, out io.Writer, report Reporter) (map[string]string, error) {
	if len(pkgs) == 0 {
		return nil, nil
	}
	uvExe := resolve(toolchainBins, "uv")
	if _, err := exec.LookPath(uvExe); err != nil {
		return nil, fmt.Errorf("uv.install: uv not found (expected mise to provide it): %s", uvExe)
	}
	venvPython := filepath.Join(BinDir(venvDir), "python")
	if !toolenv.IsExecutable(venvPython) {
		python := resolve(toolchainBins, "python3")
		if !toolenv.IsExecutable(python) {
			python = resolve(toolchainBins, "python")
		}
		if err := run(uvExe, []string{"venv", "--python", python, venvDir}, out, "uv venv"); err != nil {
			return nil, err
		}
	}

	// Install all packages in one command so uv resolves them together.
	refs := make([]string, len(pkgs))
	for i, p := range pkgs {
		refs[i] = ref(p)
	}
	finish := func(bool) {}
	if report != nil {
		finish = report(strings.Join(refs, " "))
	}
	args := append([]string{"pip", "install", "--python", venvPython}, refs...)
	if err := run(uvExe, args, out, "uv pip install "+strings.Join(refs, " ")); err != nil {
		finish(false)
		return nil, err
	}
	finish(true)

	resolved := map[string]string{}
	for _, p := range pkgs {
		resolved[p.Name] = installedVersion(uvExe, venvPython, p.Name)
	}
	return resolved, nil
}

// Uninstall removes the named packages from the venv (best effort). Used to GC
// packages dropped from the config.
func Uninstall(venvDir string, names []string, toolchainBins []string, out io.Writer) error {
	if len(names) == 0 {
		return nil
	}
	venvPython := filepath.Join(BinDir(venvDir), "python")
	if !toolenv.IsExecutable(venvPython) {
		return nil // no venv, nothing to remove
	}
	uvExe := resolve(toolchainBins, "uv")
	args := append([]string{"pip", "uninstall", "--python", venvPython}, names...)
	return run(uvExe, args, out, "uv pip uninstall")
}

// installedVersion returns the concrete installed version via `uv pip show`, or
// "" if it cannot be determined.
func installedVersion(uvExe, venvPython, name string) string {
	out, err := exec.Command(uvExe, "pip", "show", "--python", venvPython, name).Output()
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "Version:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func run(bin string, args []string, out io.Writer, what string) error {
	return toolenv.Run(exec.Command(bin, args...), out, outputPrefix, what)
}
