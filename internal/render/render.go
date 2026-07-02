// Package render produces shell-specific activation and deactivation scripts
// from a normalized plan. Supported shells: bash, zsh, and fish.
//
// bash/zsh scripts are POSIX-sh compatible; fish uses its own syntax.
// Deactivation restores prior environment/PATH values that activation saved
// into helper variables with a unique prefix.
package render

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/edganiukov/taugres/internal/model"
)

// helperPrefix namespaces the shell variables used to save prior state.
const helperPrefix = "__TAU_SAVE_"

// SupportedShells lists shells the renderer can produce.
var SupportedShells = []string{"bash", "zsh", "fish"}

// Activate renders the activation script for the given shell.
func Activate(p *model.Plan, shell string) (string, error) {
	switch shell {
	case "bash", "zsh":
		return posixActivate(p, shell), nil
	case "fish":
		return fishActivate(p), nil
	default:
		return "", fmt.Errorf("unsupported shell for rendering: %q", shell)
	}
}

// Deactivate renders the deactivation script for the given shell.
func Deactivate(p *model.Plan, shell string) (string, error) {
	switch shell {
	case "bash", "zsh":
		return posixDeactivate(p, shell), nil
	case "fish":
		return fishDeactivate(p), nil
	default:
		return "", fmt.Errorf("unsupported shell for rendering: %q", shell)
	}
}

// posixActivate renders the bash/zsh activation script.
func posixActivate(p *model.Plan, shell string) string {
	var b strings.Builder
	w := &b

	header(w, shell, "activate")

	// Taugres built-in variables.
	fmt.Fprintln(w, "# --- Taugres built-in variables ---")
	setExport(w, "TAUGRES_ACTIVE", "1")
	setExport(w, "TAUGRES_ROOT", p.RepoRoot)
	setExport(w, "TAUGRES_REPO_ROOT", p.RepoRoot)
	setExport(w, "TAUGRES_PROJECT_ROOT", p.ProjectRoot)
	setExport(w, "TAUGRES_CONFIG", p.ConfigPath)
	setExport(w, "TAUGRES_LOCK", p.ProjectRoot+"/.taugres.lock")
	setExport(w, "TAUGRES_STATE", p.StateDir)
	fmt.Fprintln(w)

	// Environment set/unset with save-for-restore.
	renderEnv(w, p)

	// PATH.
	renderPath(w, p)

	// Aliases (conservative: skip if already defined).
	renderAliases(w, p)

	// Sourced functions.
	renderFunctions(w, p, shell)

	// Raw activation hooks, run after everything else is set up.
	renderHooks(w, p, shell)

	// Friendly notice, shown once when the project is entered.
	renderNotice(w, p)

	fmt.Fprintln(w, "# --- end activation ---")
	return b.String()
}

// activationEmoji is a small ASCII cat shown on activation. It is passed as a
// printf argument (not part of the format) so its characters are printed
// literally on every shell.
const activationEmoji = `=^..^=`

// renderNotice prints a short activation message to stderr, with a little color
// when stderr is a terminal.
func renderNotice(w *strings.Builder, p *model.Plan) {
	name := p.ProjectName
	if name == "" {
		name = filepath.Base(p.ProjectRoot)
	}
	q := shellQuote(name)
	e := shellQuote(activationEmoji)
	fmt.Fprintln(w, "# --- notice ---")
	fmt.Fprintln(w, "if [ -t 2 ]; then")
	// green emoji, dim "tau activated", bold name.
	fmt.Fprintf(w, "  printf '\\033[32m%%s\\033[0m \\033[2mtau activated\\033[0m \\033[1m%%s\\033[0m\\n' %s %s >&2\n", e, q)
	fmt.Fprintln(w, "else")
	fmt.Fprintf(w, "  printf '%%s tau activated %%s\\n' %s %s >&2\n", e, q)
	fmt.Fprintln(w, "fi")
	fmt.Fprintln(w)
}

