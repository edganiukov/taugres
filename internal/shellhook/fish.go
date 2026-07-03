package shellhook

import (
	"fmt"
	"strings"
)

// fishHook returns the fish shell hook: the same minimal shim as bash/zsh in
// fish syntax, wired to every prompt via the fish_prompt event (so a config
// edit takes effect on the next prompt, like bash/zsh — not only on cd).
func fishHook(tauBin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "set -g _TAU_BIN %s\n", FishSingleQuote(tauBin))
	b.WriteString(fishHookBody)
	return b.String()
}

// FishSingleQuote wraps s as a fish single-quoted literal.
func FishSingleQuote(s string) string {
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

function _tau_hook --on-event fish_prompt
    set -l proj (_tau_find_config)
    if test -z "$proj"
        # Outside any project with nothing sourced (empty or applied-bit 0):
        # nothing to tear down, so spawn nothing.
        if test -z "$TAUGRES_HOOK"; or string match -q -- '0|*' "$TAUGRES_HOOK"
            return 0
        end
    end
    $_TAU_BIN hook-env fish | source
end

# Run once for the current directory.
_tau_hook
`
