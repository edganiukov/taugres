package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edganiukov/taugres/internal/testutil"
)

var (
	tauBinOnce sync.Once
	tauBinPath string
	tauBinErr  error
)

// builtTau builds the tau binary once and returns its path, skipping the test
// if the go toolchain is unavailable or the build fails.
func builtTau(t *testing.T) string {
	t.Helper()
	tauBinOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			tauBinErr = err
			return
		}
		dir, err := os.MkdirTemp("", "tau-bin-*")
		if err != nil {
			tauBinErr = err
			return
		}
		bin := filepath.Join(dir, "tau")
		out, err := exec.Command("go", "build", "-o", bin, "github.com/edganiukov/taugres/cmd/tau").CombinedOutput()
		if err != nil {
			tauBinErr = fmt.Errorf("build failed: %v\n%s", err, out)
			return
		}
		tauBinPath = bin
	})
	if tauBinErr != nil {
		t.Skipf("cannot build tau: %v", tauBinErr)
	}
	return tauBinPath
}

// TestDenyBlocksActivation checks that `tau deny` prevents the hook from
// activating the (still on-disk) generated scripts, and that re-allowing
// restores activation.
func TestDenyBlocksActivation(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	tau := builtTau(t)

	cfgHome := t.TempDir()
	cacheHome := t.TempDir()
	env := append(os.Environ(), "XDG_CONFIG_HOME="+cfgHome, "XDG_CACHE_HOME="+cacheHome)

	repo := testutil.TempWorkspace(t)
	testutil.WriteFile(t, repo, "workspace.tg", "project(\"demo\")\nshell.env(\"SCOPE\", \"root\")\n")

	runTau := func(args ...string) {
		c := exec.Command(tau, args...)
		c.Dir = repo
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("tau %v: %v\n%s", args, err, out)
		}
	}
	runTau("allow")
	runTau("sync")

	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hookOut, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}

	// enter returns whether the env activated (SCOPE set) after entering repo.
	enter := func() string {
		script := string(hookOut) + `
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
cd "` + repo + `"; prompt
echo "SCOPE=${SCOPE:-unset}"`
		cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
		cmd.Dir = t.TempDir() // start outside the project
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bash: %v\n%s", err, out)
		}
		return string(out)
	}

	if got := enter(); !strings.Contains(got, "SCOPE=root") {
		t.Fatalf("expected activation before deny:\n%s", got)
	}

	runTau("deny")
	if got := enter(); strings.Contains(got, "SCOPE=root") {
		t.Errorf("deny did not block activation:\n%s", got)
	} else if !strings.Contains(got, "not trusted") {
		t.Errorf("expected a not-trusted message after deny:\n%s", got)
	}

	runTau("allow")
	if got := enter(); !strings.Contains(got, "SCOPE=root") {
		t.Errorf("re-allow did not restore activation:\n%s", got)
	}
}

// TestConcurrentEntryWaitsForInProgressSync reproduces the two-shells race: a
// slow sync is running (holding the lock) when a second shell enters. The
// second shell's hook must shell out to tau, block on the lock, and only
// activate once the first sync has finished writing the env.
func TestConcurrentEntryWaitsForInProgressSync(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	tau := builtTau(t)

	// Fake mise whose install sleeps, so sync #1 holds the lock long enough for
	// shell #2 to enter mid-flight.
	store := testutil.TempWorkspace(t)
	testutil.WriteExec(t, store, "node/1/bin/node", "#!/bin/sh\n")
	fakeBin := t.TempDir()
	miseScript := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  install) sleep 1 ;;\n" +
		"  where) echo \"" + store + "/$(echo $2 | tr '@' '/')\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "mise"), []byte(miseScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgHome := t.TempDir()
	cacheHome := t.TempDir()
	env := append(os.Environ(),
		"XDG_CONFIG_HOME="+cfgHome,
		"XDG_CACHE_HOME="+cacheHome,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	repo := testutil.TempWorkspace(t)
	testutil.WriteFile(t, repo, "workspace.tg", "project(\"demo\")\nshell.env(\"SCOPE\", \"root\")\nmise.tool(\"node@1\")\n")

	allow := exec.Command(tau, "allow")
	allow.Dir = repo
	allow.Env = env
	if out, err := allow.CombinedOutput(); err != nil {
		t.Fatalf("allow: %v\n%s", err, out)
	}

	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hookOut, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}

	// The shell starts OUTSIDE the repo so the hook's install-time run does not
	// pre-sync. Then sync #1 runs in the background (holding the lock ~1s), and
	// only after it is in flight does the "second shell" cd in and run the hook.
	outside := t.TempDir()
	script := string(hookOut) + `
( cd "` + repo + `" && "` + tau + `" sync ) >/dev/null 2>&1 &
BG=$!
sleep 0.3
cd "` + repo + `"
_tau_hook
echo "SCOPE=${SCOPE:-unset}"
wait "$BG"`

	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	cmd.Dir = outside
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "SCOPE=root") {
		t.Errorf("second shell did not wait for the in-progress sync (env not active):\n%s", got)
	}
	if strings.Contains(got, "is not synced") {
		t.Errorf("second shell raced ahead to a half-written env:\n%s", got)
	}
}

