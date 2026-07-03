package npm

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/testutil"
)

// fakeToolchain returns a bin dir containing a stub `npm` that, on install,
// creates <prefix>/bin/tool and echoes its arguments for inspection.
func fakeToolchain(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake npm stub is POSIX-only")
	}
	dir := t.TempDir()
	// args: install -g --prefix <dir> <ref>
	script := "#!/bin/sh\n" +
		"echo \"npm $@\"\n" +
		"if [ \"$1\" = \"install\" ]; then\n" +
		"  mkdir -p \"$4/bin\"\n" +
		"  touch \"$4/bin/tool\"\n" +
		"fi\n"
	bin := filepath.Join(dir, "npm")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInstallIntoPrefixAndStreams(t *testing.T) {
	toolchain := fakeToolchain(t)
	npmDir := filepath.Join(testutil.TempWorkspace(t), ".taugres", "tools", "npm")

	var out bytes.Buffer
	_, err := Install([]model.Package{{Name: "typescript", Version: "5.6.2"}, {Name: "cowsay"}}, npmDir, []string{toolchain}, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(npmDir, "bin")); err != nil {
		t.Errorf("npm prefix bin not created: %v", err)
	}
	// All packages are installed in one batched command.
	got := out.String()
	if !strings.Contains(got, "install -g --prefix "+npmDir+" typescript@5.6.2 cowsay") {
		t.Errorf("expected batched prefixed install, got:\n%s", got)
	}
}

func TestInstallErrorsWithoutNpm(t *testing.T) {
	// Empty toolchain + a PATH with no npm.
	t.Setenv("PATH", t.TempDir())
	_, err := Install([]model.Package{{Name: "cowsay"}}, t.TempDir(), nil, nil, nil)
	if err == nil {
		t.Fatal("expected error when npm is unavailable")
	}
}

func TestInstallNoPackagesIsNoop(t *testing.T) {
	if _, err := Install(nil, t.TempDir(), nil, nil, nil); err != nil {
		t.Errorf("empty install should be a no-op, got %v", err)
	}
}

func TestBinDir(t *testing.T) {
	if got := BinDir("/p/.taugres/tools/npm"); got != filepath.FromSlash("/p/.taugres/tools/npm/bin") {
		t.Errorf("BinDir = %q", got)
	}
}
