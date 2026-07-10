package render

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/edganiukov/taugres/internal/model"
)

// fishActivate renders the fish activation script.
func fishActivate(p *model.Plan) string {
	var b strings.Builder
	w := &b

	header(w, "fish", "activate")

	// Taugres built-in variables.
	fmt.Fprintln(w, "# --- Taugres built-in variables ---")
	fishExport(w, "TAUGRES_ACTIVE", "1")
	fishExport(w, "TAUGRES_ROOT", p.RepoRoot)
	fishExport(w, "TAUGRES_REPO_ROOT", p.RepoRoot)
	fishExport(w, "TAUGRES_PROJECT_ROOT", p.ProjectRoot)
	fishExport(w, "TAUGRES_CONFIG", p.ConfigPath)
	fishExport(w, "TAUGRES_LOCK", p.ProjectRoot+"/.taugres.lock")
	fishExport(w, "TAUGRES_STATE", p.StateDir)
	fmt.Fprintln(w)

	fishEnv(w, p)
	fishExecEnv(w, p)
	fishPath(w, p)
	fishAliases(w, p)
	fishFunctions(w, p)
	fishHooks(w, p)
	fishNotice(w, p)

	fmt.Fprintln(w, "# --- end activation ---")
	return b.String()
}

// fishDeactivate renders the fish deactivation script.
func fishDeactivate(p *model.Plan) string {
	var b strings.Builder
	w := &b

	header(w, "fish", "deactivate")

	// Restore environment variables.
	fmt.Fprintln(w, "# --- restore environment ---")
	for _, name := range envRestoreOrder(p) {
		saveVar := helperPrefix + "ENV_" + name
		fmt.Fprintf(w, "if test \"$%s__set\" = 1\n", saveVar)
		fmt.Fprintf(w, "    set -gx %s \"$%s\"\n", name, saveVar)
		fmt.Fprintln(w, "else")
		fmt.Fprintf(w, "    set -e %s\n", name)
		fmt.Fprintln(w, "end")
		fmt.Fprintf(w, "set -e %s %s__set\n", saveVar, saveVar)
	}
	fmt.Fprintln(w)

	// Restore PATH (a list in fish).
	fmt.Fprintln(w, "# --- restore PATH ---")
	fmt.Fprintf(w, "if test \"$%sPATH__set\" = 1\n", helperPrefix)
	fmt.Fprintf(w, "    set -gx PATH $%sPATH\n", helperPrefix)
	fmt.Fprintln(w, "end")
	fmt.Fprintf(w, "set -e %sPATH %sPATH__set\n\n", helperPrefix, helperPrefix)

	// Restore in reverse activation order: functions first, then aliases.
	fmt.Fprintln(w, "# --- restore functions ---")
	for _, name := range funcNamesForShell(p, "fish") {
		guard := helperPrefix + "FN_" + sanitizeVar(name)
		definition := guard + "__definition"
		present := guard + "__set"
		fmt.Fprintf(w, "if test \"$%s\" = 1\n", guard)
		fmt.Fprintf(w, "    functions -e %s\n", fishQuote(name))
		fmt.Fprintf(w, "    if test \"$%s\" = 1\n", present)
		fmt.Fprintf(w, "        printf '%%s\\n' \"$%s\" | source\n", definition)
		fmt.Fprintln(w, "    end")
		fmt.Fprintln(w, "end")
		fmt.Fprintf(w, "set -e %s %s %s\n", guard, definition, present)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "# --- restore aliases ---")
	for _, name := range sortedKeys(p.Aliases) {
		guard := helperPrefix + "ALIAS_" + sanitizeVar(name)
		definition := guard + "__definition"
		present := guard + "__set"
		fmt.Fprintf(w, "if test \"$%s\" = 1\n", guard)
		fmt.Fprintf(w, "    functions -e %s\n", fishQuote(name))
		fmt.Fprintf(w, "    if test \"$%s\" = 1\n", present)
		fmt.Fprintf(w, "        printf '%%s\\n' \"$%s\" | source\n", definition)
		fmt.Fprintln(w, "    end")
		fmt.Fprintln(w, "end")
		fmt.Fprintf(w, "set -e %s %s %s\n", guard, definition, present)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "# --- clear Taugres variables ---")
	fmt.Fprintln(w, "set -e TAUGRES_ACTIVE TAUGRES_ROOT TAUGRES_REPO_ROOT TAUGRES_PROJECT_ROOT TAUGRES_CONFIG TAUGRES_LOCK TAUGRES_STATE")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# --- end deactivation ---")
	return b.String()
}

