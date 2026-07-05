package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edganiukov/taugres/internal/testutil"
)

// run executes a tau command in wd and returns exit code, stdout, stderr.
func run(t *testing.T, wd string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	e := &Env{Args: args, Stdout: &out, Stderr: &errb, Wd: wd}
	code := Main(e)
	return code, out.String(), errb.String()
}

// isolate points trust storage (under the user config dir) at a temp dir.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

func TestInitCreatesWorkspace(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	code, out, _ := run(t, dir, "init")
	if code != 0 {
		t.Fatalf("init exit %d", code)
	}
	if !strings.Contains(out, "workspace.tg") {
		t.Errorf("unexpected output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workspace.tg")); err != nil {
		t.Error("workspace.tg not created")
	}
}

func TestInitNestedRejectsExisting(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	if code, _, _ := run(t, dir, "init"); code != 0 {
		t.Fatal("first init failed")
	}
	// A second init should fail because workspace.tg exists.
	code, _, errOut := run(t, dir, "init")
	if code == 0 {
		t.Error("expected failure re-initializing")
	}
	if !strings.Contains(errOut, "already exists") {
		t.Errorf("unexpected error: %s", errOut)
	}
}

func TestSyncRequiresTrust(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.env(\"A\", \"b\")\n")

	code, _, errOut := run(t, dir, "sync")
	if code == 0 {
		t.Fatal("sync should fail when untrusted")
	}
	if !strings.Contains(errOut, "allow") {
		t.Errorf("expected trust hint, got %s", errOut)
	}
}

func TestFullSyncFlow(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.env(\"A\", \"b\")\n")

	if code, _, errOut := run(t, dir, "allow"); code != 0 {
		t.Fatalf("allow failed: %s", errOut)
	}
	if code, _, errOut := run(t, dir, "sync"); code != 0 {
		t.Fatalf("sync failed: %s", errOut)
	}
	for _, f := range []string{"activate.bash", "activate.zsh", "deactivate.bash", "deactivate.zsh", "manifest"} {
		p := filepath.Join(dir, ".taugres", "gen", f)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing generated file %s", f)
		}
	}
	// .gitignore should be created.
	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || !strings.Contains(string(gi), ".taugres/") {
		t.Errorf("gitignore not written: %v", err)
	}

	// status should now report synced + trusted.
	code, out, _ := run(t, dir, "status")
	if code != 0 {
		t.Fatal("status failed")
	}
	if !strings.Contains(out, "synced:  yes") || !strings.Contains(out, "trust:   trusted") {
		t.Errorf("unexpected status:\n%s", out)
	}
}

