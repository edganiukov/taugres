package shellhook

import (
	"strings"
	"testing"
)

func TestHookDelegatesToHookEnv(t *testing.T) {
	out, err := Hook("bash", "/usr/local/bin/tau")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"_TAU_BIN='/usr/local/bin/tau'",
		`hook-env "$_TAU_SHELL"`, // all logic delegated to tau
		"_tau_find_config",       // pure-shell gate for out-of-project prompts
		"TAUGRES_HOOK",           // session state round-trips via env var
		`""|0\|*`,                // dormant states spawn nothing outside projects
		"_tau_hook",
		"PROMPT_COMMAND",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bash hook missing %q", want)
		}
	}
	if strings.Contains(out, ".trusted") {
		t.Errorf("hook must not gate on a forgeable in-repo marker:\n%s", out)
	}
	// The shim must hold no hook logic: no manifest parsing, no sync invocation.
	for _, reject := range []string{"manifest", "sync --if-stale", "probe"} {
		if strings.Contains(out, reject) {
			t.Errorf("bash hook should delegate %q handling to tau hook-env:\n%s", reject, out)
		}
	}
}

func TestHookZshUsesChpwd(t *testing.T) {
	out, err := Hook("zsh", "tau")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "add-zsh-hook chpwd _tau_hook") {
		t.Errorf("zsh hook should register a chpwd hook:\n%s", out)
	}
}

func TestHookQuotesBinPath(t *testing.T) {
	out, err := Hook("bash", "/path/with space/tau")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "_TAU_BIN='/path/with space/tau'") {
		t.Errorf("bin path not single-quoted:\n%s", out)
	}
}

func TestHookFishUsesPromptEvent(t *testing.T) {
	out, err := Hook("fish", "/usr/local/bin/tau")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set -g _TAU_BIN '/usr/local/bin/tau'",
		"function _tau_hook --on-event fish_prompt",
		`hook-env fish "$_TAU_APPLIED" | source`,
		"_tau_find_config",
		"TAUGRES_HOOK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fish hook missing %q", want)
		}
	}
}

func TestHookUnsupportedShell(t *testing.T) {
	if _, err := Hook("powershell", "tau"); err == nil {
		t.Error("expected error for unsupported shell")
	}
}