func fishExport(w *strings.Builder, name, value string) {
	fmt.Fprintf(w, "set -gx %s %s\n", name, fishQuote(value))
}

func fishEnv(w *strings.Builder, p *model.Plan) {
	fmt.Fprintln(w, "# --- environment ---")
	for _, name := range sortedKeys(p.EnvSet) {
		fishSaveEnv(w, name)
		fishExport(w, name, p.EnvSet[name])
	}
	for _, name := range p.EnvUnset {
		fishSaveEnv(w, name)
		fmt.Fprintf(w, "set -e %s\n", name)
	}
	fmt.Fprintln(w)
}

// fishExecEnv emits dynamic deferred env vars for fish: those with a dynamic
// shell.exec segment, run in the shell on each activation via `(cmd)`, with the
// prior value saved for restoration. The value concatenates its segments —
// single-quoted literals (static parts already baked at sync) and `(cmd)`
// substitutions — which fish joins with no separator. Fully-static deferred vars
// were baked into EnvSet.
func fishExecEnv(w *strings.Builder, p *model.Plan) {
	entries := dynamicDeferredEnv(p)
	if len(entries) == 0 {
		return
	}
	fmt.Fprintln(w, "# --- exec env (dynamic) ---")
	for _, de := range entries {
		fishSaveEnv(w, de.Name)
		var b strings.Builder
		for _, s := range de.Segments {
			switch s.Kind {
			case model.SegExec: // dynamic (static execs were baked to literals at sync)
				cmd := s.Value
				if s.Shell != "" {
					cmd = s.Shell + " -c " + fishQuote(s.Value)
				}
				fmt.Fprintf(&b, "(%s)", cmd)
			default: // literal
				b.WriteString(fishQuote(s.Value))
			}
		}
		fmt.Fprintf(w, "set -gx %s %s\n", de.Name, b.String())
	}
	fmt.Fprintln(w)
}

// fishSaveEnv records the current value (and presence) of an env var so
// deactivation can restore it.
func fishSaveEnv(w *strings.Builder, name string) {
	saveVar := helperPrefix + "ENV_" + name
	fmt.Fprintf(w, "if not set -q %s__set\n", saveVar)
	fmt.Fprintf(w, "    if set -q %s\n", name)
	fmt.Fprintf(w, "        set -g %s \"$%s\"; set -g %s__set 1\n", saveVar, name, saveVar)
	fmt.Fprintln(w, "    else")
	fmt.Fprintf(w, "        set -g %s__set 0\n", saveVar)
	fmt.Fprintln(w, "    end")
	fmt.Fprintln(w, "end")
}

func fishPath(w *strings.Builder, p *model.Plan) {
	fmt.Fprintln(w, "# --- PATH ---")
	fmt.Fprintf(w, "if not set -q %sPATH__set\n", helperPrefix)
	fmt.Fprintf(w, "    set -g %sPATH $PATH; set -g %sPATH__set 1\n", helperPrefix, helperPrefix)
	fmt.Fprintln(w, "end")
	// Prepend in reverse so the first entry ends up first on PATH.
	for i := len(p.PathPrepend) - 1; i >= 0; i-- {
		fmt.Fprintf(w, "set -gx PATH %s $PATH\n", fishQuote(p.PathPrepend[i]))
	}
	for _, dir := range p.PathAppend {
		fmt.Fprintf(w, "set -gx PATH $PATH %s\n", fishQuote(dir))
	}
	fmt.Fprintln(w)
}