func TestExecAppliesEnvAndPath(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteExec(t, dir, "bin/taugreet", "#!/bin/sh\necho \"greet:$TAU_EXEC_VAR\"\n")
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.env("TAU_EXEC_VAR", "hello")
shell.path.prepend("//bin")
`)
	// Untrusted: refused before running anything.
	if code, _, errOut := run(t, dir, "exec", "taugreet"); code == 0 || !strings.Contains(errOut, "trust") {
		t.Fatalf("exec should require trust, code=%d err=%s", code, errOut)
	}

	run(t, dir, "allow")
	// The project's bin/ is on PATH (found via prepend) and env vars are applied.
	code, out, errOut := run(t, dir, "exec", "--", "taugreet")
	if code != 0 {
		t.Fatalf("exec failed: %s", errOut)
	}
	if !strings.Contains(out, "greet:hello") {
		t.Errorf("exec stdout = %q, want it to contain greet:hello", out)
	}
}

func TestExecEnvStaticAndDynamic(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.env("STATIC", shell.exec("echo baked"))
shell.env("DYN", shell.exec("echo live", dynamic = True))
`)
	run(t, dir, "allow")
	if code, _, errOut := run(t, dir, "sync"); code != 0 {
		t.Fatalf("sync failed: %s", errOut)
	}

	// Static exec is baked into the activation script; dynamic is a command
	// substitution that runs in the shell on activation.
	act, err := os.ReadFile(filepath.Join(dir, ".taugres", "gen", "activate.bash"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(act); !strings.Contains(s, "export STATIC='baked'") {
		t.Errorf("static exec not baked into activate script:\n%s", s)
	}
	if s := string(act); !strings.Contains(s, `export DYN="$(echo live)"`) {
		t.Errorf("dynamic exec not rendered as substitution:\n%s", s)
	}

	// tau exec has no shell, so it resolves both static and dynamic vars itself.
	code, out, errOut := run(t, dir, "exec", "--", "sh", "-c", "echo S=$STATIC D=$DYN")
	if code != 0 {
		t.Fatalf("exec failed: %s", errOut)
	}
	if !strings.Contains(out, "S=baked") || !strings.Contains(out, "D=live") {
		t.Errorf("exec did not resolve exec-env vars: %q", out)
	}
}

func TestExecShellOverride(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("x")
shell.env("DYN", shell.exec("echo hi", dynamic = True, shell = "bash"))
`)
	run(t, dir, "allow")
	if code, _, errOut := run(t, dir, "sync"); code != 0 {
		t.Fatalf("sync failed: %s", errOut)
	}
	// A dynamic entry with shell="bash" is wrapped so it runs under bash, not the
	// activating shell.
	act, err := os.ReadFile(filepath.Join(dir, ".taugres", "gen", "activate.bash"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(act); !strings.Contains(s, `export DYN="$(bash -c 'echo hi')"`) {
		t.Errorf("dynamic shell override not rendered:\n%s", s)
	}
}

func TestExecPropagatesExitCode(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `project("x")`)
	run(t, dir, "allow")
	if code, _, _ := run(t, dir, "exec", "sh", "-c", "exit 7"); code != 7 {
		t.Errorf("exec exit code = %d, want 7", code)
	}
}

func TestExecNoCommand(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `project("x")`)
	run(t, dir, "allow")
	if code, _, errOut := run(t, dir, "exec"); code == 0 || !strings.Contains(errOut, "usage") {
		t.Errorf("exec with no command should print usage, code=%d err=%s", code, errOut)
	}
}

func TestManualSyncPrintsDoneLine(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"demo\")\n")
	run(t, dir, "allow")

	_, out, _ := run(t, dir, "sync")
	if !strings.Contains(out, "tau synced demo") {
		t.Errorf("manual sync should print a done line, got: %q", out)
	}
}

func TestUpdateUnknownName(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nmise.tool(\"go@1.26.2\")\n")

	code, _, errOut := run(t, dir, "update", "nope")
	if code == 0 {
		t.Fatal("update of an undeclared tool should fail")
	}
	if !strings.Contains(errOut, "not a declared") {
		t.Errorf("unexpected error: %s", errOut)
	}
}

func TestUpdatePinnedIsNoOp(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nmise.tool(\"go@1.26.2\")\n")

	// A pinned tool is steered to the config; no sync runs, so mise is not needed.
	code, out, _ := run(t, dir, "update", "go")
	if code != 0 {
		t.Fatalf("update exit %d", code)
	}
	if !strings.Contains(out, "pinned in the config") || !strings.Contains(out, "nothing to update") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestUpdateManagerQualifier(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	// Same name under two managers; both pinned so no sync (mise) is needed —
	// the pinned-skip path exercises which manager(s) a qualifier selects.
	testutil.WriteFile(t, dir, "workspace.tg",
		"project(\"x\")\npip.install(\"ruff@1.0\")\nuv.install(\"ruff@2.0\")\n")

	// Qualified: only the uv entry is considered.
	_, out, _ := run(t, dir, "update", "uv:ruff")
	if !strings.Contains(out, "ruff (uv) is pinned") || strings.Contains(out, "(pip)") {
		t.Errorf("uv:ruff should touch only uv, got:\n%s", out)
	}

	// Unqualified: both managers match.
	_, out, _ = run(t, dir, "update", "ruff")
	if !strings.Contains(out, "(pip)") || !strings.Contains(out, "(uv)") {
		t.Errorf("bare ruff should match both managers, got:\n%s", out)
	}

	// Wrong manager for the name.
	code, _, errOut := run(t, dir, "update", "npm:ruff")
	if code == 0 || !strings.Contains(errOut, "not a npm-managed") {
		t.Errorf("npm:ruff should be rejected, got code %d err %q", code, errOut)
	}
}

func TestSyncIfStaleSkipsWhenFresh(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"demo\")\n")
	run(t, dir, "allow")
	run(t, dir, "sync") // now fresh

	// --if-stale on a fresh project is a silent no-op (the hook's auto path).
	code, out, errOut := run(t, dir, "sync", "--if-stale")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "synced") {
		t.Errorf("--if-stale should be silent when fresh, got: %q", out)
	}
}

func TestDenyRevokesTrust(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	run(t, dir, "allow")
	run(t, dir, "deny")

	code, _, errOut := run(t, dir, "sync")
	if code == 0 {
		t.Error("sync should fail after deny")
	}
	_ = errOut
}

func TestCheckReportsErrors(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	// Invalid env var name is a hard error.
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.env(\"BAD-NAME\", \"v\")\n")

	code, _, errOut := run(t, dir, "check")
	if code != 1 {
		t.Fatalf("expected exit 1 for validation error, got %d", code)
	}
	if !strings.Contains(errOut, "invalid environment variable name") {
		t.Errorf("expected validation error:\n%s", errOut)
	}
}

func TestActivatePrintsScript(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.env(\"A\", \"b\")\n")
	run(t, dir, "allow")
	run(t, dir, "sync")

	// activate emits the script on stdout and does no work beyond that: no
	// staleness hashing (the hook re-syncs on change before calling activate;
	// `tau status` reports staleness), so stderr stays clean.
	code, out, errOut := run(t, dir, "activate", "bash")
	if code != 0 {
		t.Fatalf("activate exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "TAUGRES_ACTIVE") {
		t.Errorf("activate did not print script:\n%s", out)
	}
	if errOut != "" {
		t.Errorf("unexpected stderr: %s", errOut)
	}

	// Even with the config changed (stale), activate still just emits the current
	// script without warning or re-hashing.
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.env(\"A\", \"c\")\n")
	code, out, errOut = run(t, dir, "activate", "bash")
	if code != 0 {
		t.Fatalf("activate exit %d", code)
	}
	if !strings.Contains(out, "TAUGRES_ACTIVE") {
		t.Errorf("script still expected on stdout:\n%s", out)
	}
	if errOut != "" {
		t.Errorf("activate should not warn on the hot path, got stderr: %q", errOut)
	}
}

func TestActivateRejectsUnsupportedShell(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	code, _, errOut := run(t, dir, "activate", "powershell")
	if code == 0 {
		t.Error("expected failure for unsupported shell")
	}
	if !strings.Contains(errOut, "unsupported shell") {
		t.Errorf("unexpected error: %s", errOut)
	}
}

func TestActivateDefaultsToShellEnv(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.env(\"A\", \"b\")\n")
	run(t, dir, "allow")
	run(t, dir, "sync")

	// With no shell arg, tau uses $SHELL.
	t.Setenv("SHELL", "/bin/zsh")
	code, out, errOut := run(t, dir, "activate")
	if code != 0 {
		t.Fatalf("activate (no arg) exit %d: %s", code, errOut)
	}
	want, err := os.ReadFile(filepath.Join(dir, ".taugres", "gen", "activate.zsh"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("activate did not default to $SHELL (zsh):\n%s", out)
	}

	// Unset $SHELL: it must error and say to pass one.
	t.Setenv("SHELL", "")
	code, _, errOut = run(t, dir, "activate")
	if code == 0 || !strings.Contains(errOut, "SHELL is not set") {
		t.Errorf("expected an error when $SHELL is unset, got code=%d err=%q", code, errOut)
	}
}

func TestDeactivatePrintsScript(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nshell.env(\"A\", \"b\")\n")
	run(t, dir, "allow")
	run(t, dir, "sync")

	code, out, errOut := run(t, dir, "deactivate", "bash")
	if code != 0 {
		t.Fatalf("deactivate exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "restore environment") {
		t.Errorf("deactivate did not print the teardown script:\n%s", out)
	}
	if errOut != "" {
		t.Errorf("unexpected stderr: %s", errOut)
	}

	// deactivate is trust-gated like activate: a repo can commit its own
	// deactivate.<shell>, so tau refuses to emit repo bytes for an untrusted
	// project. The hook's auto-teardown handles a denied-while-active project
	// separately (it reads the script directly, not via this command).
	run(t, dir, "deny")
	code, out, errOut = run(t, dir, "deactivate", "bash")
	if code == 0 {
		t.Errorf("deactivate should refuse an untrusted project, got:\n%s", out)
	}
	if !strings.Contains(errOut, "not trusted") {
		t.Errorf("expected a not-trusted error after deny, got: %q", errOut)
	}
}

func TestHookOutput(t *testing.T) {
	isolate(t)
	code, out, _ := run(t, t.TempDir(), "hook", "zsh")
	if code != 0 {
		t.Fatal("hook failed")
	}
	if !strings.Contains(out, "_tau_hook") || !strings.Contains(out, "add-zsh-hook") {
		t.Errorf("unexpected hook:\n%s", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	isolate(t)
	code, _, _ := run(t, t.TempDir(), "bogus")
	if code != 2 {
		t.Errorf("expected exit 2 for unknown command, got %d", code)
	}
}

func TestCleanRemovesStateKeepsLock(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"demo\")\nshell.env(\"A\", \"b\")\n")
	if code, _, e := run(t, dir, "allow"); code != 0 {
		t.Fatalf("allow: %s", e)
	}
	if code, _, e := run(t, dir, "sync"); code != 0 {
		t.Fatalf("sync: %s", e)
	}
	// Seed a lockfile to confirm clean keeps it.
	lockPath := filepath.Join(dir, ".taugres.lock")
	if err := os.WriteFile(lockPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, e := run(t, dir, "clean"); code != 0 {
		t.Fatalf("clean: %s", e)
	}
	if _, err := os.Stat(filepath.Join(dir, ".taugres")); !os.IsNotExist(err) {
		t.Error(".taugres should be removed by clean")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Error("clean should keep .taugres.lock without --lock")
	}

	// Re-sync rebuilds; --lock also drops the lockfile.
	if code, _, e := run(t, dir, "sync"); code != 0 {
		t.Fatalf("resync: %s", e)
	}
	if code, _, e := run(t, dir, "clean", "--lock"); code != 0 {
		t.Fatalf("clean --lock: %s", e)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Error("clean --lock should remove .taugres.lock")
	}
}

func TestPruneRemovesOrphanedTrust(t *testing.T) {
	isolate(t)
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"demo\")\n")
	if code, _, e := run(t, dir, "allow"); code != 0 {
		t.Fatalf("allow: %s", e)
	}
	// A live project is not pruned (prune ignores cwd; it scans the trust store).
	if _, out, _ := run(t, t.TempDir(), "prune"); strings.Contains(out, "pruned") {
		t.Errorf("live project should not be pruned: %s", out)
	}
	// Remove the config; its trust record is now orphaned and prunable.
	if err := os.Remove(filepath.Join(dir, "workspace.tg")); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, t.TempDir(), "prune")
	if code != 0 || !strings.Contains(out, "pruned trust") {
		t.Errorf("expected orphaned record pruned, got code=%d out=%s", code, out)
	}
}
