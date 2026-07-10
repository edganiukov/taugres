package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edganiukov/taugres/internal/testutil"
)

// TestAutosyncSkipsInstallsWhenToolsUnchanged proves the shell-prep/install
// split: a config edit that changes only shell state (an env var) regenerates
// the scripts without invoking mise at all, while changing the tool set does
// re-invoke it. A fake mise logs every call so we can assert this directly.
func TestAutosyncSkipsInstallsWhenToolsUnchanged(t *testing.T) {
	tau := builtTau(t)

	// Fake mise: `where` resolves to a prebuilt store path; every other call is a
	// no-op. It appends its args to a log so we can see whether tool work ran.
	store := testutil.TempWorkspace(t)
	testutil.WriteExec(t, store, "node/1/bin/node", "#!/bin/sh\n")
	testutil.WriteExec(t, store, "node/2/bin/node", "#!/bin/sh\n")
	fakeBin := t.TempDir()
	miseLog := filepath.Join(t.TempDir(), "mise.log")
	miseScript := "#!/bin/sh\n" +
		"echo \"$@\" >> \"" + miseLog + "\"\n" +
		"case \"$1\" in\n" +
		"  where) echo \"" + store + "/$(echo $2 | tr '@' '/')\" ;;\n" +
		"  bin-paths) echo \"" + store + "/$(echo $3 | tr '@' '/')/bin\" ;;\n" +
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
	testutil.WriteFile(t, repo, "workspace.tg",
		"project(\"demo\")\nshell.env(\"SCOPE\", \"root\")\nmise.tool(\"node@1\")\n")

	runTau := func(args ...string) {
		t.Helper()
		c := exec.Command(tau, args...)
		c.Dir = repo
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("tau %v: %v\n%s", args, err, out)
		}
	}
	miseCalls := func() string {
		data, _ := os.ReadFile(miseLog)
		return string(data)
	}

	runTau("allow")
	runTau("sync")
	if miseCalls() == "" {
		t.Fatalf("expected mise to be invoked on the initial sync")
	}

	// Shell-only edit: change an env var, leave the tool set alone. The fast path
	// must regenerate scripts WITHOUT touching mise.
	if err := os.Truncate(miseLog, 0); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, repo, "workspace.tg",
		"project(\"demo\")\nshell.env(\"SCOPE\", \"changed\")\nmise.tool(\"node@1\")\n")
	runTau("sync", "--if-stale")
	if c := miseCalls(); c != "" {
		t.Errorf("shell-only edit re-invoked mise (install skipping missed):\n%s", c)
	}
	act, err := os.ReadFile(filepath.Join(repo, ".taugres", "gen", "activate.bash"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(act), "changed") {
		t.Errorf("scripts not regenerated with the new env value:\n%s", act)
	}

	// Sanity: changing the tool set DOES re-invoke mise (guards against
	// over-skipping).
	if err := os.Truncate(miseLog, 0); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, repo, "workspace.tg",
		"project(\"demo\")\nshell.env(\"SCOPE\", \"changed\")\nmise.tool(\"node@2\")\n")
	runTau("sync", "--if-stale")
	if miseCalls() == "" {
		t.Errorf("tool-set change did not re-invoke mise")
	}
}

