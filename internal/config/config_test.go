package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edganiukov/taugres/internal/discover"
	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/testutil"
)

func evalWorkspace(t *testing.T, dir string) *Result {
	t.Helper()
	d, err := discover.Discover(dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	res, err := Evaluate(d)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return res
}

func TestEnvPathAliasFunction(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("demo")
shell.env("DATABASE_URL", "postgres://localhost/app")
shell.unset("PYTHONPATH")
shell.path.prepend("//node_modules/.bin")
shell.path.append("//scripts")
shell.alias("ll", "ls -lah")
shell.fn("croot", shells = ["bash", "zsh"], file = "//bin/croot.sh")
`)
	testutil.WriteFile(t, dir, "bin/croot.sh", "cd .\n")

	res := evalWorkspace(t, dir)
	p := res.Plan

	if p.ProjectName != "demo" {
		t.Errorf("ProjectName = %q", p.ProjectName)
	}
	if p.EnvSet["DATABASE_URL"] != "postgres://localhost/app" {
		t.Errorf("env not set: %v", p.EnvSet)
	}
	if len(p.EnvUnset) != 1 || p.EnvUnset[0] != "PYTHONPATH" {
		t.Errorf("unset = %v", p.EnvUnset)
	}
	// No automatic bin/: node_modules/.bin is the only prepend.
	if len(p.PathPrepend) != 1 || p.PathPrepend[0] != filepath.Join(dir, "node_modules", ".bin") {
		t.Errorf("PathPrepend = %v", p.PathPrepend)
	}
	if p.PathAppend[0] != filepath.Join(dir, "scripts") {
		t.Errorf("PathAppend = %v", p.PathAppend)
	}
	if p.Aliases["ll"] != "ls -lah" {
		t.Errorf("alias = %v", p.Aliases)
	}
	fn := p.SourceFuncs["croot"]
	if len(fn) != 1 || fn[0].File != filepath.Join(dir, "bin", "croot.sh") {
		t.Errorf("sourceFunc = %+v", fn)
	}
}

func TestShellHook(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.hook(shells = ["bash", "zsh"], content = "mkdir -p .cache")
shell.hook(shells = ["fish"], file = "//hooks/setup.fish")
`)
	testutil.WriteFile(t, dir, "hooks/setup.fish", "echo hi\n")
	res := evalWorkspace(t, dir)
	hooks := res.Plan.Hooks
	if len(hooks) != 2 {
		t.Fatalf("Hooks = %+v", hooks)
	}
	if hooks[0].Content != "mkdir -p .cache" || len(hooks[0].Shells) != 2 {
		t.Errorf("hook[0] = %+v", hooks[0])
	}
	if hooks[1].File != filepath.Join(dir, "hooks", "setup.fish") {
		t.Errorf("hook[1] file = %q", hooks[1].File)
	}
}

