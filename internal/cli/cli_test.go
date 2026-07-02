package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.gnkv.dev/taugres/internal/testutil"
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
	for _, f := range []string{"activate.bash", "activate.zsh", "deactivate.bash", "deactivate.zsh", "manifest.json"} {
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