// TestAutosyncReinstallsOnlyChangedManager proves per-manager tracking: with
// both a mise-provisioned toolchain and an npm package, changing only the npm
// package reinstalls npm without re-invoking mise (its signature is unchanged,
// so its cached store dirs are reused — no `mise where`, no install).
func TestAutosyncReinstallsOnlyChangedManager(t *testing.T) {
	tau := builtTau(t)

	store := testutil.TempWorkspace(t)
	testutil.WriteExec(t, store, "node/1/bin/node", "#!/bin/sh\n")
	miseLog := filepath.Join(t.TempDir(), "mise.log")
	npmLog := filepath.Join(t.TempDir(), "npm.log")

	// Fake npm lives in the mise node bin dir (that's where the toolchain resolves
	// it). It records calls and lays down the prefix layout tau reads back.
	npmScript := "#!/bin/sh\n" +
		"echo \"$@\" >> \"" + npmLog + "\"\n" +
		"prefix=\n" +
		"while [ $# -gt 0 ]; do [ \"$1\" = \"--prefix\" ] && { shift; prefix=\"$1\"; }; shift; done\n" +
		"if [ -n \"$prefix\" ]; then\n" +
		"  mkdir -p \"$prefix/bin\" \"$prefix/lib/node_modules/leftpad\"\n" +
		"  printf '{\"version\":\"1.0.0\"}' > \"$prefix/lib/node_modules/leftpad/package.json\"\n" +
		"fi\n"
	testutil.WriteExec(t, store, "node/1/bin/npm", npmScript)

	// Fake mise: `where` resolves node to the prebuilt store path; everything else
	// is a no-op. Records calls so we can assert mise is untouched.
	fakeBin := t.TempDir()
	miseScript := "#!/bin/sh\n" +
		"echo \"$@\" >> \"" + miseLog + "\"\n" +
		"case \"$1\" in\n" +
		"  where) echo \"" + store + "/node/1\" ;;\n" +
		"  bin-paths) echo \"" + store + "/node/1/bin\" ;;\n" +
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
	testutil.WriteFile(t, repo, "workspace.tg",
		"project(\"demo\")\nnpm.install(\"leftpad@1\")\n")

	runTau := func(args ...string) {
		t.Helper()
		c := exec.Command(tau, args...)
		c.Dir = repo
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("tau %v: %v\n%s", args, err, out)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	runTau("allow")
	runTau("sync")
	if read(miseLog) == "" || read(npmLog) == "" {
		t.Fatalf("initial sync should invoke both mise and npm\nmise:%q\nnpm:%q", read(miseLog), read(npmLog))
	}

	// Change only the npm package; mise (the node toolchain) is unchanged.
	if err := os.Truncate(miseLog, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(npmLog, 0); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, repo, "workspace.tg",
		"project(\"demo\")\nnpm.install(\"leftpad@2\")\n")
	runTau("sync", "--if-stale")

	if c := read(miseLog); c != "" {
		t.Errorf("npm-only change re-invoked mise (per-manager tracking missed):\n%s", c)
	}
	if read(npmLog) == "" {
		t.Errorf("npm-only change did not reinstall npm")
	}
}

// TestAutosyncNoOpTouchSkipsRegeneration proves the hash confirmation: a config
// whose mtime is bumped without any content change (an editor save, a git
// checkout) triggers the cheap mtime check but must not regenerate scripts —
// the thorough hash check sees no real change and re-anchors the manifest mtime.
func TestAutosyncNoOpTouchSkipsRegeneration(t *testing.T) {
	tau := builtTau(t)
	cfgHome := t.TempDir()
	cacheHome := t.TempDir()
	env := append(os.Environ(), "XDG_CONFIG_HOME="+cfgHome, "XDG_CACHE_HOME="+cacheHome)

	repo := testutil.TempWorkspace(t)
	cfgPath := testutil.WriteFile(t, repo, "workspace.tg",
		"project(\"demo\")\nshell.env(\"SCOPE\", \"root\")\n")

	runTau := func(args ...string) {
		t.Helper()
		c := exec.Command(tau, args...)
		c.Dir = repo
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("tau %v: %v\n%s", args, err, out)
		}
	}
	runTau("allow")
	runTau("sync")

	act := filepath.Join(repo, ".taugres", "gen", "activate.bash")
	before, err := os.Stat(act)
	if err != nil {
		t.Fatal(err)
	}

	// Bump the config mtime into the future without changing its content.
	future := before.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(cfgPath, future, future); err != nil {
		t.Fatal(err)
	}
	runTau("sync", "--if-stale")

	after, err := os.Stat(act)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("no-op touch regenerated scripts; hash confirmation missed the early-out")
	}
}

var (
	tauBinOnce sync.Once
	tauBinPath string
	tauBinErr  error
)

// builtTau builds the tau binary once and returns its path, skipping the test
// if the go toolchain is unavailable or the build fails.
func builtTau(t testing.TB) string {
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

// TestUntrustedEntryPrintsSingleTrustMessage guards against the double-message
// regression: entering an untrusted project that is also stale used to print
// two trust messages. Now `tau hook-env` is the single voice and prints it once
// per state.
func TestUntrustedEntryPrintsSingleTrustMessage(t *testing.T) {
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
		t.Helper()
		c := exec.Command(tau, args...)
		c.Dir = repo
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("tau %v: %v\n%s", args, err, out)
		}
	}
	// Trust + sync so a manifest/activate script exist, then deny and edit the
	// config so the next entry is BOTH stale (would trigger auto-sync) and
	// untrusted (would hit both trust gates).
	runTau("allow")
	runTau("sync")
	runTau("deny")
	testutil.WriteFile(t, repo, "workspace.tg", "project(\"demo\")\nshell.env(\"SCOPE\", \"changed\")\n")

	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hookOut, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	// Start in a neutral dir so the hook's install-time run doesn't fire here.
	// Prompt several times: the notice must appear once, not on every prompt (the
	// stale + pre-existing activate script combination used to re-sync and
	// re-activate — and thus re-print — on each prompt).
	script := "cd " + t.TempDir() + "\n" + string(hookOut) + `
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
cd "` + repo + `"; for i in 1 2 3 4 5; do prompt; done`
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	got := string(out)
	if n := strings.Count(got, "not trusted"); n != 1 {
		t.Errorf("expected exactly one not-trusted message, got %d:\n%s", n, got)
	}
	if strings.Contains(got, "SCOPE=changed") || strings.Contains(got, "run `tau sync`") {
		t.Errorf("untrusted entry should not activate or nag tau sync:\n%s", got)
	}
}

