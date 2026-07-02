package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edganiukov/taugres/internal/testutil"
)

// writeFakeMise installs a stub `mise` on PATH that understands `install` and
// `where`, backed by a fake install store. Returns nothing; adjusts PATH for
// the test via t.Setenv.
func writeFakeMise(t *testing.T, store string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake mise shell stub is POSIX-only")
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  install) exit 0 ;;\n" +
		"  where) echo \"" + store + "/$(echo $2 | tr '@' '/')\" ;;\n" +
		"esac\n"
	mise := filepath.Join(binDir, "mise")
	if err := os.WriteFile(mise, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestSyncSkipsUnchangedMiseInstall proves the per-tool staleness gate: a second
// sync with nothing changed must not re-invoke `mise install` (no network),
// because the tool is already present at its locked version.
func TestSyncSkipsUnchangedMiseInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake mise shell stub is POSIX-only")
	}
	isolate(t)

	store := testutil.TempWorkspace(t)
	logFile := filepath.Join(t.TempDir(), "install.log")
	binDir := t.TempDir()
	// Fake mise: `install` logs a line and creates the store bin dir; `where`
	// reports it. So the first sync installs (creating the dir); the cached bin
	// dir then lets the freshness check skip the second.
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  install) echo call >> \"" + logFile + "\"; mkdir -p \"" + store + "/node/1/bin\" ;;\n" +
		"  where) echo \"" + store + "/node/1\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "mise"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\nmise.tool(\"node@1\")\n")
	if code, _, e := run(t, dir, "allow"); code != 0 {
		t.Fatalf("allow: %s", e)
	}

	installs := func() int {
		data, _ := os.ReadFile(logFile)
		return strings.Count(string(data), "call")
	}

	if code, _, e := run(t, dir, "sync"); code != 0 {
		t.Fatalf("sync 1: %s", e)
	}
	n1 := installs()
	if n1 == 0 {
		t.Fatal("first sync should have run mise install")
	}
	if code, _, e := run(t, dir, "sync"); code != 0 {
		t.Fatalf("sync 2: %s", e)
	}
	if n2 := installs(); n2 != n1 {
		t.Errorf("second sync re-ran mise install (%d -> %d); expected it to be skipped", n1, n2)
	}
}

func TestSyncPrependsMiseToolBinDir(t *testing.T) {
	isolate(t)

	// Fake mise install store containing node's binaries.
	store := testutil.TempWorkspace(t)
	testutil.WriteExec(t, store, "node/22.11.0/bin/node", "#!/bin/sh\necho node\n")
	testutil.WriteExec(t, store, "node/22.11.0/bin/npm", "#!/bin/sh\necho npm\n")
	writeFakeMise(t, store)

	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"demo\")\nmise.tool(\"node@22.11.0\")\n")

	if code, _, e := run(t, dir, "allow"); code != 0 {
		t.Fatalf("allow: %s", e)
	}
	code, out, errOut := run(t, dir, "sync")
	if code != 0 {
		t.Fatalf("sync failed: %s\n%s", errOut, out)
	}

	// Activation prepends the mise tool's real store bin dir to PATH (the way
	// `mise activate` exposes tools) — no symlink/wrapper farm.
	act, err := os.ReadFile(filepath.Join(dir, ".taugres", "gen", "activate.bash"))
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(store, "node", "22.11.0", "bin")
	if !strings.Contains(string(act), wantPath) {
		t.Errorf("activate.bash does not prepend %s:\n%s", wantPath, act)
	}
	// The tool dir is recorded in the manifest for staleness checks.
	manifest, err := os.ReadFile(filepath.Join(dir, ".taugres", "gen", "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "tooldir:"+wantPath) {
		t.Errorf("manifest missing tooldir %s:\n%s", wantPath, manifest)
	}
}

func TestSyncFailsWhenMiseMissing(t *testing.T) {
	isolate(t)
	// Ensure no mise is found.
	t.Setenv("PATH", t.TempDir())

	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"demo\")\nmise.tool(\"node@22\")\n")
	run(t, dir, "allow")

	code, _, errOut := run(t, dir, "sync")
	if code == 0 {
		t.Fatal("sync should fail when mise is required but missing")
	}
	if !strings.Contains(errOut, "mise is required") || !strings.Contains(errOut, "mise.jdx.dev") {
		t.Errorf("unexpected error: %s", errOut)
	}
}