func TestShellHookRequiresExactlyOneBody(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.hook(shells = [\"bash\"])\n")
	d, _ := discover.Discover(dir)
	if _, err := Evaluate(d); err == nil {
		t.Error("expected error when neither file nor content is given")
	}
}

func TestFnSourceInlineContent(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.fn("foobar", shells = ["bash"], content = "echo foobar")
`)
	res := evalWorkspace(t, dir)
	fns := res.Plan.SourceFuncs["foobar"]
	if len(fns) != 1 {
		t.Fatalf("SourceFuncs = %+v", fns)
	}
	if fns[0].Content != "echo foobar" {
		t.Errorf("Content = %q", fns[0].Content)
	}
	if fns[0].File != "" {
		t.Errorf("File should be empty for inline content, got %q", fns[0].File)
	}
}

func TestFnSourceRequiresExactlyOneBody(t *testing.T) {
	cases := map[string]string{
		"neither": `shell.fn("f", shells = ["bash"])`,
		"both":    `shell.fn("f", shells = ["bash"], file = "//f.sh", content = "echo hi")`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			dir := testutil.TempWorkspace(t)
			testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n"+line+"\n")
			d, _ := discover.Discover(dir)
			if _, err := Evaluate(d); err == nil {
				t.Error("expected error for invalid fn.source body")
			}
		})
	}
}

func TestBinNotAutoAdded(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	// A bin/ directory that exists must NOT be added to PATH implicitly.
	testutil.WriteExec(t, dir, "bin/greet", "#!/bin/sh\necho hi\n")
	testutil.WriteFile(t, dir, "workspace.tg", `project("x")`)
	res := evalWorkspace(t, dir)
	if len(res.Plan.PathPrepend) != 0 || len(res.Plan.PathAppend) != 0 {
		t.Errorf("expected empty PATH additions, got prepend=%v append=%v",
			res.Plan.PathPrepend, res.Plan.PathAppend)
	}
}

func TestToolSpecArrayForm(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
mise.tool(["go@1.26.2", "python", "node@22"])
pip.install(["ruff@0.6.9", "rich"])
npm.install(["typescript@5.6.2", "@angular/cli@17", "cowsay"])
`)
	res := evalWorkspace(t, dir)
	p := res.Plan

	wantMise := []model.MiseTool{{Name: "go", Version: "1.26.2"}, {Name: "python"}, {Name: "node", Version: "22"}}
	if len(p.MiseTools) != 3 {
		t.Fatalf("MiseTools = %+v", p.MiseTools)
	}
	for i, w := range wantMise {
		if p.MiseTools[i] != w {
			t.Errorf("MiseTools[%d] = %+v, want %+v", i, p.MiseTools[i], w)
		}
	}
	if p.PipPackages[0] != (model.Package{Name: "ruff", Version: "0.6.9"}) || p.PipPackages[1] != (model.Package{Name: "rich"}) {
		t.Errorf("PipPackages = %+v", p.PipPackages)
	}
	// npm scoped name keeps its leading "@"; version comes from the last "@".
	want := []model.Package{{Name: "typescript", Version: "5.6.2"}, {Name: "@angular/cli", Version: "17"}, {Name: "cowsay"}}
	for i, w := range want {
		if p.NpmPackages[i] != w {
			t.Errorf("NpmPackages[%d] = %+v, want %+v", i, p.NpmPackages[i], w)
		}
	}
}

func TestToolSpecSingleEmbeddedVersion(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
mise.tool("go@1.26.2")
`)
	res := evalWorkspace(t, dir)
	if len(res.Plan.MiseTools) != 1 || res.Plan.MiseTools[0] != (model.MiseTool{Name: "go", Version: "1.26.2"}) {
		t.Errorf("MiseTools = %+v", res.Plan.MiseTools)
	}
}

func TestToolSpecRejectsSecondArg(t *testing.T) {
	// The old two-arg form is gone: versions are given as "name@version".
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nmise.tool(\"go\", \"1.26.2\")\n")
	d, _ := discover.Discover(dir)
	if _, err := Evaluate(d); err == nil {
		t.Error("expected error for the removed two-arg form")
	}
}

func TestExists(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "present.txt", "hi\n")
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
if exists("//present.txt"):
    shell.env("HAS", "yes")
if not exists("//absent.txt"):
    shell.env("MISSING", "yes")
`)
	p := evalWorkspace(t, dir).Plan
	if p.EnvSet["HAS"] != "yes" {
		t.Errorf("exists(present) should be true")
	}
	if p.EnvSet["MISSING"] != "yes" {
		t.Errorf("exists(absent) should be false")
	}

	// A bare relative path is rejected, like other path arguments.
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nexists(\"present.txt\")\n")
	d, _ := discover.Discover(dir)
	if _, err := Evaluate(d); err == nil {
		t.Error("expected error for bare relative path")
	}
}

func TestWhich(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	// A fake executable on a PATH we control.
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "taufake")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
p = which("taufake")
if p:
    shell.env("FAKE", p)
if not which("tau-definitely-absent-xyz"):
    shell.env("NOPE", "yes")