// TestBashHookAutoSyncsOnEntry proves the headline behavior: with only `tau
// allow` run once (never a manual `tau sync`), entering the project via the
// hook auto-syncs and activates.
func TestBashHookAutoSyncsOnEntry(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	tau := builtTau(t)

	cfgHome := t.TempDir()
	cacheHome := t.TempDir()
	env := append(os.Environ(), "XDG_CONFIG_HOME="+cfgHome, "XDG_CACHE_HOME="+cacheHome)

	repo := testutil.TempWorkspace(t)
	testutil.WriteFile(t, repo, "workspace.tg", "project(\"demo\")\nshell.env(\"SCOPE\", \"root\")\n")

	// Trust once (no manual sync).
	allow := exec.Command(tau, "allow")
	allow.Dir = repo
	allow.Env = env
	if out, err := allow.CombinedOutput(); err != nil {
		t.Fatalf("allow failed: %v\n%s", err, out)
	}

	// The hook bakes in the built tau path via os.Executable().
	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hookOut, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}

	script := string(hookOut) + `
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
cd /tmp; prompt
cd "` + repo + `"; prompt
echo "SCOPE=${SCOPE:-unset}"`

	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "SCOPE=root") {
		t.Errorf("environment not activated by auto-sync:\n%s", got)
	}
	if !strings.Contains(got, "tau activated demo") {
		t.Errorf("expected activation banner:\n%s", got)
	}
	// Auto-sync must have generated the scripts.
	if _, err := os.Stat(filepath.Join(repo, ".taugres", "gen", "activate.bash")); err != nil {
		t.Errorf("auto-sync did not generate activate.bash: %v", err)
	}
}

// TestBashHookResyncsOnProbeChange proves that a config branching on exists()
// re-syncs (fork-free detection) when the probed file appears — without any
// config-file edit. The hook records the probe result and notices the flip.
func TestBashHookResyncsOnProbeChange(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	tau := builtTau(t)
	cfgHome := t.TempDir()
	cacheHome := t.TempDir()
	env := append(os.Environ(), "XDG_CONFIG_HOME="+cfgHome, "XDG_CACHE_HOME="+cacheHome)

	repo := testutil.TempWorkspace(t)
	testutil.WriteFile(t, repo, "workspace.tg",
		"project(\"demo\")\nif exists(\"//trigger\"):\n    shell.env(\"TRIG\", \"yes\")\n")

	runTau := func(args ...string) {
		c := exec.Command(tau, args...)
		c.Dir = repo
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("tau %v: %v\n%s", args, err, out)
		}
	}
	runTau("allow")
	runTau("sync") // initial sync: trigger absent -> TRIG unset, probe recorded as 0

	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hb, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}

	// Enter (TRIG unset), then create the probed file and prompt again: the hook
	// must detect the exists() flip and auto-sync so TRIG becomes set.
	// The `sleep 1` crosses a second boundary so the regenerated activate.bash
	// has a newer mtime than the first activation (the hook keys reactivation on
	// that mtime, which stat reports at 1s granularity).
	script := string(hb) + `
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
cd "` + repo + `"; prompt; echo "BEFORE TRIG=${TRIG:-unset}"
sleep 1
touch "` + repo + `/trigger"; prompt; echo "AFTER TRIG=${TRIG:-unset}"`
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "BEFORE TRIG=unset") {
		t.Errorf("expected TRIG unset before the probe flips:\n%s", got)
	}
	if !strings.Contains(got, "AFTER TRIG=yes") {
		t.Errorf("probe change did not trigger a resync (TRIG not set):\n%s", got)
	}
}

