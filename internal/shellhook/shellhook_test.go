package shellhook

import (
	"strings"
	"testing"
)

func TestHookContainsAutoSync(t *testing.T) {
	out, err := Hook("bash", "/usr/local/bin/tau")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"_TAU_BIN='/usr/local/bin/tau'",
		"sync --if-stale",
		`activate "$_TAU_SHELL"`, // activation delegated to `tau activate` (trust gate)
		`-nt "$manifest"`,        // staleness: an input newer than the manifest
		`"$gen_dir/manifest"`,    // single tagged state file
		"input:*",                // config inputs tracked
		"tooldir:*",              // tool dirs tracked
		"probe:*",                // exists()/which() probes tracked
		`"$gen_dir/tried"`,       // tau-owned retry guard (storm protection)
		"_TAU_ACT_TOKEN",         // re-activate when the generated env changes
		"_tau_hook",
		"PROMPT_COMMAND",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bash hook missing %q", want)
		}
	}
	if strings.Contains(out, ".trusted") {
		t.Errorf("hook must not gate on the forgeable in-repo .trusted marker:\n%s", out)
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

func TestHookFishUsesOnVariablePwd(t *testing.T) {
	out, err := Hook("fish", "/usr/local/bin/tau")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set -g _TAU_BIN '/usr/local/bin/tau'",
		"function _tau_hook --on-variable PWD",
		"sync --if-stale",
		"activate fish",
		`-nt "$manifest"`,
		"$gen_dir/manifest",
		"$gen_dir/tried",
		"_TAU_ACT_TOKEN",
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