`)
	p := evalWorkspace(t, dir).Plan
	if p.EnvSet["FAKE"] != fake {
		t.Errorf("which(taufake) = %q, want %q", p.EnvSet["FAKE"], fake)
	}
	if p.EnvSet["NOPE"] != "yes" {
		t.Errorf("which(absent) should be None/falsy")
	}
}

func TestEnvProbe(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	t.Setenv("TAU_TEST_CI", "1")
	t.Setenv("TAU_TEST_EMPTY", "")
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
if env("TAU_TEST_CI"):
    shell.env("IN_CI", "yes")
shell.env("PROFILE", env("TAU_TEST_PROFILE", "dev"))
# A set-but-empty var is falsy.
if not env("TAU_TEST_EMPTY"):
    shell.env("EMPTY_FALSY", "yes")
`)
	res := evalWorkspace(t, dir)
	p := res.Plan
	if p.EnvSet["IN_CI"] != "yes" {
		t.Errorf("env(set) should be truthy")
	}
	if p.EnvSet["PROFILE"] != "dev" {
		t.Errorf("env(unset, default) = %q, want dev", p.EnvSet["PROFILE"])
	}
	if p.EnvSet["EMPTY_FALSY"] != "yes" {
		t.Errorf("env(set-but-empty) should be falsy")
	}

	// The observations are recorded as probes for stale detection, with the value
	// hashed (never stored raw).
	var ci *model.Probe
	for i := range res.Probes {
		if res.Probes[i].Kind == "env" && res.Probes[i].Arg == "TAU_TEST_CI" {
			ci = &res.Probes[i]
		}
	}
	if ci == nil {
		t.Fatalf("env probe not recorded: %+v", res.Probes)
	}
	if ci.Result == "" || ci.Result == "1" {
		t.Errorf("env probe result should be a value hash, got %q", ci.Result)
	}
	if ci.Result != model.EnvProbeResult("1", true) {
		t.Errorf("env probe result = %q, want hash of %q", ci.Result, "1")
	}
}

func TestDotenv(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, ".env", `
# a comment
export TOKEN=abc123
DATABASE_URL=postgres://localhost/app
QUOTED="a b	c"
LITERAL='keep $HOME literal'
`)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.dotenv("//.env")
shell.env("DERIVED", "$TOKEN")
`)
	res := evalWorkspace(t, dir)
	p := res.Plan
	if p.EnvSet["TOKEN"] != "abc123" {
		t.Errorf("TOKEN = %q", p.EnvSet["TOKEN"])
	}
	if p.EnvSet["DATABASE_URL"] != "postgres://localhost/app" {
		t.Errorf("DATABASE_URL = %q", p.EnvSet["DATABASE_URL"])
	}
	if p.EnvSet["QUOTED"] != "a b\tc" {
		t.Errorf("QUOTED = %q", p.EnvSet["QUOTED"])
	}
	if p.EnvSet["LITERAL"] != "keep $HOME literal" {
		t.Errorf("LITERAL = %q (single quotes should be literal)", p.EnvSet["LITERAL"])
	}
	// A later shell.env can expand a var loaded from the dotenv file.
	if p.EnvSet["DERIVED"] != "abc123" {
		t.Errorf("DERIVED = %q, want abc123", p.EnvSet["DERIVED"])
	}
	// The .env file is tracked as a config input for stale detection.
	if len(res.DotenvFiles) != 1 || res.DotenvFiles[0] != filepath.Join(dir, ".env") {
		t.Errorf("DotenvFiles = %v", res.DotenvFiles)
	}
}

func TestDotenvMissingFileErrors(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.dotenv(\"//.env\")\n")
	d, _ := discover.Discover(dir)
	if _, err := Evaluate(d); err == nil {
		t.Error("expected error for missing .env file")
	}
}

func TestExecEnv(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
sha = shell.exec("echo abc123")
shell.env("GIT_SHA", sha)
shell.env("NOW", shell.exec("date", dynamic = True))
`)
	p := evalWorkspace(t, dir).Plan
	if len(p.DeferredEnv) != 2 {
		t.Fatalf("DeferredEnv = %+v", p.DeferredEnv)
	}
	git := p.DeferredEnv[0]
	if git.Name != "GIT_SHA" || len(git.Segments) != 1 {
		t.Fatalf("GIT_SHA = %+v", git)
	}
	if s := git.Segments[0]; s.Kind != model.SegExec || s.Value != "echo abc123" || s.Dynamic {
		t.Errorf("static exec segment = %+v", s)
	}
	now := p.DeferredEnv[1]
	if s := now.Segments[0]; now.Name != "NOW" || s.Value != "date" || !s.Dynamic {
		t.Errorf("dynamic exec = %+v / %+v", now, s)
	}
	// The command must NOT run during evaluation, so no value is baked into EnvSet.
	if _, ok := p.EnvSet["GIT_SHA"]; ok {
		t.Errorf("shell.exec must not run at eval; EnvSet baked GIT_SHA=%q", p.EnvSet["GIT_SHA"])
	}
}