// posixDeactivate renders the bash/zsh deactivation script.
func posixDeactivate(p *model.Plan, shell string) string {
	var b strings.Builder
	w := &b

	header(w, shell, "deactivate")

	// Restore environment variables.
	fmt.Fprintln(w, "# --- restore environment ---")
	for _, name := range envRestoreOrder(p) {
		saveVar := helperPrefix + "ENV_" + name
		presentVar := saveVar + "__set"
		fmt.Fprintf(w, "if [ \"${%s:-}\" = 1 ]; then\n", presentVar)
		fmt.Fprintf(w, "  export %s=\"${%s}\"\n", name, saveVar)
		fmt.Fprintln(w, "else")
		fmt.Fprintf(w, "  unset %s\n", name)
		fmt.Fprintln(w, "fi")
		fmt.Fprintf(w, "unset %s %s\n", saveVar, presentVar)
	}
	fmt.Fprintln(w)

	// Restore PATH.
	fmt.Fprintln(w, "# --- restore PATH ---")
	fmt.Fprintf(w, "if [ \"${%sPATH__set:-}\" = 1 ]; then\n", helperPrefix)
	fmt.Fprintf(w, "  export PATH=\"${%sPATH}\"\n", helperPrefix)
	fmt.Fprintln(w, "fi")
	fmt.Fprintf(w, "unset %sPATH %sPATH__set\n\n", helperPrefix, helperPrefix)

	// Remove aliases that Taugres created (only those it actually set).
	fmt.Fprintln(w, "# --- remove aliases ---")
	for _, name := range sortedKeys(p.Aliases) {
		guard := helperPrefix + "ALIAS_" + sanitizeVar(name)
		fmt.Fprintf(w, "if [ \"${%s:-}\" = 1 ]; then unalias %s 2>/dev/null; fi\n", guard, shellQuoteBare(name))
		fmt.Fprintf(w, "unset %s\n", guard)
	}
	fmt.Fprintln(w)

	// Remove functions that Taugres created.
	fmt.Fprintln(w, "# --- remove functions ---")
	for _, name := range funcNamesForShell(p, shell) {
		guard := helperPrefix + "FN_" + sanitizeVar(name)
		fmt.Fprintf(w, "if [ \"${%s:-}\" = 1 ]; then unset -f %s 2>/dev/null; fi\n", guard, shellQuoteBare(name))
		fmt.Fprintf(w, "unset %s\n", guard)
	}
	fmt.Fprintln(w)

	// Clear Taugres built-in variables.
	fmt.Fprintln(w, "# --- clear Taugres variables ---")
	fmt.Fprintln(w, "unset TAUGRES_ACTIVE TAUGRES_ROOT TAUGRES_REPO_ROOT TAUGRES_PROJECT_ROOT TAUGRES_CONFIG TAUGRES_LOCK TAUGRES_STATE")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# --- end deactivation ---")
	return b.String()
}

func header(w *strings.Builder, shell, kind string) {
	fmt.Fprintf(w, "# Generated by tau. Do not edit.\n")
	fmt.Fprintf(w, "# shell=%s kind=%s\n\n", shell, kind)
}

func renderEnv(w *strings.Builder, p *model.Plan) {
	fmt.Fprintln(w, "# --- environment ---")
	// Set vars (sorted for determinism).
	for _, name := range sortedKeys(p.EnvSet) {
		saveEnv(w, name)
		setExport(w, name, p.EnvSet[name])
	}
	// Unset vars.
	for _, name := range p.EnvUnset {
		saveEnv(w, name)
		fmt.Fprintf(w, "unset %s\n", name)
	}
	fmt.Fprintln(w)
}

// saveEnv records the current value (and presence) of an env var so
// deactivation can restore it.
func saveEnv(w *strings.Builder, name string) {
	saveVar := helperPrefix + "ENV_" + name
	presentVar := saveVar + "__set"
	fmt.Fprintf(w, "if [ -z \"${%s+x}\" ]; then\n", presentVar)
	fmt.Fprintf(w, "  if [ -n \"${%s+x}\" ]; then %s=\"${%s}\"; %s=1; else %s=0; fi\n",
		name, saveVar, name, presentVar, presentVar)
	fmt.Fprintln(w, "fi")
}

func renderPath(w *strings.Builder, p *model.Plan) {
	fmt.Fprintln(w, "# --- PATH ---")
	// Save PATH once.
	fmt.Fprintf(w, "if [ -z \"${%sPATH__set+x}\" ]; then %sPATH=\"${PATH}\"; %sPATH__set=1; fi\n",
		helperPrefix, helperPrefix, helperPrefix)
	for i := len(p.PathPrepend) - 1; i >= 0; i-- {
		fmt.Fprintf(w, "export PATH=%s:\"${PATH}\"\n", shellQuote(p.PathPrepend[i]))
	}
	for _, dir := range p.PathAppend {
		fmt.Fprintf(w, "export PATH=\"${PATH}\":%s\n", shellQuote(dir))
	}
	fmt.Fprintln(w)
}

