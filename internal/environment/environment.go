// Package environment assembles the shell-agnostic process environment shared
// by tau exec and future editor/CI integrations.
package environment

import (
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edganiukov/taugres/internal/model"
)

// Build returns the ambient environment with the plan's env changes, Taugres
// built-ins, and resolved manager/user PATH entries applied.
func Build(plan *model.Plan, managerBinDirs []string) map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if key, value, ok := strings.Cut(kv, "="); ok {
			env[key] = value
		}
	}
	maps.Copy(env, plan.EnvSet)
	for _, key := range plan.EnvUnset {
		delete(env, key)
	}
	env["TAUGRES_ACTIVE"] = "1"
	env["TAUGRES_ROOT"] = plan.RepoRoot
	env["TAUGRES_REPO_ROOT"] = plan.RepoRoot
	env["TAUGRES_PROJECT_ROOT"] = plan.ProjectRoot
	env["TAUGRES_CONFIG"] = plan.ConfigPath
	env["TAUGRES_LOCK"] = filepath.Join(plan.ProjectRoot, ".taugres.lock")
	env["TAUGRES_STATE"] = plan.StateDir

	separator := string(os.PathListSeparator)
	path := env["PATH"]
	for _, dir := range plan.PathAppend {
		path = joinPath(path, dir, separator, false)
	}
	prepend := dedup(append(append([]string{}, managerBinDirs...), plan.PathPrepend...))
	for i := len(prepend) - 1; i >= 0; i-- {
		path = joinPath(path, prepend[i], separator, true)
	}
	env["PATH"] = path
	return env
}

func joinPath(path, dir, separator string, prepend bool) string {
	if path == "" {
		return dir
	}
	if prepend {
		return dir + separator + path
	}
	return path + separator + dir
}

func dedup(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// Flatten renders env as a deterministic KEY=VALUE slice for exec.Cmd.
func Flatten(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}
