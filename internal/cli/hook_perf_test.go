package cli

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/edganiukov/taugres/internal/testutil"
)

// TestHookTransitionPerformance times the transition cycle
// outside -> workspace -> nested -> workspace -> outside and asserts it stays
// well under the 100ms-per-change target. Each in-project prompt runs one
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

	if perTransition > 100*time.Millisecond {
		t.Errorf("per-transition time %s exceeds 100ms target", perTransition)
	}
}