func TestExecEmptyCommandErrors(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.env(\"X\", shell.exec(\"   \"))\n")
	d, _ := discover.Discover(dir)
	if _, err := Evaluate(d); err == nil {
		t.Error("expected error for empty shell.exec command")
	}
}

func TestEnvRejectsNonStringValue(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.env(\"X\", 42)\n")
	d, _ := discover.Discover(dir)
	if _, err := Evaluate(d); err == nil {
		t.Error("expected error for non-string/non-exec shell.env value")
	}
}

func TestExecShellOption(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.env("A", shell.exec("echo a", shell = "bash"))
shell.env("B", shell.exec("echo b", dynamic = True, shell = "bash"))
shell.env("C", shell.exec("echo c"))
`)
	p := evalWorkspace(t, dir).Plan
	if len(p.DeferredEnv) != 3 {
		t.Fatalf("DeferredEnv = %+v", p.DeferredEnv)
	}
	if s := p.DeferredEnv[0].Segments[0]; s.Shell != "bash" {
		t.Errorf("A shell = %q, want bash", s.Shell)
	}
	if s := p.DeferredEnv[1].Segments[0]; s.Shell != "bash" || !s.Dynamic {
		t.Errorf("B = %+v, want shell=bash dynamic=true", s)
	}
	if s := p.DeferredEnv[2].Segments[0]; s.Shell != "" {
		t.Errorf("C shell = %q, want empty (local default)", s.Shell)
	}
}

func TestMiseWhere(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
mise.tool("node@22.11.0")
shell.env("NODE_BIN", mise.where("node"))
`)
	p := evalWorkspace(t, dir).Plan
	if len(p.DeferredEnv) != 1 || p.DeferredEnv[0].Name != "NODE_BIN" {
		t.Fatalf("DeferredEnv = %+v", p.DeferredEnv)
	}
	segs := p.DeferredEnv[0].Segments
	if len(segs) != 1 || segs[0].Kind != model.SegWhere || segs[0].Value != "node" {
		t.Errorf("segments = %+v", segs)
	}
	// Deferred: not resolved (no mise call) during evaluation.
	if _, ok := p.EnvSet["NODE_BIN"]; ok {
		t.Errorf("mise.where must not resolve at eval; EnvSet baked NODE_BIN=%q", p.EnvSet["NODE_BIN"])
	}
}

func TestMiseWhereSuffix(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
mise.tool("go")
shell.env("GO", mise.where("go") + "/go")
`)
	p := evalWorkspace(t, dir).Plan
	if len(p.DeferredEnv) != 1 {
		t.Fatalf("DeferredEnv = %+v", p.DeferredEnv)
	}
	// `+` composes into two segments: the where, then the literal suffix.
	segs := p.DeferredEnv[0].Segments
	if len(segs) != 2 {
		t.Fatalf("segments = %+v", segs)
	}
	if segs[0].Kind != model.SegWhere || segs[0].Value != "go" {
		t.Errorf("segment 0 = %+v, want where(go)", segs[0])
	}
	if segs[1].Kind != model.SegLiteral || segs[1].Value != "/go" {
		t.Errorf("segment 1 = %+v, want literal(/go)", segs[1])
	}
}

func TestDeferredCompose(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
mise.tool("go")
shell.env("MIX", "pre-" + shell.exec("echo x") + "-post")
shell.env("COMBO", mise.where("go") + ":" + shell.exec("id -un"))
`)
	p := evalWorkspace(t, dir).Plan
	if len(p.DeferredEnv) != 2 {
		t.Fatalf("DeferredEnv = %+v", p.DeferredEnv)
	}
	mix := p.DeferredEnv[0].Segments
	if len(mix) != 3 ||
		mix[0].Kind != model.SegLiteral || mix[0].Value != "pre-" ||
		mix[1].Kind != model.SegExec || mix[1].Value != "echo x" ||
		mix[2].Kind != model.SegLiteral || mix[2].Value != "-post" {
		t.Errorf("MIX segments = %+v", mix)
	}
	combo := p.DeferredEnv[1].Segments
	if len(combo) != 3 ||
		combo[0].Kind != model.SegWhere || combo[0].Value != "go" ||
		combo[1].Kind != model.SegLiteral || combo[1].Value != ":" ||
		combo[2].Kind != model.SegExec || combo[2].Value != "id -un" {
		t.Errorf("COMBO segments = %+v", combo)
	}
}

