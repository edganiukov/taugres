package pip

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

// fakeToolchain returns a bin dir containing a stub `python3` that, on
// `-m venv <dir>`, creates <dir>/bin/pip as an executable stub echoing its args.
func fakeToolchain(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake python stub is POSIX-only")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-m\" ] && [ \"$2\" = \"venv\" ]; then\n" +
		"  mkdir -p \"$3/bin\"\n" +
		"  printf '#!/bin/sh\\necho pip $@\\n' > \"$3/bin/pip\"\n" +
		"  chmod +x \"$3/bin/pip\"\n" +
		"fi\n"
	bin := filepath.Join(dir, "python3")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInstallCreatesVenvAndInstalls(t *testing.T) {
	toolchain := fakeToolchain(t)
	venv := filepath.Join(testutil.TempWorkspace(t), ".taugres", "tools", "pip")

	var out bytes.Buffer
	_, err := Install(context.Background(), []model.Package{{Name: "requests", Version: "2.31.0"}, {Name: "rich"}}, venv, []string{toolchain}, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(venv, "bin", "pip")); err != nil {
		t.Errorf("venv pip not created: %v", err)
	}
	// All packages are installed in one batched command.
	got := out.String()
	if !strings.Contains(got, "install requests==2.31.0 rich") {
		t.Errorf("expected batched install, got:\n%s", got)
	}
}

func TestInstallReusesExistingVenv(t *testing.T) {
	toolchain := fakeToolchain(t)
	venv := filepath.Join(testutil.TempWorkspace(t), ".taugres", "tools", "pip")

	if _, err := Install(context.Background(), []model.Package{{Name: "rich"}}, venv, []string{toolchain}, nil, nil); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(venv, "bin", "MARKER")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), []model.Package{{Name: "requests"}}, venv, []string{toolchain}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("existing venv should be reused, marker gone: %v", err)
	}
}

func TestInstallErrorsWithoutPython(t *testing.T) {
	// Empty toolchain + a PATH with no python.
	t.Setenv("PATH", t.TempDir())
	_, err := Install(context.Background(), []model.Package{{Name: "rich"}}, t.TempDir(), nil, nil, nil)
	if err == nil {
		t.Fatal("expected error when no python interpreter is available")
	}
}

func TestInstallNoPackagesIsNoop(t *testing.T) {
	if _, err := Install(context.Background(), nil, t.TempDir(), nil, nil, nil); err != nil {
		t.Errorf("empty install should be a no-op, got %v", err)
	}
}

func TestBinDir(t *testing.T) {
	if got := BinDir("/p/.taugres/tools/pip"); got != filepath.FromSlash("/p/.taugres/tools/pip/bin") {
		t.Errorf("BinDir = %q", got)
	}
}
