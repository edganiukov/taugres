package shellhook

import (
	"fmt"
	"strings"
)

// fishHook returns the fish shell hook. It mirrors the bash/zsh hook logic but
// in fish syntax, wired to directory changes via `--on-variable PWD`.
func fishHook(tauBin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "set -g _TAU_BIN %s\n", fishSingleQuote(tauBin))
	b.WriteString(fishHookBody)
	return b.String()
}

// fishSingleQuote wraps s as a fish single-quoted literal.
func fishSingleQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

const fishHookBody = `# tau shell hook (generated). Do not edit.
function _tau_find_config
    set -l dir $PWD
    while test -n "$dir"
        if test -f "$dir/project.tg"; or test -f "$dir/workspace.tg"
            echo $dir
            return 0
        end
        test "$dir" = "/"; and break
        set dir (string replace -r '/[^/]*$' '' -- $dir)
        test -z "$dir"; and set dir "/"
    end
    return 1
end

function _tau_gen_dir
    echo "$argv[1]/.taugres/gen"
end

function _tau_config_file
    if test -f "$argv[1]/project.tg"
        echo "$argv[1]/project.tg"
    else
        echo "$argv[1]/workspace.tg"
    end
end

function _tau_mtime
    stat -c %Y $argv[1] 2>/dev/null; or stat -f %m $argv[1] 2>/dev/null
end

function _tau_deactivate
    set -l d (_tau_gen_dir "$argv[1]")/deactivate.fish
    test -f "$d"; and source "$d"
end

# _tau_teardown deactivates the currently-active project, if any, and forgets it.
function _tau_teardown
    set -q _TAU_ACTIVE_ROOT; and test -n "$_TAU_ACTIVE_ROOT"; or return 0
    _tau_deactivate "$_TAU_ACTIVE_ROOT"
    set -e _TAU_ACTIVE_ROOT
end

# Runs on every directory change. The common case (nothing changed) costs only a
# few stats and no subprocess. It shells out to \x60tau sync --if-stale\x60 only when
# the manifest shows drift (guarded by _TAU_TRIED so a failing sync is not
# retried until inputs change), and delegates activation to \x60tau activate\x60. It
# never evaluates Starlark or runs package managers itself.
function _tau_hook --on-variable PWD
    set -l proj (_tau_find_config)

    # Outside any project: tear down and stop. Drop the activation token only when
    # something was active, so returning to a never-activated (e.g. untrusted)
    # project does not re-run \x60tau activate\x60 and re-print its notice every cd.
    if test -z "$proj"
        if set -q _TAU_ACTIVE_ROOT; and test -n "$_TAU_ACTIVE_ROOT"
            _tau_teardown
            set -e _TAU_ACT_TOKEN
        end
        return 0
    end

    set -l gen_dir (_tau_gen_dir "$proj")
    set -l activate "$gen_dir/activate.fish"
    set -l manifest "$gen_dir/manifest"

    # Cheap staleness check using only builtins (-f/-nt/-d, command -s) — no
    # subprocess on the common (unchanged) path. One pass over the single
    # manifest, dispatched by line tag: input:<hash>:<path>, tooldir:<path>,
    # probe:<kind>|<arg>|<result>.
    set -l stale
    set -l present 1
    set -l probesig ""
    if test ! -f "$activate"; or test ! -f "$manifest"
        set stale 1; set present 0
    end
    if test -f "$manifest"
        while read -l line
            switch "$line"
                case 'input:*'
                    set -l p (string replace -r '^input:[^:]*:' '' -- $line)
                    if test -n "$p"; and test "$p" -nt "$manifest"
                        set stale 1
                    end
                case 'tooldir:*'
                    set -l d (string replace -r '^tooldir:' '' -- $line)
                    if test -n "$d"; and test ! -d "$d"
                        set stale 1
                        set present 0
                    end
                case 'probe:*'
                    set -l spec (string replace -r '^probe:' '' -- $line)
                    set -l kind (string split -m1 '|' -- $spec)[1]
                    set -l tail (string split -m1 '|' -- $spec)[2]
                    set -l arg (string split -r -m1 '|' -- $tail)[1]
                    set -l rec (string split -r -m1 '|' -- $tail)[2]
                    set -l now 0
                    set -l want 0
                    switch "$kind"
                        case exists
                            test -e "$arg"; and set now 1
                            test "$rec" = 1; and set want 1
                        case which
                            command -s "$arg" >/dev/null 2>&1; and set now 1
                            test -n "$rec"; and set want 1
                    end
                    set probesig "$probesig|$now"
                    test "$now" = "$want"; or set stale 1
            end
        end <"$manifest"
    end

    if test -n "$stale"; and test -n "$_TAU_BIN"
        # Retry token so a failing sync isn't retried until inputs change;
        # reading mtimes runs stat, but only here on the (rare) stale path. The
        # probe signal is folded in so a genuine probe flip forces one resync.
        set -l newest (_tau_mtime (_tau_config_file "$proj"))
        test -n "$newest"; or set newest 0
        if test -f "$manifest"
            while read -l line
                switch "$line"
                    case 'input:*'
                        set -l p (string replace -r '^input:[^:]*:' '' -- $line)
                        test -n "$p"; or continue
                        set -l m (_tau_mtime "$p")
                        if test -n "$m"; and test "$m" -gt "$newest"
                            set newest $m
                        end
                end
            end <"$manifest"
        end
        set -l _tau_tok "$proj|$present|$newest|$probesig"
        if test "$_tau_tok" != "$_TAU_TRIED"
            set -g _TAU_TRIED "$_tau_tok"
            # If this project is active, tear it down with the CURRENT deactivate
            # script before the sync regenerates it, so removed vars/PATH don't leak.
            test "$_TAU_ACTIVE_ROOT" = "$proj"; and _tau_teardown
            set -l _tau_pre (_tau_mtime "$activate")
            if pushd "$proj" 2>/dev/null
                $_TAU_BIN sync --if-stale
                popd
            end
            set -e _TAU_ACT_TOKEN # force (re)activation below (activate mtime is 1s-granular)
            # Re-arm the guard only if the sync actually regenerated the env
            # (activate mtime changed): a teardown (rm -rf .taugres) then re-syncs,
            # but a no-op sync (e.g. an untrusted project) is not retried every prompt.
            set -l _tau_post (_tau_mtime "$activate")
            test -n "$_tau_post"; and test "$_tau_pre" != "$_tau_post"; and set -e _TAU_TRIED
        end
    end

    # (Re)activate on entering/switching, or when the env changed. Delegated to
    # \x60tau activate\x60, which refuses untrusted projects (trust lives outside the
    # repo, so a cloned repo can't run code on cd) and is the single voice for
    # "not trusted"/"not synced". Guarded by _TAU_ACT_TOKEN (the activate mtime)
    # so it runs at most once per state.
    set -l stamp (_tau_mtime "$activate")
    set -l acttok "$proj|$stamp"
    if test "$acttok" != "$_TAU_ACT_TOKEN"
        set -g _TAU_ACT_TOKEN "$acttok"
        _tau_teardown
        if test -n "$_TAU_BIN"
            set -l _tau_script ($_TAU_BIN activate fish | string collect)
            if test -n "$_tau_script"
                echo "$_tau_script" | source
                set -g _TAU_ACTIVE_ROOT $proj
            end
        end
    end
end

# Run once for the current directory.
_tau_hook
`
