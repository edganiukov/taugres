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

# Runs on every directory change. The common case (nothing changed) costs only a
# few stats. It shells out to tau sync only when a config input changed or a
# generated script / tool dir is missing, and never re-runs a failed sync for
# the same inputs. It never evaluates Starlark or runs package managers itself.
function _tau_hook --on-variable PWD
    set -l proj (_tau_find_config)

    # Outside any project: tear down an active env and stop.
    if test -z "$proj"
        if set -q _TAU_ACTIVE_ROOT; and test -n "$_TAU_ACTIVE_ROOT"
            _tau_deactivate "$_TAU_ACTIVE_ROOT"
            set -e _TAU_ACTIVE_ROOT
        end
        set -e _TAU_ACT_TOKEN
        return 0
    end

    set -l gen_dir (_tau_gen_dir "$proj")
    set -l activate "$gen_dir/activate.fish"
    set -l manifest "$gen_dir/manifest.json"

    # Cheap staleness check using only builtins (-f/-nt/-d) — no subprocess on
    # the common (unchanged) path. The mtime token is computed only when stale.
    set -l stale
    set -l present 1
    if test ! -f "$activate"; or test ! -f "$manifest"
        set stale 1; set present 0
    else if test -f "$gen_dir/sources"
        for f in (cat "$gen_dir/sources")
            if test -n "$f"; and test "$f" -nt "$manifest"
                set stale 1
                break
            end
        end
    end
    if test -f "$gen_dir/tooldirs"
        for d in (cat "$gen_dir/tooldirs")
            if test -n "$d"; and test ! -d "$d"
                set stale 1; set present 0
                break
            end
        end
    end

    if test -n "$stale"; and test -n "$_TAU_BIN"
        # Token so a failing sync isn't retried until inputs change; reading
        # mtimes runs stat, but only here on the (rare) stale path.
        set -l newest (_tau_mtime (_tau_config_file "$proj"))
        test -n "$newest"; or set newest 0
        if test -f "$gen_dir/sources"
            for f in (cat "$gen_dir/sources")
                test -n "$f"; or continue
                set -l m (_tau_mtime "$f")
                if test -n "$m"; and test "$m" -gt "$newest"
                    set newest $m
                end
            end
        end
        set -l tok "$proj|$newest|$present"
        if test "$tok" != "$_TAU_TRIED"
            set -g _TAU_TRIED "$tok"
            # Tear down this project's active env with the matching deactivate
            # before regenerating, so removed vars/PATH do not leak.
            if test "$_TAU_ACTIVE_ROOT" = "$proj"
                _tau_deactivate "$proj"
                set -e _TAU_ACTIVE_ROOT
            end
            if pushd "$proj" 2>/dev/null
                $_TAU_BIN sync --if-stale
                popd
            end
        end
    end

    if test ! -f "$activate"
        printf 'tau: environment is not synced; run \x60tau sync\x60\n' >&2
        return 0
    end

    # (Re)activate on entering/switching, or when the generated env changed.
    # Activation is delegated to \x60tau activate\x60, which refuses to emit a script
    # for a project not trusted on THIS machine, so a cloned repo cannot run code
    # on cd. Trust lives outside the repo and cannot be forged by repo contents.
    set -l stamp (_tau_mtime "$activate")
    set -l acttok "$proj|$stamp"
    if test "$acttok" != "$_TAU_ACT_TOKEN"
        set -g _TAU_ACT_TOKEN "$acttok"
        if set -q _TAU_ACTIVE_ROOT; and test -n "$_TAU_ACTIVE_ROOT"
            _tau_deactivate "$_TAU_ACTIVE_ROOT"
            set -e _TAU_ACTIVE_ROOT
        end
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
