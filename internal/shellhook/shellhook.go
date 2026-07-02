// Package shellhook generates the shell integration snippet installed via
// `eval "$(tau hook zsh)"`. The hook is a minimal shim: it finds the nearest
// config directory with pure shell (so prompts outside any project spawn no
// subprocess) and delegates everything else — staleness, auto-sync, trust,
// activation — to `tau hook-env`, which owns all logic in Go and round-trips
// session state through the TAUGRES_HOOK env var set by its own eval'd output.
package shellhook

import (
	"fmt"
	"strings"
)

// SupportedShells lists shells with hook support.
var SupportedShells = []string{"bash", "zsh", "fish"}

// Hook returns the shell hook script for the given shell. tauBin is the path to
// the tau executable the shim invokes as `tau hook-env <shell>`.
func Hook(shell, tauBin string) (string, error) {
	switch shell {
	case "bash", "zsh":
		return posixHook(shell, tauBin), nil
	case "fish":
		return fishHook(tauBin), nil
	default:
		return "", fmt.Errorf("unsupported shell for hook: %q (supported: bash, zsh, fish)", shell)
	}
}

// posixHook returns a bash/zsh compatible hook. The shell-specific difference is
// only in how the hook is wired to prompts/directory changes.
func posixHook(shell, tauBin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "_TAU_BIN=%s\n", SingleQuote(tauBin))
	b.WriteString(hookBody)
	switch shell {
	case "zsh":
		b.WriteString(zshWiring)
	case "bash":
		b.WriteString(bashWiring)
	}

	return b.String()
}

// SingleQuote wraps s as a POSIX single-quoted literal.
func SingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookBody is the entire hook: a pure-shell walk to the nearest config dir as a
// gate, then one `tau hook-env` invocation whose output is eval'd. TAUGRES_HOOK
// holds the session state as "<applied>|..."; the leading applied bit is 0 when
// nothing is sourced, so outside any project an empty or "0"-prefixed token
// means there is nothing to tear down and the prompt spawns nothing.
const hookBody = `# tau shell hook (generated). Do not edit.
_tau_find_config() {
  # Walk upward from $PWD to the nearest project.tg or workspace.tg directory.
  local dir="$PWD"
  while [ -n "$dir" ]; do
    if [ -f "$dir/project.tg" ] || [ -f "$dir/workspace.tg" ]; then
      printf '%s\n' "$dir"
      return 0
    fi
    [ "$dir" = "/" ] && break
    dir="${dir%/*}"
    [ -z "$dir" ] && dir="/"
  done
  return 1
}

_tau_hook() {
  local proj
  proj="$(_tau_find_config)"
  if [ -z "$proj" ]; then
    # Outside any project with nothing sourced (empty or applied-bit 0): nothing
    # to tear down, so spawn nothing.
    case "${TAUGRES_HOOK:-}" in ""|0\|*) return 0 ;; esac
  fi
  # _TAU_APPLIED is set (unexported) by the eval'd output when THIS shell
  # sourced the activate script. A child shell inherits the exported token but
  # not this flag (or the aliases/functions), so hook-env re-activates there.
  eval "$("$_TAU_BIN" hook-env "$_TAU_SHELL" "${_TAU_APPLIED:-}")"
}
`

const zshWiring = `
_TAU_SHELL=zsh
autoload -U add-zsh-hook 2>/dev/null
if typeset -f add-zsh-hook >/dev/null 2>&1; then
  add-zsh-hook chpwd _tau_hook
  add-zsh-hook precmd _tau_hook
fi
# Run once for the current directory.
_tau_hook
`

// bashWiring registers the hook into PROMPT_COMMAND without clobbering an
// existing one. It preserves a user's custom PROMPT_COMMAND and handles both
// the scalar-string form and the bash 5.1+ array form. Install this at (or
// near) the end of ~/.bashrc, after your own PROMPT_COMMAND setup.
const bashWiring = `
_TAU_SHELL=bash
_tau_prompt_hook() {
  _tau_hook
}
if [[ "${PROMPT_COMMAND[*]:-}" != *_tau_prompt_hook* ]]; then
  if [[ "$(declare -p PROMPT_COMMAND 2>/dev/null)" == "declare -a"* ]]; then
    # Array form (bash >= 5.1): prepend so tau runs before the rest.
    PROMPT_COMMAND=(_tau_prompt_hook "${PROMPT_COMMAND[@]}")
  else
    # Scalar form: keep any existing command after ours.
    PROMPT_COMMAND="_tau_prompt_hook${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
  fi
fi
# Run once for the current directory.
_tau_hook
`
