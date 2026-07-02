// Package npm integrates with Node's npm via a project-local install prefix.
// During `tau sync` it installs the declared packages into .taugres/tools/npm
// using npm's global-with-prefix mode, so their executables land in
// .taugres/tools/npm/bin. Activation prepends that directory to PATH; it never
// runs npm.
//
// node/npm come from the mise-provisioned toolchain (tau adds an implicit
// `node` tool for npm projects), so npm never depends on a system Node.
// Installing into a project-local prefix keeps environments isolated; package
// CLIs become runnable directly on PATH — like npx, but resolved locally.
package npm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edganiukov/taugres/internal/lock"
	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/tools/toolenv"
)

// Fresh reports whether every package is already installed at its locked version
// — the prefix's node_modules exists and each package's recorded spec is
// unchanged and resolved — so Install (and its registry access) can be skipped.
func Fresh(pkgs []model.NpmPackage, npmDir string, locked map[string]lock.Entry) bool {
	if len(pkgs) == 0 {
		return true // nothing declared -> nothing to install
	}
	if !toolenv.IsDir(filepath.Join(npmDir, "lib", "node_modules")) {
		return false
	}
	for _, p := range pkgs {
		if e, ok := locked[p.Name]; !ok || e.Requested != p.Version || e.Resolved == "" {
			return false
		}
	}
	return true
}

// outputPrefix labels npm's own output so its origin is clear.
const outputPrefix = "npm: "

// Reporter is the shared progress reporter type.
type Reporter = toolenv.Reporter

// ref returns the npm package spec ("name@version" or "name").
func ref(p model.NpmPackage) string {
	if p.Version == "" {
		return p.Name
	}
	return p.Name + "@" + p.Version
}

// BinDir returns the bin directory inside an npm prefix. This is what activation
// prepends to PATH; no symlinking is needed since the prefix lives under
// .taugres/ already.
func BinDir(npmDir string) string {
	return filepath.Join(npmDir, "bin")
}

// Install installs the given packages into the project-local npm prefix at
// npmDir (each NpmPackage.Version is the exact spec to install), using npm/node
// from the mise toolchain dirs. It returns each package's resolved concrete
// version. When out is non-nil, npm's output is streamed live.
func Install(pkgs []model.NpmPackage, npmDir string, toolchainBins []string, out io.Writer, report Reporter) (map[string]string, error) {
	if len(pkgs) == 0 {
		return nil, nil
	}
	npmExe := toolenv.Resolve(toolchainBins, "npm")
	if _, err := exec.LookPath(npmExe); err != nil {
		return nil, fmt.Errorf("npm.install: npm not found (expected mise to provide node): %s", npmExe)
	}
	if err := os.MkdirAll(npmDir, 0o755); err != nil {
		return nil, err
	}
	// Install all packages in one command so npm resolves and downloads them
	// together. `-g --prefix <dir>` installs into <dir>/bin and <dir>/lib
	// without touching the system/global location.
	refs := make([]string, len(pkgs))
	for i, p := range pkgs {
		refs[i] = ref(p)
	}
	finish := func(bool) {}
	if report != nil {
		finish = report(strings.Join(refs, " "))
	}
	args := append([]string{"install", "-g", "--prefix", npmDir}, refs...)
	if err := run(npmExe, args, toolchainBins, out, "npm install "+strings.Join(refs, " ")); err != nil {
		finish(false)
		return nil, err
	}
	finish(true)

	resolved := map[string]string{}
	for _, p := range pkgs {
		resolved[p.Name] = installedVersion(npmDir, p.Name)
	}
	return resolved, nil
}

// Uninstall removes the named packages from the npm prefix (best effort). Used
// to GC packages dropped from the config.
func Uninstall(npmDir string, names []string, toolchainBins []string, out io.Writer) error {
	if len(names) == 0 {
		return nil
	}
	if !toolenv.IsDir(filepath.Join(npmDir, "lib", "node_modules")) {
		return nil // nothing installed
	}
	npmExe := toolenv.Resolve(toolchainBins, "npm")
	args := append([]string{"uninstall", "-g", "--prefix", npmDir}, names...)
	return run(npmExe, args, toolchainBins, out, "npm uninstall")
}

// installedVersion reads a package's version from its installed package.json,
// or "" if it cannot be determined.
func installedVersion(npmDir, name string) string {
	data, err := os.ReadFile(filepath.Join(npmDir, "lib", "node_modules", name, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return pkg.Version
}

// run executes npm, streaming combined output to out (also captured so a
// failure can surface it). toolchainBins are prepended to the child's PATH so
// npm's `#!/usr/bin/env node` shebang resolves the mise-provided node.
func run(bin string, args []string, toolchainBins []string, out io.Writer, what string) error {
	cmd := exec.Command(bin, args...)
	if len(toolchainBins) > 0 {
		p := strings.Join(toolchainBins, string(os.PathListSeparator))
		cmd.Env = append(os.Environ(), "PATH="+p+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return toolenv.Run(cmd, out, outputPrefix, what)
}
