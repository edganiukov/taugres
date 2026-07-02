package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edganiukov/taugres/internal/testutil"
)

func TestManifestRoundTrip(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	stateDir := filepath.Join(dir, ".taugres")

	m := &Manifest{
		TauVersion:   "test",
		ConfigPath:   filepath.Join(dir, "workspace.tg"),
		RepoRoot:     dir,
		ProjectRoot:  dir,
		ConfigHash:   "abc",
		ModuleHashes: map[string]string{"m": "1"},
		SourceHashes: map[string]string{},
		Shells:       []string{"bash", "zsh"},
	}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigHash != "abc" || got.ModuleHashes["m"] != "1" {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestCheckStaleDetectsConfigChange(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")

	hash, err := HashFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{ConfigPath: cfg, ConfigHash: hash, Shells: []string{"bash"}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	// Write the generated scripts the check expects.
	genDir := GenDir(stateDir)
	testutil.WriteFile(t, dir, ".taugres/gen/activate.bash", "x")
	testutil.WriteFile(t, dir, ".taugres/gen/deactivate.bash", "x")
	_ = genDir

	if r := CheckStale(stateDir, []string{"bash"}); r.Stale {
		t.Errorf("should be fresh: %s", r.Reason)
	}

	// Modify config -> stale.
	if err := os.WriteFile(cfg, []byte("project(\"y\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := CheckStale(stateDir, []string{"bash"}); !r.Stale {
		t.Error("expected stale after config change")
	}
}

func TestCheckStaleMissingManifest(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	stateDir := filepath.Join(dir, ".taugres")
	if r := CheckStale(stateDir, []string{"bash"}); !r.Stale {
		t.Error("expected stale when no manifest")
	}
}

func TestCheckStaleMissingScript(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")
	hash, _ := HashFile(cfg)
	m := &Manifest{ConfigPath: cfg, ConfigHash: hash, Shells: []string{"bash"}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	// No generated scripts written.
	if r := CheckStale(stateDir, []string{"bash"}); !r.Stale {
		t.Error("expected stale when scripts missing")
	}
}

func TestNeedsSync(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")

	// No manifest yet -> needs sync.
	if need, _ := NeedsSync(stateDir, cfg); !need {
		t.Error("expected needs-sync when no manifest")
	}

	// Writing the manifest marks a completed sync.
	m := &Manifest{ConfigPath: cfg, Shells: []string{"bash"}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); need {
		t.Error("should not need sync right after writing manifest")
	}

	// Touch the config so it is newer than the manifest.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(cfg, []byte("project(\"y\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); !need {
		t.Error("expected needs-sync after config edited")
	}

	// Re-sync (fresh manifest), then remove a recorded tool dir -> needs sync.
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	toolBin := filepath.Join(stateDir, "tools", "pip", "bin")
	if err := os.MkdirAll(toolBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteToolDirs(stateDir, []string{toolBin}); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); need {
		t.Error("should not need sync when tool dir present")
	}
	if err := os.RemoveAll(toolBin); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); !need {
		t.Error("expected needs-sync after tool dir removed")
	}
}

func TestLockSerializes(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	stateDir := filepath.Join(dir, ".taugres")

	unlock, err := Lock(stateDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	waited := make(chan struct{}, 1)
	acquired := make(chan struct{})
	go func() {
		u2, err := Lock(stateDir, func() { waited <- struct{}{} })
		if err != nil {
			return
		}
		close(acquired)
		_ = u2()
	}()

	// The second lock must not be acquired while the first is held.
	select {
	case <-acquired:
		t.Fatal("second Lock acquired while first was held")
	case <-time.After(150 * time.Millisecond):
	}

	// The waiter should have reported that it is blocking.
	select {
	case <-waited:
	case <-time.After(150 * time.Millisecond):
		t.Error("onWait was not called while the lock was contended")
	}

	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second Lock did not acquire after release")
	}
}
