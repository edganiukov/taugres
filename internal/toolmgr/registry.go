// Package toolmgr defines the compile-time tool-manager registry used by sync,
// update, status, and lock handling. Static registration keeps startup cheap
// while giving new integrations one central descriptor to implement.
package toolmgr

import (
	"context"
	"io"
	"path/filepath"

	"github.com/edganiukov/taugres/internal/lock"
	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/tools/npm"
	"github.com/edganiukov/taugres/internal/tools/pip"
	"github.com/edganiukov/taugres/internal/tools/toolenv"
	"github.com/edganiukov/taugres/internal/tools/uv"
)

const (
	Mise = "mise"
	Pip  = "pip"
	Npm  = "npm"
	Uv   = "uv"
)

// All is the deterministic manager order used by flags and orchestration.
var All = []string{Mise, Pip, Npm, Uv}

// PackageDescriptor adapts one project-local package manager to the common
// declaration, runtime requirement, lock, path, install, GC, and display model.
type PackageDescriptor struct {
	ID           string
	Display      func(model.Package) string
	Requirements []string
	Install      func(context.Context, []model.Package, string, []string, io.Writer, toolenv.Reporter) (map[string]string, error)
	Uninstall    func(context.Context, string, []string, []string, io.Writer) error
}

// Packages returns this manager's declarations from the generic plan view.
func (d PackageDescriptor) Packages(plan *model.Plan) []model.Package {
	return plan.Packages(d.ID)
}

// Dir returns the manager-owned project-local prefix.
func (d PackageDescriptor) Dir(plan *model.Plan) string {
	return filepath.Join(plan.StateDir, "tools", d.ID)
}

// BinDir returns the directory exposed on PATH.
func (d PackageDescriptor) BinDir(plan *model.Plan) string {
	return filepath.Join(d.Dir(plan), "bin")
}

// Section returns this manager's lock section.
func (d PackageDescriptor) Section(file *lock.File) map[string]lock.Entry {
	return file.Section(d.ID)
}

// PackageManagers is the registry for project-local package integrations.
var PackageManagers = []PackageDescriptor{
	{
		ID:           Pip,
		Display:      pythonRef,
		Requirements: []string{"python"},
		Install:      pip.Install,
		Uninstall: func(ctx context.Context, dir string, names, _ []string, out io.Writer) error {
			return pip.Uninstall(ctx, dir, names, out)
		},
	},
	{
		ID: Npm,
		Display: func(pkg model.Package) string {
			version := pkg.Version
			if version == "" {
				version = "latest"
			}
			return pkg.Name + "@" + version
		},
		Requirements: []string{"node"},
		Install:      npm.Install,
		Uninstall: func(ctx context.Context, dir string, names, bins []string, out io.Writer) error {
			return npm.Uninstall(ctx, dir, names, bins, out)
		},
	},
	{
		ID:           Uv,
		Display:      pythonRef,
		Requirements: []string{"python", "uv"},
		Install:      uv.Install,
		Uninstall: func(ctx context.Context, dir string, names, bins []string, out io.Writer) error {
			return uv.Uninstall(ctx, dir, names, bins, out)
		},
	},
}

func pythonRef(pkg model.Package) string {
	version := pkg.Version
	if version == "" {
		version = "latest"
	}
	return pkg.Name + "==" + version
}

// Package returns a package-manager descriptor by ID.
func Package(id string) (PackageDescriptor, bool) {
	for _, descriptor := range PackageManagers {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return PackageDescriptor{}, false
}

// Section returns a manager's lock section.
func Section(file *lock.File, id string) map[string]lock.Entry {
	for _, manager := range All {
		if manager == id {
			return file.Section(id)
		}
	}
	return nil
}
