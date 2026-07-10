package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edganiukov/taugres/internal/shellhook"
	"github.com/edganiukov/taugres/internal/testutil"
)

// TestHookTransitionPerformance times the transition cycle
// outside -> workspace -> nested -> workspace -> outside and asserts it stays
// under the 20ms-per-change product target. Each in-project prompt runs one
// `tau hook-env` (all logic in Go); prompts outside any project are pure shell.
func TestHookTransitionPerformance(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	tau := builtTau(t)

	cfgHome := t.TempDir()
	cacheHome := t.TempDir()
	env := append(os.Environ(), "XDG_CONFIG_HOME="+cfgHome, "XDG_CACHE_HOME="+cacheHome)

	repo := testutil.TempWorkspace(t)
	testutil.WriteFile(t, repo, "workspace.tg", "project(\"root\")\nshell.env(\"SCOPE\",\"root\")\n")
	testutil.WriteFile(t, repo, "svc/project.tg", "project(\"svc\")\nshell.env(\"SCOPE\",\"svc\")\n")

	for _, sub := range []string{"", "svc"} {
		wd := repo
		if sub != "" {
			wd = repo + "/" + sub
		}
		for _, cmd := range []string{"allow", "sync"} {
			c := exec.Command(tau, cmd)
			c.Dir = wd
			c.Env = env
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("tau %s %s: %v\n%s", cmd, sub, err, out)
			}
		}
	}

	hookCmd := exec.Command(tau, "hook", "bash")
	hookCmd.Env = env
	hook, err := hookCmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}

	const iterations = 50
	script := string(hook) + fmt.Sprintf(`
for i in $(seq 1 %d); do
  cd /tmp;          _tau_hook
  cd "%s";          _tau_hook
  cd "%s/svc";      _tau_hook
  cd "%s";          _tau_hook
done
`, iterations, repo, repo, repo)

	start := time.Now()
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash failed: %v\n%s", err, out)
	}
	elapsed := time.Since(start)

	// 4 transitions per iteration; subtract nothing for process startup to be
	// conservative.
	transitions := iterations * 4
	perTransition := elapsed / time.Duration(transitions)
	t.Logf("total=%s for %d transitions, ~%s each", elapsed, transitions, perTransition)

	if perTransition > 20*time.Millisecond {
		t.Errorf("per-transition time %s exceeds 20ms target", perTransition)
	}
}

// TestHookOutsideProjectDoesNotSpawn asserts the fast path stays pure shell:
// a prompt outside any project (with nothing active) must not exec tau. It
// points _TAU_BIN at a nonexistent binary so any spawn would error loudly.
func BenchmarkHookEnvSteadyState(b *testing.B) {
	tau := builtTau(b)
	cfgHome := b.TempDir()
	repo := testutil.TempWorkspace(b)
	testutil.WriteFile(b, repo, "workspace.tg", "project(\"bench\")\nshell.env(\"A\", \"b\")\n")
	deep := filepath.Join(repo, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		b.Fatal(err)
	}
	env := append(os.Environ(), "XDG_CONFIG_HOME="+cfgHome)
	for _, subcommand := range []string{"allow", "sync"} {
		cmd := exec.Command(tau, subcommand)
		cmd.Dir = repo
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("tau %s: %v\n%s", subcommand, err, out)
		}
	}

	// Let bash evaluate the first transition and return only the exported token.
	setup := exec.Command("bash", "--noprofile", "--norc", "-c",
		`eval "$("$TAU" hook-env bash "" "$PROJECT")"; printf '%s' "$TAUGRES_HOOK"`)
	setup.Dir = deep
	setup.Env = append(env, "TAU="+tau, "PROJECT="+repo)
	token, err := setup.Output()
	if err != nil {
		b.Fatal(err)
	}

	steadyEnv := append(env, "TAUGRES_HOOK="+string(token))
	b.ResetTimer()
	for range b.N {
		cmd := exec.Command(tau, "hook-env", "bash", "1", repo)
		cmd.Dir = deep
		cmd.Env = steadyEnv
		out, err := cmd.Output()
		if err != nil {
			b.Fatal(err)
		}
		if len(out) != 0 {
			b.Fatalf("steady hook emitted %q", out)
		}
	}
}

func TestHookOutsideProjectDoesNotSpawn(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	hook, err := shellhook.Hook("bash", "/nonexistent/tau")
	if err != nil {
		t.Fatal(err)
	}
	// Start outside any project, TAUGRES_HOOK unset; run the hook repeatedly.
	script := "cd " + t.TempDir() + "\n" + string(hook) + `
for i in $(seq 1 20); do _tau_hook; done
echo DONE`
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	cmd.Env = append(os.Environ(), "TAUGRES_HOOK=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash failed: %v\n%s", err, out)
	}
	got := string(out)
	if strings.TrimSpace(got) != "DONE" {
		t.Errorf("outside-project prompt spawned tau (or produced output): %q", got)
	}
}
