package mise

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/testutil"
)

// binPathsFail makes the fakeMise stub's `bin-paths` exit non-zero with an
// error on stderr, exercising the error path.
const binPathsFail = "\x00FAIL"

// fakeMise installs a stub mise whose `install` echoes two lines and whose
// `where` points at store. `bin-paths` (invoked as `bin-paths --silent <ref>`)
// prints binPathsOut when non-empty; when empty it prints nothing and exits 0,
// as real mise does for a ref it cannot resolve, exercising the `mise where`
// fallback; binPathsFail makes it fail. It sets Binary to the stub for the
// test.
func fakeMise(t *testing.T, store, binPathsOut string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake mise stub is POSIX-only")
	}
	dir := t.TempDir()
	binPathsCase := "  bin-paths) : ;;\n"
	switch binPathsOut {
	case "":
	case binPathsFail:
		binPathsCase = "  bin-paths) echo \"mise ERROR boom\" >&2; exit 1 ;;\n"
	default:
		binPathsCase = "  bin-paths) echo \"" + binPathsOut + "\" ;;\n"
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  install) echo \"downloading $2\"; echo \"installed $2\" ;;\n" +
		"  where) echo \"" + store + "/$(echo $2 | tr '@' '/')\" ;;\n" +
		binPathsCase +
		"esac\n"
	bin := filepath.Join(dir, "mise")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := Binary
	Binary = bin
	t.Cleanup(func() { Binary = old })
}

func TestInstallReturnsResolvedAndStreams(t *testing.T) {
	store := testutil.TempWorkspace(t)
	testutil.WriteExec(t, store, "node/22/bin/node", "#!/bin/sh\n")
	fakeMise(t, store, filepath.Join(store, "node", "22", "bin"))

	// mise writes its raw output to the provided writer; line prefixing is the
	// caller's responsibility (see internal/ui Reporter/LinePrefixer).
	var out bytes.Buffer
	installed, err := Install(context.Background(), []model.MiseTool{{Name: "node", Version: "22"}}, 0, false, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 {
		t.Fatalf("installed = %+v", installed)
	}
	ins := installed[0]
	if ins.Name != "node" || ins.Resolved != "22" || ins.BinDir != filepath.Join(store, "node", "22", "bin") {
		t.Errorf("unexpected installed: %+v", ins)
	}
	if !strings.Contains(out.String(), "downloading node@22") {
		t.Errorf("expected mise output streamed, got:\n%s", out.String())
	}
}

func TestInstallQuietWhenOutNil(t *testing.T) {
	store := testutil.TempWorkspace(t)
	testutil.WriteExec(t, store, "node/22/bin/node", "#!/bin/sh\n")
	fakeMise(t, store, "")

	if _, err := Install(context.Background(), []model.MiseTool{{Name: "node", Version: "22"}}, 0, false, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInstallPrefersMiseBinPaths(t *testing.T) {
	store := testutil.TempWorkspace(t)
	// Nested aqua layout: the authoritative `mise bin-paths` answer must win,
	// not the install dir `mise where` reports.
	testutil.WriteExec(t, store, "aqua-syncthing-syncthing/2.1.2/syncthing-macos-arm64-v2.1.2/syncthing", "#!/bin/sh\n")
	want := filepath.Join(store, "aqua-syncthing-syncthing", "2.1.2", "syncthing-macos-arm64-v2.1.2")
	fakeMise(t, store, want)

	tools := []model.MiseTool{{Name: "aqua:syncthing/syncthing", Version: "2.1.2"}}
	installed, err := Install(context.Background(), tools, 0, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0].BinDir != want {
		t.Errorf("installed = %+v, want BinDir %q", installed, want)
	}
}

func TestInstallFallsBackToWhereWhenBinPathsEmpty(t *testing.T) {
	store := testutil.TempWorkspace(t)
	testutil.WriteExec(t, store, "node/22/bin/node", "#!/bin/sh\n")
	fakeMise(t, store, "") // bin-paths prints nothing

	installed, err := Install(context.Background(), []model.MiseTool{{Name: "node", Version: "22"}}, 0, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(store, "node", "22")
	if len(installed) != 1 || installed[0].BinDir != want {
		t.Errorf("installed = %+v, want BinDir %q (the mise where install dir)", installed, want)
	}
}

func TestInstallSurfacesBinPathsError(t *testing.T) {
	store := testutil.TempWorkspace(t)
	fakeMise(t, store, binPathsFail)

	_, err := Install(context.Background(), []model.MiseTool{{Name: "node", Version: "22"}}, 0, false, nil, nil)
	if err == nil {
		t.Fatal("expected error when bin-paths fails")
	}
	if !strings.Contains(err.Error(), "could not install node@22") || !strings.Contains(err.Error(), "mise ERROR boom") {
		t.Errorf("error should name the tool and surface mise's stderr, got: %v", err)
	}
}

func TestToolBinDirUsesBinPaths(t *testing.T) {
	store := testutil.TempWorkspace(t)
	want := filepath.Join(store, "node", "22", "bin")
	fakeMise(t, store, want)

	got, err := ToolBinDir("node", "22")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("ToolBinDir = %q, want %q", got, want)
	}
}

func TestToolBinDirFallsBackToWhere(t *testing.T) {
	store := testutil.TempWorkspace(t)
	fakeMise(t, store, "") // bin-paths prints nothing

	got, err := ToolBinDir("node", "22")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(store, "node", "22"); got != want {
		t.Errorf("ToolBinDir = %q, want %q (the mise where install dir)", got, want)
	}
}

func TestToolBinDirRetriesBareRefWithResolvedVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake mise stub is POSIX-only")
	}
	store := testutil.TempWorkspace(t)
	want := filepath.Join(store, "node", "22", "bin")
	// Real mise prints nothing (exit 0) for a bare ref it cannot resolve, but
	// answers once the concrete version is given — which `mise where` reports.
	// The stub's bin-paths only answers refs carrying an explicit @version.
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  where) echo \"" + filepath.Join(store, "node", "22") + "\" ;;\n" +
		"  bin-paths) case \"$3\" in *@*) echo \"" + want + "\" ;; esac ;;\n" +
		"esac\n"
	bin := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := Binary
	Binary = bin
	t.Cleanup(func() { Binary = old })

	got, err := ToolBinDir("node", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("ToolBinDir = %q, want %q (bin-paths retried with the resolved version)", got, want)
	}
}

func TestToolBinDirErrorsWhenToolMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake mise stub is POSIX-only")
	}
	// Missing tools: bin-paths prints nothing but exits 0, so `mise where` —
	// which does exit non-zero — is the error signal.
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  where) echo \"mise ERROR no version installed\" >&2; exit 1 ;;\n" +
		"  bin-paths) : ;;\n" +
		"esac\n"
	bin := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := Binary
	Binary = bin
	t.Cleanup(func() { Binary = old })

	_, err := ToolBinDir("ghost", "1")
	if err == nil {
		t.Fatal("expected error for a missing tool")
	}
	if !strings.Contains(err.Error(), "mise where ghost@1") || !strings.Contains(err.Error(), "no version installed") {
		t.Errorf("error should name the command and surface mise's stderr, got: %v", err)
	}
}

func TestToolBinDirSurfacesBinPathsError(t *testing.T) {
	store := testutil.TempWorkspace(t)
	fakeMise(t, store, binPathsFail)

	_, err := ToolBinDir("node", "22")
	if err == nil {
		t.Fatal("expected error when bin-paths fails")
	}
	if !strings.Contains(err.Error(), "mise bin-paths node@22") || !strings.Contains(err.Error(), "mise ERROR boom") {
		t.Errorf("error should name the command and surface mise's stderr, got: %v", err)
	}
}

func TestInstallErrorsWithoutMise(t *testing.T) {
	old := Binary
	Binary = "definitely-not-a-real-mise-binary-xyz"
	t.Cleanup(func() { Binary = old })

	_, err := Install(context.Background(), []model.MiseTool{{Name: "node", Version: "22"}}, 0, false, nil, nil)
	if err == nil {
		t.Fatal("expected error when mise is unavailable")
	}
}

func TestInstallNoToolsIsNoop(t *testing.T) {
	installed, err := Install(context.Background(), nil, 0, false, nil, nil)
	if err != nil || installed != nil {
		t.Errorf("empty install should be a no-op, got %v err=%v", installed, err)
	}
}