func renderAliases(w *strings.Builder, p *model.Plan) {
	if len(p.Aliases) == 0 {
		return
	}
	fmt.Fprintln(w, "# --- aliases ---")
	for _, name := range sortedKeys(p.Aliases) {
		guard := helperPrefix + "ALIAS_" + sanitizeVar(name)
		qn := shellQuoteBare(name)
		// The project's aliases win: always (re)define them, shadowing any
		// existing alias or command for the duration of activation. Deactivation
		// removes them again (unalias restores a shadowed command). Re-defining is
		// also what lets reactivation refresh a stale definition.
		fmt.Fprintf(w, "alias %s=%s\n", qn, shellQuote(p.Aliases[name]))
		fmt.Fprintf(w, "%s=1\n", guard)
	}
	fmt.Fprintln(w)
}

func renderFunctions(w *strings.Builder, p *model.Plan, shell string) {
	names := funcNamesForShell(p, shell)
	if len(names) == 0 {
		return
	}
	fmt.Fprintln(w, "# --- functions ---")
	for _, name := range names {
		e, ok := funcEntryForShell(p, name, shell)
		if !ok {
			continue
		}
		guard := helperPrefix + "FN_" + sanitizeVar(name)
		// The project's functions win: always (re)define them (shadowing any
		// existing command), so reactivation refreshes a stale definition.
		// Deactivation removes them again.
		if e.File != "" {
			// Body lives in a file, sourced with the caller's arguments.
			fmt.Fprintf(w, "%s() { source %s \"$@\"; }\n", name, shellQuote(e.File))
		} else {
			// Inline body embedded verbatim (shell ignores indentation), with
			// surrounding blank lines trimmed.
			fmt.Fprintf(w, "%s() {\n%s\n}\n", name, strings.Trim(e.Content, "\n"))
		}
		fmt.Fprintf(w, "%s=1\n", guard)
	}
	fmt.Fprintln(w)
}

// renderHooks emits raw activation snippets (bash/zsh) for the given shell, in
// declaration order, verbatim.
func renderHooks(w *strings.Builder, p *model.Plan, shell string) {
	hooks := hooksForShell(p, shell)
	if len(hooks) == 0 {
		return
	}

	fmt.Fprintln(w, "# --- hooks ---")
	for _, h := range hooks {
		if h.File != "" {
			fmt.Fprintf(w, "source %s\n", shellQuote(h.File))
		} else {
			fmt.Fprintln(w, strings.Trim(h.Content, "\n"))
		}
	}

	fmt.Fprintln(w)
}

// hooksForShell returns the hooks targeting the given shell, in declaration order.
func hooksForShell(p *model.Plan, shell string) []model.HookScript {
	var out []model.HookScript
	for _, h := range p.Hooks {
		if slices.Contains(h.Shells, shell) {
			out = append(out, h)
		}
	}
	return out
}

// --- helpers ---
func setExport(w *strings.Builder, name, value string) {
	fmt.Fprintf(w, "export %s=%s\n", name, shellQuote(value))
}

// funcNamesForShell returns function names that target the given shell, sorted.
func funcNamesForShell(p *model.Plan, shell string) []string {
	var out []string
	for name, entries := range p.SourceFuncs {
		for _, e := range entries {
			if slices.Contains(e.Shells, shell) {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func funcEntryForShell(p *model.Plan, name, shell string) (model.SourceFunc, bool) {
	for _, e := range p.SourceFuncs[name] {
		if slices.Contains(e.Shells, shell) {
			return e, true
		}
	}
	return model.SourceFunc{}, false
}

// envRestoreOrder returns the set of env var names that activation saved
// (union of set and unset), sorted and de-duplicated.
func envRestoreOrder(p *model.Plan) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range sortedKeys(p.EnvSet) {
		add(n)
	}
	for _, n := range p.EnvUnset {
		add(n)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)
	return out
}

// shellQuote returns a single-quoted shell literal safe for bash/zsh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellQuoteBare quotes a name for use as a command word (alias/function name).
// Names are validated to a safe charset, so this is mostly a passthrough.
func shellQuoteBare(s string) string {
	return shellQuote(s)
}

// sanitizeVar turns an arbitrary name into a valid shell variable suffix.
func sanitizeVar(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}

	return b.String()
}