// TestBashHookActivatesOnCd verifies the direnv-style wiring: installing the
// hook registers _tau_prompt_hook in PROMPT_COMMAND, and running that (as bash
// does before each prompt) activates/deactivates as the directory changes.
func TestBashHookActivatesOnCd(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	// Use the built binary so the hook bakes a real tau path into _TAU_BIN
	// (the in-process `run` would bake the test binary, and activation now
	// shells out to `tau activate`).
	tau := builtTau(t)
	cfgHome := t.TempDir()
	cacheHome := t.TempDir()
	env := append(os.Environ(), "XDG_CONFIG_HOME="+cfgHome, "XDG_CACHE_HOME="+cacheHome)

	repo := testutil.TempWorkspace(t)
	testutil.WriteFile(t, repo, "workspace.tg", "project(\"root\")\nshell.env(\"SCOPE\", \"root\")\n")
	testutil.WriteFile(t, repo, "svc/project.tg", "project(\"svc\")\nshell.env(\"SCOPE\", \"svc\")\n")
	runTau := func(dir string, args ...string) {
		c := exec.Command(tau, args...)
		c.Dir = dir
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("tau %v: %v\n%s", args, err, out)
		}
	}
	for _, wd := range []string{repo, repo + "/svc"} {
		runTau(wd, "allow")
		runTau(wd, "sync")
	}

	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hb, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	hookOut := string(hb)

	// Simulate bash: eval PROMPT_COMMAND before each "prompt" after a cd.
	script := hookOut + `
if [ "$PROMPT_COMMAND" != "_tau_prompt_hook" ]; then echo "BADPROMPT:[$PROMPT_COMMAND]"; fi
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
cd /tmp;         prompt; echo "OUT SCOPE=${SCOPE:-unset}"
cd "` + repo + `";    prompt; echo "ROOT SCOPE=${SCOPE:-unset}"
cd "` + repo + `/svc"; prompt; echo "SVC SCOPE=${SCOPE:-unset}"
cd "` + repo + `";    prompt; echo "BACK SCOPE=${SCOPE:-unset}"
cd /tmp;         prompt; echo "GONE SCOPE=${SCOPE:-unset} ACTIVE=${TAUGRES_ACTIVE:-unset}"
`
	bashCmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	bashCmd.Env = env
	out, err := bashCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash failed: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"OUT SCOPE=unset",
		"ROOT SCOPE=root",
		"SVC SCOPE=svc",
		"BACK SCOPE=root",
		"GONE SCOPE=unset ACTIVE=unset",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "BADPROMPT") {
		t.Errorf("PROMPT_COMMAND not wired correctly:\n%s", got)
	}
}

