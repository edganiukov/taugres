package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edganiukov/taugres/internal/model"
	"github.com/edganiukov/taugres/internal/testutil"
)

func TestManifestRoundTrip(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	stateDir := filepath.Join(dir, ".taugres")
	cfg := filepath.Join(dir, "workspace.tg")

	m := &Manifest{
		Inputs:      map[string]string{cfg: "abc", filepath.Join(dir, "mod.tg"): "def"},
		ToolDirs:    []string{"/store/node/1/bin"},
		ManagerDirs: map[string][]string{"mise": {"/store/node/1/bin"}},
		Probes:      []model.Probe{{Kind: "exists", Arg: "/x", Result: "1"}, {Kind: "which", Arg: "go", Result: "/usr/bin/go"}},
	}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Inputs[cfg] != "abc" || len(got.Inputs) != 2 {
		t.Errorf("inputs round trip mismatch: %+v", got.Inputs)
	}
	if len(got.ToolDirs) != 1 || got.ToolDirs[0] != "/store/node/1/bin" {
		t.Errorf("tooldirs round trip mismatch: %+v", got.ToolDirs)
	}
	if dirs := got.ManagerDirs["mise"]; len(dirs) != 1 || dirs[0] != "/store/node/1/bin" {
		t.Errorf("manager dirs round trip mismatch: %+v", got.ManagerDirs)
	}
	if len(got.Probes) != 2 || got.Probes[1].Result != "/usr/bin/go" {
		t.Errorf("probes round trip mismatch: %+v", got.Probes)
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
	m := &Manifest{Inputs: map[string]string{cfg: hash}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	// Write the generated scripts the check expects.
	testutil.WriteFile(t, dir, ".taugres/gen/activate.bash", "x")
	testutil.WriteFile(t, dir, ".taugres/gen/deactivate.bash", "x")

	if r := CheckStale(stateDir, []string{"bash"}); r.Stale {
		t.Errorf("should be fresh: %s", r.Reason)
	}

	// Modify config -> hash mismatch -> stale.
	if err := os.WriteFile(cfg, []byte("project(\"y\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := CheckStale(stateDir, []string{"bash"}); !r.Stale {
		t.Error("expected stale after config change")
	}
}

func TestCheckStaleDetectsEnvProbeDrift(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")
	hash, _ := HashFile(cfg)

	t.Setenv("TAU_TEST_VAR", "one")
	m := &Manifest{
		Inputs: map[string]string{cfg: hash},
		Probes: []model.Probe{{Kind: "env", Arg: "TAU_TEST_VAR", Result: model.EnvProbeResult("one", true)}},
	}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, dir, ".taugres/gen/activate.bash", "x")
	testutil.WriteFile(t, dir, ".taugres/gen/deactivate.bash", "x")

	if r := CheckStale(stateDir, []string{"bash"}); r.Stale {
		t.Errorf("should be fresh when env var unchanged: %s", r.Reason)
	}
	// Change the observed env var -> stale.
	t.Setenv("TAU_TEST_VAR", "two")
	if r := CheckStale(stateDir, []string{"bash"}); !r.Stale {
		t.Error("expected stale after env var changed")
	}
	// Unsetting is also drift.
	os.Unsetenv("TAU_TEST_VAR")
	if r := CheckStale(stateDir, []string{"bash"}); !r.Stale {
		t.Error("expected stale after env var unset")
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
	m := &Manifest{Inputs: map[string]string{cfg: hash}}
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

	// Writing the manifest (newest) marks a completed sync.
	m := &Manifest{Inputs: map[string]string{cfg: ""}}
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

	// Re-sync (fresh manifest, newest again) recording a tool dir that exists.
	toolBin := filepath.Join(stateDir, "tools", "pip", "bin")
	if err := os.MkdirAll(toolBin, 0o755); err != nil {
		t.Fatal(err)
	}
	m2 := &Manifest{Inputs: map[string]string{cfg: ""}, ToolDirs: []string{toolBin}}
	if err := m2.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); need {
		t.Error("should not need sync when tool dir present")
	}
	// Remove the recorded tool dir -> needs sync.
	if err := os.RemoveAll(toolBin); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); !need {
		t.Error("expected needs-sync after tool dir removed")
	}
}

func TestNeedsSyncMigratesLegacyManifest(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")
	if err := os.MkdirAll(GenDir(stateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ManifestPath(stateDir), []byte("input:hash:"+cfg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); !need {
		t.Error("legacy manifest should trigger a one-time migration sync")
	}
}

func TestNeedsSyncDetectsBackdatedEdit(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"old\")\n")
	stateDir := filepath.Join(dir, ".taugres")
	hash, err := HashFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Inputs: map[string]string{cfg: hash}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(cfg, []byte("project(\"new\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(946684800, 0)
	if err := os.Chtimes(cfg, old, old); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); !need {
		t.Error("expected a backdated content edit to trigger sync")
	}
}

func TestInspectHookDetectsMissingScript(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")
	m := &Manifest{Inputs: map[string]string{cfg: ""}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, dir, ".taugres/gen/deactivate.bash", "x")
	if inspection := InspectHook(stateDir, cfg, "bash"); !inspection.NeedsSync {
		t.Error("missing activation script should trigger sync")
	}
}

// tamperTauBuild rewrites the manifest's tau: line in place, simulating a
// manifest written by a different tau build.
func tamperTauBuild(t *testing.T, stateDir string) {
	t.Helper()
	data, err := os.ReadFile(ManifestPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "tau:") {
			lines[i] = "tau:some-other-build"
		}
	}
	if err := os.WriteFile(ManifestPath(stateDir), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManifestStampsAndRoundTripsTauBuild(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")

	// Write stamps an empty TauBuild with the running build…
	m := &Manifest{Inputs: map[string]string{cfg: ""}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.TauBuild != BuildStamp() {
		t.Errorf("TauBuild = %q, want current BuildStamp %q", got.TauBuild, BuildStamp())
	}

	// …and preserves a foreign build on rewrite (Load→Write, as TouchManifest
	// does), so touching never launders another build's derived state.
	got.TauBuild = "some-other-build"
	if err := got.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	again, err := Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if again.TauBuild != "some-other-build" {
		t.Errorf("TauBuild = %q, want preserved foreign stamp", again.TauBuild)
	}
}

func TestNeedsSyncOnDifferentTauBuild(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")

	m := &Manifest{Inputs: map[string]string{cfg: ""}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); need {
		t.Fatal("should not need sync right after writing manifest")
	}

	tamperTauBuild(t, stateDir)
	if need, _ := NeedsSync(stateDir, cfg); !need {
		t.Error("expected needs-sync when the manifest was written by another tau build")
	}
}

func TestCheckStaleOnDifferentTauBuild(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")
	hash, _ := HashFile(cfg)

	m := &Manifest{Inputs: map[string]string{cfg: hash}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, dir, ".taugres/gen/activate.bash", "x")
	testutil.WriteFile(t, dir, ".taugres/gen/deactivate.bash", "x")

	if r := CheckStale(stateDir, []string{"bash"}); r.Stale {
		t.Fatalf("should be fresh: %s", r.Reason)
	}

	tamperTauBuild(t, stateDir)
	r := CheckStale(stateDir, []string{"bash"})
	if !r.Stale {
		t.Fatal("expected stale when the manifest was written by another tau build")
	}
	if !strings.Contains(r.Reason, "tau was updated") {
		t.Errorf("reason should mention the tau update, got %q", r.Reason)
	}
}

func TestNeedsSyncPendingManager(t *testing.T) {
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")
	stateDir := filepath.Join(dir, ".taugres")
	m := &Manifest{Inputs: map[string]string{cfg: ""}, PendingManagers: []string{"mise"}}
	if err := m.Write(stateDir); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSync(stateDir, cfg); !need {
		t.Error("expected an incomplete manager to keep the manifest stale")
	}
}
