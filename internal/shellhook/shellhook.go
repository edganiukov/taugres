// Package shellhook generates the shell integration snippet installed via
// `eval "$(tau hook zsh)"`. The hook must be cheap: it never evaluates
// Starlark, runs package managers, or does heavy filesystem scans. It only
// finds the nearest config directory and sources already-generated scripts.
package shellhook

import (
	"fmt"
	"strings"

	"github.com/edganiukov/taugres/internal/state"
)

// SupportedShells lists shells with hook support.
var SupportedShells = []string{"bash", "zsh", "fish"}

// Hook returns the shell hook script for the given shell. tauBin is the path to
// the tau executable, invoked (as `tau sync --if-stale`) only when the cheap
// staleness check indicates the environment needs regenerating.
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
// only in how the hook is wired to directory changes. The per-check staleness
// fragments are spliced in from the state package so both the hook and Go stay
// in sync as checks are added.
func posixHook(shell, tauBin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "_TAU_BIN=%s\n", singleQuote(tauBin))
	body := strings.Replace(hookCommon, "__TAU_DETECT__\n", state.ShellDetect(state.Posix), 1)
	body = strings.Replace(body, "__TAU_TOKEN__\n", state.ShellToken(state.Posix), 1)
	b.WriteString(body)
	switch shell {
	case "zsh":
		b.WriteString(zshWiring)
	case "bash":
		b.WriteString(bashWiring)
	}
	return b.String()
}

// singleQuote wraps s as a POSIX single-quoted literal.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookCommon contains the core logic shared by bash and zsh. It caches the
// active project root in _TAU_ACTIVE_ROOT to avoid work when the directory has
// not changed projects.
const hookCommon = `# tau shell hook (generated). Do not edit.
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

_tau_gen_dir() {
  printf '%s/.taugres/gen' "$1"
}

_tau_config_file() {
  if [ -f "$1/project.tg" ]; then
    printf '%s/project.tg' "$1"
  else
    printf '%s/workspace.tg' "$1"
  fi
}

# _tau_mtime prints a file's modification time in seconds (GNU or BSD stat).
_tau_mtime() {
  stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null
}

# _tau_deactivate sources a project's deactivate script if present.
_tau_deactivate() {
  local d="$(_tau_gen_dir "$1")/deactivate.$_TAU_SHELL"
  [ -f "$d" ] && source "$d"
}

# The hook runs on every prompt/dir-change. The common case (nothing changed)
# costs only a few stats. It shells out to \x60tau sync\x60 only when a config input
# changed or a generated script/tool dir is missing — and never re-runs a failed
# sync for the same inputs (guarded by _TAU_TRIED). It never evaluates Starlark
# or runs package managers itself.
_tau_hook() {
  local proj gen_dir activate manifest
  proj="$(_tau_find_config)"

  # Outside any project: tear down an active env and stop.
  if [ -z "$proj" ]; then
    if [ -n "${_TAU_ACTIVE_ROOT:-}" ]; then
      _tau_deactivate "$_TAU_ACTIVE_ROOT"
      unset _TAU_ACTIVE_ROOT
    fi
    unset _TAU_ACT_TOKEN
    return 0
  fi

  gen_dir="$(_tau_gen_dir "$proj")"
  activate="$gen_dir/activate.$_TAU_SHELL"
  manifest="$gen_dir/manifest.json"

  # Cheap staleness check using only shell builtins (-f/-nt/-d, command -v) — no
  # subprocess on the common (unchanged) path. Each dimension's fragment is
  # spliced in from the state package (__TAU_DETECT__); the retry token is built
  # from matching fragments (__TAU_TOKEN__) only when stale.
  local stale= present=1 probesig= f
  { [ -f "$activate" ] && [ -f "$manifest" ]; } || { stale=1; present=0; }
__TAU_DETECT__

  if [ -n "$stale" ] && [ -n "${_TAU_BIN:-}" ]; then
    # Token so a failing sync isn't retried until inputs change. Reading mtimes
    # runs stat, but only here on the (rare) stale path.
    local _tau_tok="$proj|$present"
__TAU_TOKEN__
    if [ "$_tau_tok" != "${_TAU_TRIED:-}" ]; then
      _TAU_TRIED="$_tau_tok"
      # If this project's env is already active, tear it down with the matching
      # (current) deactivate script before regenerating, so removed vars/PATH do
      # not leak once the scripts change.
      if [ "${_TAU_ACTIVE_ROOT:-}" = "$proj" ]; then
        _tau_deactivate "$proj"
        unset _TAU_ACTIVE_ROOT
      fi
      ( cd "$proj" && "$_TAU_BIN" sync --if-stale )
    fi
  fi

  if [ ! -f "$activate" ]; then
    printf 'tau: environment is not synced; run \x60tau sync\x60\n' >&2
    return 0
  fi

  # (Re)activate on entering/switching projects, or when the generated env
  # changed (activate regenerated -> newer mtime). Activation is delegated to
  # \x60tau activate\x60 rather than sourcing the file directly: tau refuses to emit
  # the script for a project not trusted on THIS machine, so a cloned repo that
  # ships a pre-generated activate script cannot run code on cd. Trust lives
  # outside the repo and cannot be forged by repo contents.
  local stamp; stamp="$(_tau_mtime "$activate")"
  local acttok="$proj|$stamp"
  if [ "$acttok" != "${_TAU_ACT_TOKEN:-}" ]; then
    _TAU_ACT_TOKEN="$acttok"
    if [ -n "${_TAU_ACTIVE_ROOT:-}" ]; then
      _tau_deactivate "$_TAU_ACTIVE_ROOT"
      unset _TAU_ACTIVE_ROOT
    fi
    if [ -n "${_TAU_BIN:-}" ]; then
      local _tau_script
      _tau_script="$( ( cd "$proj" && "$_TAU_BIN" activate "$_TAU_SHELL" ) )"
      if [ -n "$_tau_script" ]; then
        eval "$_tau_script"
        _TAU_ACTIVE_ROOT="$proj"
      fi
    fi
  fi
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
