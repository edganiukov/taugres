package toolmgr

import (
	"testing"

	"github.com/edganiukov/taugres/internal/lock"
	"github.com/edganiukov/taugres/internal/model"
)

func TestRegistryCoversAllManagers(t *testing.T) {
	file := lock.New()
	for _, id := range All {
		if Section(file, id) == nil {
			t.Errorf("manager %q has no lock section", id)
		}
	}
	for _, manager := range PackageManagers {
		if manager.Install == nil || manager.Uninstall == nil || len(manager.Requirements) == 0 {
			t.Errorf("manager %q has an incomplete adapter", manager.ID)
		}
	}
}

func TestPackageDescriptor(t *testing.T) {
	plan := model.NewPlan()
	plan.StateDir = "/repo/.taugres"
	plan.PipPackages = []model.Package{{Name: "ruff"}}
	descriptor, ok := Package(Pip)
	if !ok {
		t.Fatal("pip is not registered")
	}
	if got := descriptor.Packages(plan); len(got) != 1 || got[0].Name != "ruff" {
		t.Fatalf("packages = %+v", got)
	}
	if got := descriptor.Display(gotPackage(descriptor.Packages(plan))); got != "ruff==latest" {
		t.Fatalf("display = %q", got)
	}
}

func gotPackage(packages []model.Package) model.Package { return packages[0] }