func TestMiseJobs(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	// Default is defaultMiseJobs when mise.jobs is not called.
	testutil.WriteFile(t, dir, "workspace.tg", `project("x")`)
	if got := evalWorkspace(t, dir).Plan.MiseJobs; got != defaultMiseJobs {
		t.Errorf("default MiseJobs = %d, want %d", got, defaultMiseJobs)
	}
	// Overridable.
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nmise.jobs(3)\n")
	if got := evalWorkspace(t, dir).Plan.MiseJobs; got != 3 {
		t.Errorf("MiseJobs = %d, want 3", got)
	}
	// Must be >= 1.
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nmise.jobs(0)\n")
	d, _ := discover.Discover(dir)
	if _, err := Evaluate(d); err == nil {
		t.Error("expected error for mise.jobs(0)")
	}
}

func TestMiseToolsRecorded(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
mise.tool("node@22.11.0")
mise.tool("ripgrep")
shell.path.prepend("//scripts")
`)
	res := evalWorkspace(t, dir)
	p := res.Plan

	if len(p.MiseTools) != 2 {
		t.Fatalf("MiseTools = %+v", p.MiseTools)
	}
	if p.MiseTools[0].Name != "node" || p.MiseTools[0].Version != "22.11.0" {
		t.Errorf("tool[0] = %+v", p.MiseTools[0])
	}
	if p.MiseTools[1].Name != "ripgrep" || p.MiseTools[1].Version != "" {
		t.Errorf("tool[1] = %+v", p.MiseTools[1])
	}
	// mise bin dirs are added to PATH at sync time (their versioned store paths
	// aren't known at eval); only the user's // path additions appear here.
	if len(p.PathPrepend) != 1 || p.PathPrepend[0] != filepath.Join(dir, "scripts") {
		t.Errorf("PathPrepend = %v, want [%s]", p.PathPrepend, filepath.Join(dir, "scripts"))
	}
}

func TestPipInstallAutoPrependsVenvBin(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
pip.install("requests@2.31.0")
pip.install("rich")
`)
	res := evalWorkspace(t, dir)
	p := res.Plan

	if len(p.PipPackages) != 2 {
		t.Fatalf("PipPackages = %+v", p.PipPackages)
	}
	if p.PipPackages[0].Name != "requests" || p.PipPackages[0].Version != "2.31.0" {
		t.Errorf("pkg[0] = %+v", p.PipPackages[0])
	}
	if p.PipPackages[1].Name != "rich" || p.PipPackages[1].Version != "" {
		t.Errorf("pkg[1] = %+v", p.PipPackages[1])
	}
	wantPip := filepath.Join(dir, ".taugres", "tools", "pip")
	if p.PipDir != wantPip {
		t.Errorf("PipDir = %q, want %q", p.PipDir, wantPip)
	}
	// pip implies an implicit mise `python` tool (added to PATH at sync time).
	if !hasMiseTool(p.MiseTools, "python") {
		t.Errorf("expected implicit mise python tool, got %+v", p.MiseTools)
	}
	wantBin := filepath.Join(wantPip, "bin")
	if len(p.PathPrepend) != 1 || p.PathPrepend[0] != wantBin {
		t.Errorf("PathPrepend = %v, want [%s]", p.PathPrepend, wantBin)
	}
}

