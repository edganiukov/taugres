// Package shellhook generates the shell integration snippet installed via
// `eval "$(tau hook zsh)"`. The hook must be cheap: it never evaluates
// Starlark, runs package managers, or does heavy filesystem scans. It only
// finds the nearest config directory and sources already-generated scripts.
package shellhook

import (
	"fmt"
	"strings"
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
// only in how the hook is wired to directory changes.
func posixHook(shell, tauBin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "_TAU_BIN=%s\n", singleQuote(tauBin))
	b.WriteString(hookCommon)
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

# _tau_teardown deactivates the currently-active project, if any, and forgets it.
_tau_teardown() {
  [ -n "${_TAU_ACTIVE_ROOT:-}" ] || return 0
  _tau_deactivate "$_TAU_ACTIVE_ROOT"
  unset _TAU_ACTIVE_ROOT
}

# The hook runs on every prompt/dir-change. The common case (nothing changed)
# costs only a few stats and no subprocess. It shells out to \x60tau sync --if-stale\x60
# only when the manifest shows drift (guarded by _TAU_TRIED so a failing sync is
# not retried until inputs change), and delegates activation to \x60tau activate\x60.
# It never evaluates Starlark or runs package managers itself.
_tau_hook() {
  local proj gen_dir activate manifest
  proj="$(_tau_find_config)"

  # Outside any project: tear down and stop. Drop the activation token only when
  # something was active, so returning to a never-activated (e.g. untrusted)
  # project does not re-run \x60tau activate\x60 and re-print its notice every cd.
  if [ -z "$proj" ]; then
    [ -n "${_TAU_ACTIVE_ROOT:-}" ] && { _tau_teardown; unset _TAU_ACT_TOKEN; }
    return 0
  fi

  gen_dir="$(_tau_gen_dir "$proj")"
  activate="$gen_dir/activate.$_TAU_SHELL"
  manifest="$gen_dir/manifest"

  # Cheap staleness check using only shell builtins (-f/-nt/-d, command -v) — no
  # subprocess on the common (unchanged) path. One pass over the single manifest,
  # dispatched by line tag: input:<hash>:<path>, tooldir:<path>,
  # probe:<kind>|<arg>|<result>.
  local stale= present=1 probesig= line rest p d kind arg rec now want
  { [ -f "$activate" ] && [ -f "$manifest" ]; } || { stale=1; present=0; }
  if [ -f "$manifest" ]; then
    while IFS= read -r line; do
      case "$line" in
        input:*)
          p=${line#input:}; p=${p#*:}
          [ -n "$p" ] && [ "$p" -nt "$manifest" ] && stale=1 ;;
        tooldir:*)
          d=${line#tooldir:}
          [ -n "$d" ] && [ ! -d "$d" ] && { stale=1; present=0; } ;;
        probe:*)
          rest=${line#probe:}
          kind=${rest%%|*}; rest=${rest#*|}; arg=${rest%|*}; rec=${rest##*|}
          case "$kind" in
            exists) [ -e "$arg" ] && now=1 || now=0; want=$rec ;;
            which)  command -v "$arg" >/dev/null 2>&1 && now=1 || now=0
                    [ -n "$rec" ] && want=1 || want=0 ;;
            *) now=0; want=0 ;;
          esac
          probesig="$probesig|$now"
          [ "$now" = "$want" ] || stale=1 ;;
      esac
    done < "$manifest"
  fi

  if [ -n "$stale" ] && [ -n "${_TAU_BIN:-}" ]; then
    # Retry token so a failing sync isn't retried until inputs change. Reading
    # mtimes runs stat, but only here on the (rare) stale path; the probe signal
    # is folded in so a genuine probe flip forces exactly one resync.
    local newest m
    newest="$(_tau_mtime "$(_tau_config_file "$proj")")"
    [ -n "$newest" ] || newest=0
    if [ -f "$manifest" ]; then
      while IFS= read -r line; do
        case "$line" in
          input:*)
            p=${line#input:}; p=${p#*:}
            [ -n "$p" ] || continue
            m="$(_tau_mtime "$p")"
            [ -n "$m" ] && [ "$m" -gt "$newest" ] && newest="$m" ;;
        esac
      done < "$manifest"
    fi
    local _tau_tok="$proj|$present|$newest|$probesig"
    if [ "$_tau_tok" != "${_TAU_TRIED:-}" ]; then
      _TAU_TRIED="$_tau_tok"
      # If this project is active, tear it down with the CURRENT deactivate
      # script before the sync regenerates it, so removed vars/PATH don't leak.
      [ "${_TAU_ACTIVE_ROOT:-}" = "$proj" ] && _tau_teardown
      local _tau_pre; _tau_pre="$(_tau_mtime "$activate")"
      ( cd "$proj" && "$_TAU_BIN" sync --if-stale )
      unset _TAU_ACT_TOKEN   # force (re)activation below (activate mtime is 1s-granular)
      # Re-arm the guard only if the sync actually regenerated the env (activate
      # mtime changed): a teardown (rm -rf .taugres) then re-syncs, but a no-op
      # sync (e.g. an untrusted project) is not retried every prompt.
      local _tau_post; _tau_post="$(_tau_mtime "$activate")"
      [ -n "$_tau_post" ] && [ "$_tau_pre" != "$_tau_post" ] && unset _TAU_TRIED
    fi
  fi

  # (Re)activate on entering/switching, or when the env changed. Delegated to
  # \x60tau activate\x60, which refuses untrusted projects (trust lives outside the
  # repo, so a cloned repo can't run code on cd) and is the single voice for
  # "not trusted"/"not synced". Guarded by _TAU_ACT_TOKEN (the activate mtime) so
  # it runs at most once per state.
  local stamp; stamp="$(_tau_mtime "$activate")"
  local acttok="$proj|$stamp"
  if [ "$acttok" != "${_TAU_ACT_TOKEN:-}" ]; then
    _TAU_ACT_TOKEN="$acttok"
    _tau_teardown
    if [ -n "${_TAU_BIN:-}" ]; then
      local _tau_script
      _tau_script="$( ( cd "$proj" && "$_TAU_BIN" activate "$_TAU_SHELL" ) )"
      [ -n "$_tau_script" ] && { eval "$_tau_script"; _TAU_ACTIVE_ROOT="$proj"; }
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
