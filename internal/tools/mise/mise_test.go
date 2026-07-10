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

// fakeMise installs a stub mise whose `install` echoes two lines and whose
// `where` points at store. `bin-paths` prints binPathsOut when non-empty;
// otherwise it prints nothing, exercising the heuristic fallback (as an older
// mise would). It sets Binary to the stub for the test.
func fakeMise(t *testing.T, store, binPathsOut string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake mise stub is POSIX-only")
	}
	dir := t.TempDir()
	binPathsCase := "  bin-paths) : ;;\n"
	if binPathsOut != "" {
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
	fakeMise(t, store, "")

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
	// Nested aqua layout the heuristic can only find via execName; the
	// authoritative `mise bin-paths` answer must win regardless.
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

func TestBinDirLayouts(t *testing.T) {
	root := testutil.TempWorkspace(t)

	// Standard: <install>/bin/<tool>.
	std := filepath.Join(root, "std")
	testutil.WriteExec(t, std, "bin/node", "#!/bin/sh\n")
	if got := binDir(std, "node"); got != filepath.Join(std, "bin") {
		t.Errorf("bin/ layout: got %q", got)
	}

	// Root: <install>/<tool>.
	rootLayout := filepath.Join(root, "rootl")
	testutil.WriteExec(t, rootLayout, "rg", "#!/bin/sh\n")
	if got := binDir(rootLayout, "rg"); got != rootLayout {
		t.Errorf("root layout: got %q", got)
	}

	// Nested (ubi archive dir, no bin/): <install>/<sub>/<tool>.
	nested := filepath.Join(root, "nested")
	testutil.WriteExec(t, nested, "uv-x86_64-unknown-linux-musl/uv", "#!/bin/sh\n")
	if got := binDir(nested, "uv"); got != filepath.Join(nested, "uv-x86_64-unknown-linux-musl") {
		t.Errorf("nested layout: got %q", got)
	}

	// Unknown: fall back to the install dir.
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := binDir(empty, "whatever"); got != empty {
		t.Errorf("fallback: got %q", got)
	}
}

func TestExecName(t *testing.T) {
	cases := map[string]string{
		"node":                     "node",
		"aqua:syncthing/syncthing": "syncthing",
		"ubi:BurntSushi/ripgrep":   "ripgrep",
		"go:github.com/x/tool":     "tool",
		"npm:prettier":             "prettier",
	}
	for in, want := range cases {
		if got := execName(in); got != want {
			t.Errorf("execName(%q) = %q, want %q", in, got, want)
		}
	}
}