func TestUvInstallImpliesToolchainAndPrependsBin(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
uv.install(["ruff@0.6.9", "rich"])
`)
	res := evalWorkspace(t, dir)
	p := res.Plan
	if len(p.UvPackages) != 2 || p.UvPackages[0] != (model.Package{Name: "ruff", Version: "0.6.9"}) {
		t.Fatalf("UvPackages = %+v", p.UvPackages)
	}
	wantUv := filepath.Join(dir, ".taugres", "tools", "uv")
	if p.UvDir != wantUv {
		t.Errorf("UvDir = %q, want %q", p.UvDir, wantUv)
	}
	// uv implies both an implicit mise `python` and `uv` tool.
	if !hasMiseTool(p.MiseTools, "python") || !hasMiseTool(p.MiseTools, "uv") {
		t.Errorf("expected implicit mise python + uv tools, got %+v", p.MiseTools)
	}
	wantBin := filepath.Join(wantUv, "bin")
	if len(p.PathPrepend) != 1 || p.PathPrepend[0] != wantBin {
		t.Errorf("PathPrepend = %v, want [%s]", p.PathPrepend, wantBin)
	}
}

func TestNpmInstallAutoPrependsNpmBin(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
npm.install("typescript@5.6.2")
npm.install("cowsay")
`)
	res := evalWorkspace(t, dir)
	p := res.Plan
	if len(p.NpmPackages) != 2 {
		t.Fatalf("NpmPackages = %+v", p.NpmPackages)
	}
	wantNpm := filepath.Join(dir, ".taugres", "tools", "npm")
	if p.NpmDir != wantNpm {
		t.Errorf("NpmDir = %q, want %q", p.NpmDir, wantNpm)
	}
	// npm implies an implicit mise `node` tool (added to PATH at sync time).
	if !hasMiseTool(p.MiseTools, "node") {
		t.Errorf("expected implicit mise node tool, got %+v", p.MiseTools)
	}
	wantBin := filepath.Join(wantNpm, "bin")
	if len(p.PathPrepend) != 1 || p.PathPrepend[0] != wantBin {
		t.Errorf("PathPrepend = %v, want [%s]", p.PathPrepend, wantBin)
	}
}

func TestPipNpmPrependProjectBins(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
mise.tool("node@22")
pip.install("rich")
npm.install("cowsay")
`)
	res := evalWorkspace(t, dir)
	p := res.Plan
	// At eval time, only the project-local pip/npm bins are in PATH; the mise
	// node bin dir is prepended at sync time.
	want := []string{
		filepath.Join(dir, ".taugres", "tools", "pip", "bin"),
		filepath.Join(dir, ".taugres", "tools", "npm", "bin"),
	}
	if len(p.PathPrepend) != 2 {
		t.Fatalf("PathPrepend = %v", p.PathPrepend)
	}
	for i, w := range want {
		if p.PathPrepend[i] != w {
			t.Errorf("PathPrepend[%d] = %s, want %s", i, p.PathPrepend[i], w)
		}
	}
}

func TestNoToolsNoPrepend(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `project("x")`)
	res := evalWorkspace(t, dir)
	if res.Plan.PipDir != "" || res.Plan.NpmDir != "" {
		t.Errorf("expected no tool dirs, got pip=%q npm=%q", res.Plan.PipDir, res.Plan.NpmDir)
	}
	if len(res.Plan.PathPrepend) != 0 {
		t.Errorf("no PATH prepend expected, got %v", res.Plan.PathPrepend)
	}
}

func TestLoadHelperModule(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "taugres/lib/node.tg", `
def node_project():
    shell.env("COREPACK_ENABLE_DOWNLOAD_PROMPT", "0")
    shell.alias("pn", "pnpm")
`)
	testutil.WriteFile(t, dir, "workspace.tg", `
load("//taugres/lib/node.tg", "node_project")
project("app")
node_project()
`)
	res := evalWorkspace(t, dir)
	if res.Plan.EnvSet["COREPACK_ENABLE_DOWNLOAD_PROMPT"] != "0" {
		t.Errorf("module env not applied: %v", res.Plan.EnvSet)
	}
	if res.Plan.Aliases["pn"] != "pnpm" {
		t.Errorf("module alias not applied")
	}
	if len(res.LoadedModules) != 1 {
		t.Errorf("expected 1 loaded module, got %v", res.LoadedModules)
	}
}

func TestEnvValueExpansion(t *testing.T) {
	t.Setenv("TAU_TEST_WHO", "world")
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.env("BASE", "/opt/app")
shell.env("BIN", "$BASE/bin")
shell.env("WHO", "$TAU_TEST_WHO")
shell.env("MISSING", "[${TAU_NOT_SET}]")
`)
	res := evalWorkspace(t, dir)
	got := res.Plan.EnvSet
	if got["BIN"] != "/opt/app/bin" {
		t.Errorf("BIN should reference earlier env var: %q", got["BIN"])
	}
	if got["WHO"] != "world" {
		t.Errorf("WHO should expand from process env: %q", got["WHO"])
	}
	if got["MISSING"] != "[]" {
		t.Errorf("unset var should expand to empty: %q", got["MISSING"])
	}
}