func fishAliases(w *strings.Builder, p *model.Plan) {
	if len(p.Aliases) == 0 {
		return
	}
	fmt.Fprintln(w, "# --- aliases ---")
	for _, name := range sortedKeys(p.Aliases) {
		guard := helperPrefix + "ALIAS_" + sanitizeVar(name)
		definition := guard + "__definition"
		present := guard + "__set"
		qn := fishQuote(name)
		fmt.Fprintf(w, "if not set -q %s\n", guard)
		fmt.Fprintf(w, "    if functions -q %s\n", qn)
		fmt.Fprintf(w, "        set -g %s (functions %s | string collect)\n", definition, qn)
		fmt.Fprintf(w, "        set -g %s 1\n", present)
		fmt.Fprintln(w, "    else")
		fmt.Fprintf(w, "        set -g %s 0\n", present)
		fmt.Fprintln(w, "    end")
		fmt.Fprintln(w, "end")
		fmt.Fprintf(w, "functions -e %s 2>/dev/null\n", qn)
		fmt.Fprintf(w, "alias %s %s\n", qn, fishQuote(p.Aliases[name]))
		fmt.Fprintf(w, "set -g %s 1\n", guard)
	}
	fmt.Fprintln(w)
}

func fishFunctions(w *strings.Builder, p *model.Plan) {
	names := funcNamesForShell(p, "fish")
	if len(names) == 0 {
		return
	}

	fmt.Fprintln(w, "# --- functions ---")
	for _, name := range names {
		e, ok := funcEntryForShell(p, name, "fish")
		if !ok {
			continue
		}

		guard := helperPrefix + "FN_" + sanitizeVar(name)
		definition := guard + "__definition"
		present := guard + "__set"
		qn := fishQuote(name)
		fmt.Fprintf(w, "if not set -q %s\n", guard)
		fmt.Fprintf(w, "    if functions -q %s\n", qn)
		fmt.Fprintf(w, "        set -g %s (functions %s | string collect)\n", definition, qn)
		fmt.Fprintf(w, "        set -g %s 1\n", present)
		fmt.Fprintln(w, "    else")
		fmt.Fprintf(w, "        set -g %s 0\n", present)
		fmt.Fprintln(w, "    end")
		fmt.Fprintln(w, "end")
		fmt.Fprintf(w, "functions -e %s 2>/dev/null\n", qn)
		if e.File != "" {
			fmt.Fprintf(w, "function %s; source %s $argv; end\n", name, fishQuote(e.File))
		} else {
			fmt.Fprintf(w, "function %s\n%s\nend\n", name, strings.Trim(e.Content, "\n"))
		}
		fmt.Fprintf(w, "set -g %s 1\n", guard)
	}
	fmt.Fprintln(w)
}

func fishHooks(w *strings.Builder, p *model.Plan) {
	hooks := hooksForShell(p, "fish")
	if len(hooks) == 0 {
		return
	}

	fmt.Fprintln(w, "# --- hooks ---")
	for _, h := range hooks {
		if h.File != "" {
			fmt.Fprintf(w, "source %s\n", fishQuote(h.File))
		} else {
			fmt.Fprintln(w, strings.Trim(h.Content, "\n"))
		}
	}
	fmt.Fprintln(w)
}

func fishNotice(w *strings.Builder, p *model.Plan) {
	name := p.ProjectName
	if name == "" {
		name = filepath.Base(p.ProjectRoot)
	}

	q := fishQuote(name)
	e := fishQuote(activationEmoji)
	fmt.Fprintln(w, "# --- notice ---")
	fmt.Fprintln(w, "if isatty stderr")
	fmt.Fprintf(w, "    printf '\\033[32m%%s\\033[0m \\033[2mtau activated\\033[0m \\033[1m%%s\\033[0m\\n' %s %s >&2\n", e, q)
	fmt.Fprintln(w, "else")
	fmt.Fprintf(w, "    printf '%%s tau activated %%s\\n' %s %s >&2\n", e, q)
	fmt.Fprintln(w, "end")
	fmt.Fprintln(w)
}

// fishQuote returns a fish single-quoted literal. In fish single quotes, only
// backslash and single-quote need escaping.
func fishQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}