// TestUntrustedNoticeOncePerShell verifies the not-trusted notice fires once per
// shell — on first entry — and not again when cd-ing out of and back into the
// untrusted project.
func TestUntrustedNoticeOncePerShell(t *testing.T) {
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
	// Untrusted (never `tau allow`).

	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hookOut, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	neutral := t.TempDir()
	script := "cd " + neutral + "\n" + string(hookOut) + `
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
cd "` + repo + `"; prompt   # first entry -> one notice
prompt                       # same dir -> silent
cd "` + neutral + `"; prompt # leave
cd "` + repo + `"; prompt` // return -> must stay silent
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	if n := strings.Count(string(out), "not trusted"); n != 1 {
		t.Errorf("expected the not-trusted notice exactly once per shell, got %d:\n%s", n, out)
	}
}

// TestHookChildShellReactivates: the TAUGRES_HOOK token is exported so tau can
// read it, which means a nested shell inherits it — but aliases and functions do
// not survive a fork. The shim's unexported _TAU_APPLIED flag tells hook-env the
// claim is inherited, so the child re-activates and regains them.
func TestHookChildShellReactivates(t *testing.T) {
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
		"project(\"demo\")\nshell.env(\"SCOPE\", \"root\")\nshell.fn(\"hi\", shells = [\"bash\", \"zsh\"], content = \"echo hello\")\n")
	for _, cmd := range []string{"allow", "sync"} {
		c := exec.Command(tau, cmd)
		c.Dir = repo
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("tau %s: %v\n%s", cmd, err, out)
		}
	}

	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hookOut, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	hookFile := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(hookFile, hookOut, 0o644); err != nil {
		t.Fatal(err)
	}

	// Parent activates; the child bash (inheriting the exported token and env
	// vars, but not the function) sources the hook and must re-activate.
	script := `cd "` + repo + `"
source "` + hookFile + `"
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
prompt
type hi >/dev/null 2>&1 && echo "PARENT_FN=ok" || echo "PARENT_FN=missing"
bash --noprofile --norc -c '
source "` + hookFile + `"
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
prompt
type hi >/dev/null 2>&1 && echo "CHILD_FN=ok" || echo "CHILD_FN=missing"
echo "CHILD_SCOPE=${SCOPE:-unset}"'`
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{"PARENT_FN=ok", "CHILD_FN=ok", "CHILD_SCOPE=root"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q — nested shell did not re-activate:\n%s", want, got)
		}
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
		"  bin-paths) echo \"" + store + "/$(echo $3 | tr '@' '/')/bin\" ;;\n" +
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
	// must detect the exists() flip and auto-sync so TRIG becomes set. No sleep:
	// the resync can land in the same wall-clock second, so this also guards that
	// the nanosecond activate-mtime stamp distinguishes a same-second resync.
	script := string(hb) + `
prompt() { local c; for c in "${PROMPT_COMMAND[@]}"; do eval "$c"; done; }
cd "` + repo + `"; prompt; echo "BEFORE TRIG=${TRIG:-unset}"
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
	// (the in-process `run` would bake the test binary, and each prompt shells
	// out to `tau hook-env`).
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
alias ll='echo original-alias'
croot() { echo original-function; }
ORIG_PATH="$PATH"
source "` + gen + `/activate.bash"
echo "ACT DEMO_VAR=$DEMO_VAR"
echo "ACT PYTHONPATH=${PYTHONPATH+set}"
echo "ACT ROOT=$TAUGRES_PROJECT_ROOT"
[ "${PATH%%:*}" = "` + dir + `/bin" ] && echo "ACT PATHHEAD=ok"
[ "$(croot; pwd)" = "` + dir + `" ] && echo "ACT croot=overwritten"
alias ll | grep -q 'ls -lah' && echo "ACT alias=overwritten"
source "` + gen + `/deactivate.bash"
echo "DEA DEMO_VAR=$DEMO_VAR"
echo "DEA PYTHONPATH=$PYTHONPATH"
[ "$PATH" = "$ORIG_PATH" ] && echo "DEA PATH=restored"
[ "$(croot)" = "original-function" ] && echo "DEA croot=preserved"
alias ll | grep -q original-alias && echo "DEA alias=preserved"
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
		"ACT croot=overwritten",
		"ACT alias=overwritten",
		"DEA DEMO_VAR=original",
		"DEA PYTHONPATH=/keep/me",
		"DEA PATH=restored",
		"DEA croot=preserved",
		"DEA alias=preserved",
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
