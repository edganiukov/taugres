package mise

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.gnkv.dev/taugres/internal/model"
	"go.gnkv.dev/taugres/internal/testutil"
)

// fakeMise installs a stub mise whose `install` echoes two lines and whose
// `where` points at store. It sets Binary to the stub for the test.
func fakeMise(t *testing.T, store string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake mise stub is POSIX-only")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  install) echo \"downloading $2\"; echo \"installed $2\" ;;\n" +
		"  where) echo \"" + store + "/$(echo $2 | tr '@' '/')\" ;;\n" +
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
	fakeMise(t, store)

	// mise writes its raw output to the provided writer; line prefixing is the
	// caller's responsibility (see internal/ui Reporter/LinePrefixer).
	var out bytes.Buffer
	installed, err := Install([]model.MiseTool{{Name: "node", Version: "22"}}, &out, nil)
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
	fakeMise(t, store)

	if _, err := Install([]model.MiseTool{{Name: "node", Version: "22"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInstallErrorsWithoutMise(t *testing.T) {
	old := Binary
	Binary = "definitely-not-a-real-mise-binary-xyz"
	t.Cleanup(func() { Binary = old })

	_, err := Install([]model.MiseTool{{Name: "node", Version: "22"}}, nil, nil)
	if err == nil {
		t.Fatal("expected error when mise is unavailable")
	}
}

func TestInstallNoToolsIsNoop(t *testing.T) {
	installed, err := Install(nil, nil, nil)
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
