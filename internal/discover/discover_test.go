package discover

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/edganiukov/taugres/internal/testutil"
)

func TestRootOnlyWorkspace(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")

	d, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !d.IsWorkspace {
		t.Error("expected IsWorkspace true")
	}
	if d.RepoRoot != dir || d.ProjectRoot != dir {
		t.Errorf("roots = repo:%s proj:%s, want %s", d.RepoRoot, d.ProjectRoot, dir)
	}
	if d.ConfigPath != filepath.Join(dir, "workspace.tg") {
		t.Errorf("config = %s", d.ConfigPath)
	}
}

func TestNestedProject(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"root\")\n")
	testutil.WriteFile(t, dir, "svc/project.tg", "project(\"svc\")\n")

	svc := filepath.Join(dir, "svc")
	d, err := Discover(svc)
	if err != nil {
		t.Fatal(err)
	}
	if d.IsWorkspace {
		t.Error("expected IsWorkspace false for nested project")
	}
	if d.ProjectRoot != svc {
		t.Errorf("ProjectRoot = %s, want %s", d.ProjectRoot, svc)
	}
	if d.RepoRoot != dir {
		t.Errorf("RepoRoot = %s, want %s", d.RepoRoot, dir)
	}
}

func TestNoConfig(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	_, err := Discover(dir)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestBothFilesInOneDir(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	testutil.WriteFile(t, dir, "project.tg", "project(\"y\")\n")

	_, err := Discover(dir)
	var both *BothFilesError
	if !errors.As(err, &both) {
		t.Errorf("expected BothFilesError, got %v", err)
	}
}

func TestArbitraryTgIgnored(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	// An arbitrary .tg in a subdir must not be treated as an active config.
	testutil.WriteFile(t, dir, "sub/helper.tg", "x = 1\n")

	d, err := Discover(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if d.ConfigPath != filepath.Join(dir, "workspace.tg") {
		t.Errorf("arbitrary .tg was used as config: %s", d.ConfigPath)
	}
}

func TestProjectWithoutWorkspaceIsOwnRepoRoot(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "svc/project.tg", "project(\"svc\")\n")
	svc := filepath.Join(dir, "svc")

	d, err := Discover(svc)
	if err != nil {
		t.Fatal(err)
	}
	if d.RepoRoot != svc {
		t.Errorf("RepoRoot = %s, want %s (project is own repo root)", d.RepoRoot, svc)
	}
}
