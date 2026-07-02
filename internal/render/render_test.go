package render

import (
	"strings"
	"testing"

	"github.com/edganiukov/taugres/internal/model"
)

func basePlan() *model.Plan {
	p := model.NewPlan()
	p.RepoRoot = "/repo"
	p.ProjectRoot = "/repo"
	p.ConfigPath = "/repo/workspace.tg"
	p.StateDir = "/repo/.taugres"
	return p
}

func TestActivateSetsBuiltins(t *testing.T) {
	p := basePlan()
	out, err := Activate(p, "bash")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`export TAUGRES_ACTIVE='1'`,
		`export TAUGRES_ROOT='/repo'`,
		`export TAUGRES_PROJECT_ROOT='/repo'`,
		`export TAUGRES_CONFIG='/repo/workspace.tg'`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("activate missing %q\n%s", want, out)
		}
	}
}

func TestActivateEmitsNotice(t *testing.T) {
	p := basePlan()
	p.ProjectName = "my-app"
	out, _ := Activate(p, "bash")
	if !strings.Contains(out, "activated") || !strings.Contains(out, "'my-app'") {
		t.Errorf("activation notice missing project name:\n%s", out)
	}
	if !strings.Contains(out, `=^..^=`) {
		t.Errorf("notice should include the ASCII emoji:\n%s", out)
	}
	if !strings.Contains(out, "[ -t 2 ]") {
		t.Errorf("notice should guard color on a tty:\n%s", out)
	}
}

func TestActivateNoticeFallsBackToDirName(t *testing.T) {
	p := basePlan() // no ProjectName; ProjectRoot=/repo
	out, _ := Activate(p, "zsh")
	if !strings.Contains(out, "'repo'") {
		t.Errorf("notice should fall back to project dir basename:\n%s", out)
	}
}

func TestActivateEscapesSingleQuotes(t *testing.T) {
	p := basePlan()
	p.EnvSet["MSG"] = "it's a test"
	out, _ := Activate(p, "bash")
	if !strings.Contains(out, `export MSG='it'\''s a test'`) {
		t.Errorf("single-quote escaping wrong:\n%s", out)
	}
}

func TestActivateRejectsUnsupportedShell(t *testing.T) {
	if _, err := Activate(basePlan(), "powershell"); err == nil {
		t.Error("expected error for unsupported shell")
	}
}

func TestFishActivate(t *testing.T) {
	p := basePlan()
	p.EnvSet["DEMO"] = "hi there"
	p.EnvUnset = []string{"PYTHONPATH"}
	p.PathPrepend = []string{"/repo/bin"}
	p.PathAppend = []string{"/repo/scripts"}
	p.Aliases["ll"] = "ls -lah"
	p.SourceFuncs["croot"] = []model.SourceFunc{
		{Name: "croot", Shells: []string{"fish"}, Content: "cd $TAUGRES_PROJECT_ROOT"},
	}
	out, err := Activate(p, "fish")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set -gx TAUGRES_ACTIVE '1'",
		"set -gx DEMO 'hi there'",
		"set -e PYTHONPATH",                  // unset
		"set -gx PATH '/repo/bin' $PATH",     // prepend
		"set -gx PATH $PATH '/repo/scripts'", // append
		"alias 'll' 'ls -lah'",
		"function croot",
		"cd $TAUGRES_PROJECT_ROOT",
		"isatty stderr",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fish activate missing %q\n%s", want, out)
		}
	}
}

func TestFishDeactivateRestores(t *testing.T) {
	p := basePlan()
	p.EnvSet["FOO"] = "bar"
	p.Aliases["ll"] = "ls -lah"
	out, err := Deactivate(p, "fish")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`set -gx FOO "$__TAU_SAVE_ENV_FOO"`, // restore prior value
		"set -gx PATH $__TAU_SAVE_PATH",     // restore PATH list
		"functions -e 'll'",                 // remove alias
		"set -e TAUGRES_ACTIVE",             // clear built-ins
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fish deactivate missing %q\n%s", want, out)
		}
	}
}

