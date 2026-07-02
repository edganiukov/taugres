package validate

import (
	"strings"
	"testing"

	"go.gnkv.dev/taugres/internal/model"
	"go.gnkv.dev/taugres/internal/testutil"
)

func TestInvalidEnvName(t *testing.T) {
	p := model.NewPlan()
	p.EnvSet["BAD-NAME"] = "x"
	r := Validate(p)
	if !r.HasErrors() {
		t.Fatal("expected error for invalid env name")
	}
}

func TestSetAndUnsetConflict(t *testing.T) {
	p := model.NewPlan()
	p.EnvSet["FOO"] = "x"
	p.EnvUnset = []string{"FOO"}
	r := Validate(p)
	if !hasErrContaining(r, "both set and unset") {
		t.Errorf("expected set/unset conflict, got %v", r.Errors)
	}
}

func TestMissingSourceFile(t *testing.T) {
	p := model.NewPlan()
	p.SourceFuncs["croot"] = []model.SourceFunc{{
		Name: "croot", Shells: []string{"bash"}, File: "/nonexistent/croot.sh",
	}}
	r := Validate(p)
	if !hasErrContaining(r, "file not found") {
		t.Errorf("expected missing file error, got %v", r.Errors)
	}
}

func TestUnsupportedShell(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	f := testutil.WriteFile(t, dir, "bin/croot.sh", "cd .\n")
	p := model.NewPlan()
	p.SourceFuncs["croot"] = []model.SourceFunc{{
		Name: "croot", Shells: []string{"powershell"}, File: f,
	}}
	r := Validate(p)
	if !hasErrContaining(r, "unsupported shell") {
		t.Errorf("expected unsupported shell error, got %v", r.Errors)
	}
}

func TestValidPlanNoErrors(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	f := testutil.WriteFile(t, dir, "bin/croot.sh", "cd .\n")
	p := model.NewPlan()
	p.EnvSet["DATABASE_URL"] = "x"
	p.EnvUnset = []string{"PYTHONPATH"}
	p.Aliases["ll"] = "ls -lah"
	p.SourceFuncs["croot"] = []model.SourceFunc{{Name: "croot", Shells: []string{"bash", "zsh"}, File: f}}
	r := Validate(p)
	if r.HasErrors() {
		t.Errorf("expected no errors, got %v", r.Errors)
	}
}

func hasErrContaining(r *Report, sub string) bool {
	for _, e := range r.Errors {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}