func TestRelativeImport(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "lib/common.tg", `
def common():
    shell.env("FROM_REL", "yes")
    shell.alias("g", "git")
`)
	testutil.WriteFile(t, dir, "workspace.tg", `
load("./lib/common.tg", "common")
project("app")
common()
`)
	res := evalWorkspace(t, dir)
	if res.Plan.EnvSet["FROM_REL"] != "yes" || res.Plan.Aliases["g"] != "git" {
		t.Errorf("relative import not applied: env=%v aliases=%v", res.Plan.EnvSet, res.Plan.Aliases)
	}
	if len(res.LoadedModules) != 1 || filepath.Base(res.LoadedModules[0]) != "common.tg" {
		t.Errorf("expected common.tg tracked as loaded module, got %v", res.LoadedModules)
	}
}

func TestRelativeImportChainAndParent(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	// base.tg loads its sibling common.tg via "./"; workspace loads base via a
	// nested path; common is also reachable via "../" from a deeper module.
	testutil.WriteFile(t, dir, "lib/common.tg", "def common():\n    shell.env(\"C\", \"1\")\n")
	testutil.WriteFile(t, dir, "lib/base.tg", `
load("./common.tg", "common")
def base():
    common()
    shell.env("B", "1")
`)
	testutil.WriteFile(t, dir, "lib/nested/deep.tg", `
load("../common.tg", "common")
def deep():
    common()
    shell.env("D", "1")
`)
	testutil.WriteFile(t, dir, "workspace.tg", `
load("./lib/base.tg", "base")
load("./lib/nested/deep.tg", "deep")
project("app")
base()
deep()
`)
	res := evalWorkspace(t, dir)
	for _, k := range []string{"B", "C", "D"} {
		if res.Plan.EnvSet[k] != "1" {
			t.Errorf("expected %s=1 via relative imports, got %v", k, res.Plan.EnvSet)
		}
	}
}

func TestBareRelativeImportRejected(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "lib.tg", "x = 1\n")
	testutil.WriteFile(t, dir, "workspace.tg", "load(\"lib.tg\", \"x\")\nproject(\"d\")\n")
	d, _ := discover.Discover(dir)
	if _, err := Evaluate(d); err == nil {
		t.Error("expected bare (non-anchored, non-relative) import to be rejected")
	}
}

func TestInvalidPathIncludesSourceLocation(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.path.prepend("relative/bad")
`)
	d, _ := discover.Discover(dir)
	_, err := Evaluate(d)
	if err == nil {
		t.Fatal("expected error for relative path")
	}
	if !strings.Contains(err.Error(), "workspace.tg") {
		t.Errorf("error missing source location: %v", err)
	}
}

func TestRemoteImportRejected(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
load("https://evil.example/mod.tg", "x")
project("x")
`)
	d, _ := discover.Discover(dir)
	_, err := Evaluate(d)
	if err == nil || !strings.Contains(err.Error(), "remote") {
		t.Errorf("expected remote import rejection, got %v", err)
	}
}

func TestPlatformConditional(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
if platform.os == "linux" or platform.os == "darwin":
    shell.env("HAS_OS", "yes")
`)
	res := evalWorkspace(t, dir)
	if res.Plan.EnvSet["HAS_OS"] != "yes" {
		t.Errorf("platform conditional failed: %v", res.Plan.EnvSet)
	}
}

func TestPathPrependDeduplicated(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.path.prepend("//bin")
shell.path.prepend("//bin")
`)
	res := evalWorkspace(t, dir)
	count := 0
	want := filepath.Join(dir, "bin")
	for _, p := range res.Plan.PathPrepend {
		if p == want {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected bin/ once in PathPrepend, got %d in %v", count, res.Plan.PathPrepend)
	}
}
