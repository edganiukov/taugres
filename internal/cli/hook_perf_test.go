package cli

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"go.gnkv.dev/taugres/internal/shellhook"
	"go.gnkv.dev/taugres/internal/testutil"
)

// TestHookTransitionPerformance times the transition cycle
// outside -> workspace -> nested -> workspace -> outside and asserts it stays
// well under the 100ms-per-change target. The hook is pure shell and never
// invokes tau or evaluates Starlark on the hot path.
func TestHookTransitionPerformance(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	isolate(t)

	repo := testutil.TempWorkspace(t)
	testutil.WriteFile(t, repo, "workspace.tg", "project(\"root\")\nshell.env(\"SCOPE\",\"root\")\n")
	testutil.WriteFile(t, repo, "svc/project.tg", "project(\"svc\")\nshell.env(\"SCOPE\",\"svc\")\n")

	for _, sub := range []string{"", "svc"} {
		wd := repo
		if sub != "" {
			wd = repo + "/" + sub
		}
		if code, _, e := run(t, wd, "allow"); code != 0 {
			t.Fatalf("allow %s: %s", sub, e)
		}
		if code, _, e := run(t, wd, "sync"); code != 0 {
			t.Fatalf("sync %s: %s", sub, e)
		}
	}

	// Empty tau path: the fast path must never shell out for fresh projects.
	hook, err := shellhook.Hook("bash", "")
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 50
	script := hook + fmt.Sprintf(`
_TAU_SHELL=bash
for i in $(seq 1 %d); do
  cd /tmp;          _tau_hook
  cd "%s";          _tau_hook
  cd "%s/svc";      _tau_hook
  cd "%s";          _tau_hook
done
`, iterations, repo, repo, repo)

	start := time.Now()
	out, err := exec.Command(bash, "--noprofile", "--norc", "-c", script).CombinedOutput()
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