// TestBashHookPreservesCustomPromptCommand verifies that installing the hook on
// top of a user's existing PROMPT_COMMAND keeps that command running, for both
// the scalar-string and array (bash 5.1+) forms.
func TestBashHookPreservesCustomPromptCommand(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	tau := builtTau(t)
	cfgHome := t.TempDir()
	cacheHome := t.TempDir()
	env := append(os.Environ(), "XDG_CONFIG_HOME="+cfgHome, "XDG_CACHE_HOME="+cacheHome)
	repo := testutil.TempWorkspace(t)
	testutil.WriteFile(t, repo, "workspace.tg", "project(\"root\")\nshell.env(\"SCOPE\", \"root\")\n")
	runTau := func(args ...string) {
		c := exec.Command(tau, args...)
		c.Dir = repo
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("tau %v: %v\n%s", args, err, out)
		}
	}
	runTau("allow")
	runTau("sync")
	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hb, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	hookOut := string(hb)

	cases := map[string]string{
		"scalar": `PROMPT_COMMAND='__mine'`,
		"array":  `PROMPT_COMMAND=(__mine)`,
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			script := `__mine() { echo CUSTOM_RAN; }
` + setup + `
` + hookOut + `
# Model bash: run each PROMPT_COMMAND element as a separate command.
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
cd "` + repo + `"; prompt
echo "SCOPE=${SCOPE:-unset}"`
			bashCmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
			bashCmd.Env = env
			out, err := bashCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bash failed: %v\n%s", err, out)
			}
			got := string(out)
			if !strings.Contains(got, "CUSTOM_RAN") {
				t.Errorf("custom PROMPT_COMMAND was dropped:\n%s", got)
			}
			if !strings.Contains(got, "SCOPE=root") {
				t.Errorf("tau hook did not activate:\n%s", got)
			}
		})
	}
}

// TestBashActivationRoundTrip syncs a workspace, then sources the generated
// bash scripts in a subprocess to verify env/PATH/aliases/functions apply and
// deactivation restores prior state.
func TestBashActivationRoundTrip(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	isolate(t)

	dir := testutil.TempWorkspace(t)
	testutil.WriteFile(t, dir, "workspace.tg", `
project("demo")
shell.env("DEMO_VAR", "hello")
shell.unset("PYTHONPATH")
shell.path.prepend("//bin")
shell.path.append("//scripts")
shell.alias("ll", "ls -lah")
shell.fn("croot", shells = ["bash", "zsh"], file = "//bin/croot.sh")
`)
	testutil.WriteFile(t, dir, "bin/croot.sh", "cd \"$TAUGRES_PROJECT_ROOT\"\n")

	if code, _, errOut := run(t, dir, "allow"); code != 0 {
		t.Fatalf("allow failed: %s", errOut)
	}
	if code, _, errOut := run(t, dir, "sync"); code != 0 {
		t.Fatalf("sync failed: %s", errOut)
	}

	gen := filepath.Join(dir, ".taugres", "gen")
	script := `
set -e
export PYTHONPATH="/keep/me"
export DEMO_VAR="original"
ORIG_PATH="$PATH"
source "` + gen + `/activate.bash"
echo "ACT DEMO_VAR=$DEMO_VAR"
echo "ACT PYTHONPATH=${PYTHONPATH+set}"
echo "ACT ROOT=$TAUGRES_PROJECT_ROOT"
[ "${PATH%%:*}" = "` + dir + `/bin" ] && echo "ACT PATHHEAD=ok"
type croot >/dev/null 2>&1 && echo "ACT croot=ok"
source "` + gen + `/deactivate.bash"
echo "DEA DEMO_VAR=$DEMO_VAR"
echo "DEA PYTHONPATH=$PYTHONPATH"
[ "$PATH" = "$ORIG_PATH" ] && echo "DEA PATH=restored"
type croot >/dev/null 2>&1 || echo "DEA croot=gone"
echo "DEA ACTIVE=${TAUGRES_ACTIVE:-unset}"
`
	out, err := exec.Command(bash, "--noprofile", "--norc", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash failed: %v\n%s", err, out)
	}
	got := string(out)

	wants := []string{
		"ACT DEMO_VAR=hello",
		"ACT PYTHONPATH=", // unset -> ${PYTHONPATH+set} empty
		"ACT ROOT=" + dir,
		"ACT PATHHEAD=ok",
		"ACT croot=ok",
		"DEA DEMO_VAR=original",
		"DEA PYTHONPATH=/keep/me",
		"DEA PATH=restored",
		"DEA croot=gone",
		"DEA ACTIVE=unset",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in output:\n%s", w, got)
		}
	}
	// Ensure PYTHONPATH was actually unset during activation.
	if strings.Contains(got, "ACT PYTHONPATH=set") {
		t.Errorf("PYTHONPATH should be unset during activation:\n%s", got)
	}
}
