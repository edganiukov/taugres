package environment

import (
	"os"
	"strings"
	"testing"

	"github.com/edganiukov/taugres/internal/model"
)

func TestBuild(t *testing.T) {
	t.Setenv("PATH", strings.Join([]string{"/ambient"}, string(os.PathListSeparator)))
	t.Setenv("DROP", "x")
	plan := model.NewPlan()
	plan.RepoRoot = "/repo"
	plan.ProjectRoot = "/repo/project"
	plan.ConfigPath = "/repo/project/project.tg"
	plan.StateDir = "/repo/project/.taugres"
	plan.EnvSet["A"] = "b"
	plan.EnvUnset = []string{"DROP"}
	plan.PathPrepend = []string{"/user", "/manager"}
	plan.PathAppend = []string{"/tail"}

	env := Build(plan, []string{"/manager"})
	if env["A"] != "b" || env["TAUGRES_PROJECT_ROOT"] != plan.ProjectRoot {
		t.Fatalf("env = %+v", env)
	}
	if _, present := env["DROP"]; present {
		t.Fatal("unset variable remains present")
	}
	wantPath := strings.Join([]string{"/manager", "/user", "/ambient", "/tail"}, string(os.PathListSeparator))
	if env["PATH"] != wantPath {
		t.Fatalf("PATH = %q, want %q", env["PATH"], wantPath)
	}
}