func TestHooksRenderedForShell(t *testing.T) {
	p := basePlan()
	p.Hooks = []model.HookScript{
		{Shells: []string{"bash", "zsh"}, Content: "mkdir -p .cache"},
		{Shells: []string{"fish"}, File: "/repo/hooks/setup.fish"},
	}
	bash, _ := Activate(p, "bash")
	if !strings.Contains(bash, "mkdir -p .cache") {
		t.Errorf("bash activate missing hook content:\n%s", bash)
	}
	if strings.Contains(bash, "setup.fish") {
		t.Errorf("bash activate should not include a fish-only hook:\n%s", bash)
	}
	fish, _ := Activate(p, "fish")
	if !strings.Contains(fish, "source '/repo/hooks/setup.fish'") {
		t.Errorf("fish activate missing hook file source:\n%s", fish)
	}
	if strings.Contains(fish, "mkdir -p .cache") {
		t.Errorf("fish activate should not include a bash-only hook:\n%s", fish)
	}
}

func TestFishQuoteEscapes(t *testing.T) {
	if got := fishQuote(`it's a\test`); got != `'it\'s a\\test'` {
		t.Errorf("fishQuote = %q", got)
	}
}

func TestPathPrependOrder(t *testing.T) {
	p := basePlan()
	p.PathPrepend = []string{"/a", "/b", "/c"}
	out, _ := Activate(p, "zsh")
	// Rendered in reverse so that /a ends up first in PATH.
	ia := strings.Index(out, "'/a'")
	ib := strings.Index(out, "'/b'")
	ic := strings.Index(out, "'/c'")
	if !(ic < ib && ib < ia) {
		t.Errorf("prepend not emitted in reverse order: a=%d b=%d c=%d", ia, ib, ic)
	}
}

func TestDeactivateRestoresEnv(t *testing.T) {
	p := basePlan()
	p.EnvSet["FOO"] = "bar"
	p.EnvUnset = []string{"BAZ"}
	out, _ := Deactivate(p, "bash")
	if !strings.Contains(out, "__TAU_SAVE_ENV_FOO") {
		t.Errorf("deactivate missing FOO restore:\n%s", out)
	}
	if !strings.Contains(out, "__TAU_SAVE_ENV_BAZ") {
		t.Errorf("deactivate missing BAZ restore:\n%s", out)
	}
	if !strings.Contains(out, "unset TAUGRES_ACTIVE") {
		t.Errorf("deactivate does not clear built-ins:\n%s", out)
	}
}

func TestInlineContentFunctionRendered(t *testing.T) {
	p := basePlan()
	p.SourceFuncs["foobar"] = []model.SourceFunc{
		{Name: "foobar", Shells: []string{"bash"}, Content: "echo foobar"},
	}
	out, _ := Activate(p, "bash")
	if !strings.Contains(out, "foobar() {\necho foobar\n}") {
		t.Errorf("inline content not embedded as function body:\n%s", out)
	}
	if strings.Contains(out, "source") {
		t.Errorf("inline function should not source a file:\n%s", out)
	}
}

func TestFunctionRenderedForShell(t *testing.T) {
	p := basePlan()
	p.SourceFuncs["croot"] = []model.SourceFunc{
		{Name: "croot", Shells: []string{"bash", "zsh"}, File: "/repo/bin/croot.sh"},
		{Name: "croot", Shells: []string{"fish"}, File: "/repo/bin/croot.fish"},
	}
	out, _ := Activate(p, "bash")
	if !strings.Contains(out, "source '/repo/bin/croot.sh'") {
		t.Errorf("bash function should source .sh file:\n%s", out)
	}
	if strings.Contains(out, "croot.fish") {
		t.Errorf("bash activation should not reference fish file:\n%s", out)
	}
}
