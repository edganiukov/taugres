// Package pip integrates with Python's pip via a project-local virtualenv.
// During `tau sync` it creates a venv under .taugres/tools/pip and installs the
// declared packages into it; its bin/ is prepended to PATH on activation, which
// never runs pip.
//
// The Python interpreter comes from the mise-provisioned toolchain (tau adds an
// implicit `python` tool for pip projects), so pip never depends on a system
// Python. Installing into a project-local venv keeps environments isolated.
package pip

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/tools/toolenv"
)

// outputPrefix labels pip's own output so its origin is clear.
const outputPrefix = "pip: "

// Reporter is the shared progress reporter type.
type Reporter = toolenv.Reporter

// ref returns the pip requirement string ("name==version" or "name").
func ref(p model.PipPackage) string {
	if p.Version == "" {
		return p.Name
	}
	return p.Name + "==" + p.Version
}

// BinDir returns the bin directory inside a virtualenv. This is what activation
// prepends to PATH; no symlinking is needed since the venv lives under
// .taugres/ already.
func BinDir(venvDir string) string {
	return filepath.Join(venvDir, "bin")
}

// Install ensures a venv exists at venvDir and installs the given packages into
// it (each PipPackage.Version is the exact spec to install), using the Python
// interpreter from the mise toolchain dirs. It returns each package's resolved
// concrete version. When out is non-nil, pip's output is streamed live.
func Install(pkgs []model.PipPackage, venvDir string, toolchainBins []string, out io.Writer, report Reporter) (map[string]string, error) {
	if len(pkgs) == 0 {
		return nil, nil
	}
	python := toolenv.Resolve(toolchainBins, "python3", "python")
	if _, err := exec.LookPath(python); err != nil {
		return nil, fmt.Errorf("pip.install: python interpreter not found (expected mise to provide it): %s", python)
	}
	if err := ensureVenv(venvDir, python, out); err != nil {
		return nil, err
	}
	pipBin := filepath.Join(BinDir(venvDir), "pip")

	// Install all packages in one command so pip resolves them together and
	// downloads in parallel.
	refs := make([]string, len(pkgs))
	for i, p := range pkgs {
		refs[i] = ref(p)
	}
	finish := func(bool) {}
	if report != nil {
		finish = report(strings.Join(refs, " "))
	}
	if err := run(pipBin, append([]string{"install"}, refs...), out, "pip install "+strings.Join(refs, " ")); err != nil {
		finish(false)
		return nil, err
	}
	finish(true)

	resolved := map[string]string{}
	for _, p := range pkgs {
		resolved[p.Name] = installedVersion(pipBin, p.Name)
	}
	return resolved, nil
}

// Uninstall removes the named packages from the venv (best effort). Used to GC
// packages that were dropped from the config.
func Uninstall(venvDir string, names []string, out io.Writer) error {
	if len(names) == 0 {
		return nil
	}
	pipBin := filepath.Join(BinDir(venvDir), "pip")
	if !toolenv.IsExecutable(pipBin) {
		return nil // no venv, nothing to remove
	}
	args := append([]string{"uninstall", "-y"}, names...)
	return run(pipBin, args, out, "pip uninstall")
}

// installedVersion returns the concrete installed version of a package via
// `pip show`, or "" if it cannot be determined.
func installedVersion(pipBin, name string) string {
	out, err := exec.Command(pipBin, "show", name).Output()
	if err != nil {
		return ""
	}
	return toolenv.ScrapeVersion(out)
}

// ensureVenv creates the virtualenv with the given python if its bin/pip is not
// already present.
func ensureVenv(venvDir, python string, out io.Writer) error {
	if toolenv.IsExecutable(filepath.Join(BinDir(venvDir), "pip")) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(venvDir), 0o755); err != nil {
		return err
	}
	return run(python, []string{"-m", "venv", venvDir}, out, "python -m venv")
}

// run executes a command, streaming combined output to out (also captured so a
// failure can surface it), and returns a concise error on failure.
func run(bin string, args []string, out io.Writer, what string) error {
	return toolenv.Run(exec.Command(bin, args...), out, outputPrefix, what)
}
