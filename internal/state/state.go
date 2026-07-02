// Package state manages Taugres per-project metadata under
// <projectRoot>/.taugres/gen (generated shell scripts, manifest, and
// markers) and provides cheap stale detection.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edganiukov/taugres/internal/model"
)

// GenDirName is the subdirectory holding tau's per-project metadata:
// generated activation scripts, the manifest, and sync/trust markers.
const GenDirName = "gen"

// Manifest records what was generated and the hashes needed for stale checks.
type Manifest struct {
	TauVersion   string            `json:"tauVersion"`
	ConfigPath   string            `json:"configPath"`
	RepoRoot     string            `json:"repoRoot"`
	ProjectRoot  string            `json:"projectRoot"`
	ConfigHash   string            `json:"configHash"`
	ModuleHashes map[string]string `json:"moduleHashes"`
	SourceHashes map[string]string `json:"sourceHashes"`
	Shells       []string          `json:"shells"`
	MiseTools    map[string]string `json:"miseTools,omitempty"`
	PipPackages  map[string]string `json:"pipPackages,omitempty"`
	NpmPackages  map[string]string `json:"npmPackages,omitempty"`
}

// GenDir returns the generated directory for a project state dir
// (<projectRoot>/.taugres/gen).
func GenDir(stateDir string) string {
	return filepath.Join(stateDir, GenDirName)
}

// ManifestPath returns the manifest path within a state dir.
func ManifestPath(stateDir string) string {
	return filepath.Join(GenDir(stateDir), "manifest.json")
}

// SourcesPath returns the file listing the config inputs (active config file,
// loaded modules, fn.source files), one absolute path per line. The hook and
// staleness checks treat any source newer than the manifest as a reason to
// resync, so editing a loaded module or sourced function file is picked up.
func SourcesPath(stateDir string) string {
	return filepath.Join(GenDir(stateDir), "sources")
}

// WriteSources records the config input files for later staleness checks.
func WriteSources(stateDir string, files []string) error {
	return writeLines(SourcesPath(stateDir), files)
}

// ToolDirsPath returns the file listing the tau-managed tool bin directories
// (one absolute path per line) that must exist for activation to work. The hook
// and staleness checks treat a missing entry as a reason to resync, so deleting
// e.g. .taugres/tools/pip triggers a rebuild.
func ToolDirsPath(stateDir string) string {
	return filepath.Join(GenDir(stateDir), "tooldirs")
}

// writeLines writes one entry per line to path, creating the gen dir.
func writeLines(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// readLines returns the non-empty trimmed lines of a file, or nil.
func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WriteToolDirs records the tool bin directories for later existence checks.
func WriteToolDirs(stateDir string, dirs []string) error {
	return writeLines(ToolDirsPath(stateDir), dirs)
}

// missingToolDir returns the first recorded tool bin dir that no longer exists,
// or "" if all are present.
func missingToolDir(stateDir string) string {
	return missingDir(readLines(ToolDirsPath(stateDir)))
}

func missingDir(dirs []string) string {
	for _, d := range dirs {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			return d
		}
	}
	return ""
}

// ProbesPath returns the file recording exists()/which() observations, one
// "kind|arg|result" per line. The hook and staleness checks re-evaluate each
// probe and treat a changed result (e.g. a probed file appearing, or a binary
// getting installed) as a reason to resync.
func ProbesPath(genDir string) string {
	return filepath.Join(genDir, "probes")
}

// WriteProbes records host-state observations for later stale detection. When
// there are none it removes any previous file, so the hot path skips the probe
// loop entirely (a single `[ -f ]` test) for configs that use no probes.
func WriteProbes(stateDir string, probes []model.Probe) error {
	path := ProbesPath(GenDir(stateDir))
	if len(probes) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	lines := make([]string, 0, len(probes))
	for _, p := range probes {
		lines = append(lines, p.Kind+"|"+p.Arg+"|"+p.Result)
	}
	return writeLines(path, lines)
}

// probeDrifted reports whether a recorded probe's result differs from the
// current world (a probed path appeared/vanished, or a binary was
// installed/removed/moved). It mirrors the shell hook's cheap re-evaluation.
func probeDrifted(kind, arg, recorded string) bool {
	switch kind {
	case "exists":
		_, err := os.Stat(arg)
		return boolResult(err == nil) != recorded
	case "which":
		path, err := exec.LookPath(arg)
		if err != nil {
			path = ""
		}
		return path != recorded
	}
	return false
}

func boolResult(ok bool) string {
	if ok {
		return "1"
	}
	return "0"
}

// probesChanged reports whether any recorded probe under genDir has drifted.
func probesChanged(genDir string) bool {
	for _, line := range readLines(ProbesPath(genDir)) {
		kind, rest, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		arg, recorded, _ := strings.Cut(rest, "|")
		if probeDrifted(kind, arg, recorded) {
			return true
		}
	}
	return false
}

// Dialect selects the shell syntax for a check's hook fragments.
type Dialect int

const (
	Posix Dialect = iota // bash/zsh
	Fish
)

// A Check is one independent dimension of staleness. Each check persists some
// state under gen/ at sync time (see the Write* functions) and later reports
// whether the world has drifted from it. Checks are evaluated concurrently by
// NeedsSync and are also reflected in the generated shell hook via their Detect
// (runs every prompt, builtins only) and Token (runs only on the stale path,
// may stat) fragments.
//
// To add a new kind of freshness check, append a checkImpl to the checks slice:
// both the Go path and the shell hook pick it up automatically.
type checkImpl struct {
	// stale evaluates drift in Go. genDir is <projectRoot>/.taugres/gen;
	// configPath and manifestMod support mtime comparisons.
	stale func(genDir, configPath string, manifestMod time.Time) bool
	// detect/token are pure-shell fragments keyed by dialect (see the const
	// blocks below). detect sets `stale=1`; token appends to `_tau_tok`.
	detect map[Dialect]string
	token  map[Dialect]string
}

var checks = []checkImpl{
	{
		stale: func(genDir, configPath string, mod time.Time) bool {
			sources := readLines(SourcesPath(genDirState(genDir)))
			if len(sources) == 0 {
				sources = []string{configPath}
			}
			for _, s := range sources {
				if si, err := os.Stat(s); err == nil && si.ModTime().After(mod) {
					return true
				}
			}
			return false
		},
		detect: map[Dialect]string{Posix: posixDetectSources, Fish: fishDetectSources},
		token:  map[Dialect]string{Posix: posixTokenSources, Fish: fishTokenSources},
	},
	{
		stale: func(genDir, _ string, _ time.Time) bool {
			return missingDir(readLines(filepath.Join(genDir, "tooldirs"))) != ""
		},
		detect: map[Dialect]string{Posix: posixDetectToolDirs, Fish: fishDetectToolDirs},
		token:  map[Dialect]string{Posix: "", Fish: ""},
	},
	{
		stale: func(genDir, _ string, _ time.Time) bool {
			return probesChanged(genDir)
		},
		detect: map[Dialect]string{Posix: posixDetectProbes, Fish: fishDetectProbes},
		token:  map[Dialect]string{Posix: posixTokenProbes, Fish: fishTokenProbes},
	},
}

// genDirState recovers the state dir from a gen dir (gen dir is
// <stateDir>/gen), so a check given genDir can call the *Path(stateDir) helpers.
func genDirState(genDir string) string {
	return filepath.Dir(genDir)
}

// ShellDetect returns the concatenated per-check detect fragments for a dialect
// (spliced into the generated hook; run on every prompt).
func ShellDetect(d Dialect) string {
	var b strings.Builder
	for _, c := range checks {
		b.WriteString(c.detect[d])
	}
	return b.String()
}

// ShellToken returns the concatenated per-check token fragments for a dialect
// (spliced into the generated hook; run only when already stale).
func ShellToken(d Dialect) string {
	var b strings.Builder
	for _, c := range checks {
		b.WriteString(c.token[d])
	}
	return b.String()
}

// NeedsSync reports whether a sync is needed. It mirrors the shell hook's cheap
// check so the two never disagree: sync is needed when there is no completed
// sync (manifest absent) or any registered check reports drift — a config input
// newer than the manifest, a removed tool directory, or a changed exists()/
// which() probe. The checks run concurrently. The manifest is written only at
// the end of a successful sync, so a failed/partial sync never suppresses a
// retry.
func NeedsSync(stateDir, configPath string) (bool, error) {
	mi, err := os.Stat(ManifestPath(stateDir))
	if err != nil {
		return true, nil // no completed sync yet
	}
	genDir := GenDir(stateDir)
	mod := mi.ModTime()

	results := make([]bool, len(checks))
	var wg sync.WaitGroup
	for i := range checks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = checks[i].stale(genDir, configPath, mod)
		}(i)
	}
	wg.Wait()
	for _, stale := range results {
		if stale {
			return true, nil
		}
	}
	return false, nil
}

// HashFile returns the hex sha256 of a file's contents.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Write writes the manifest to disk (creating the generated dir if needed).
func (m *Manifest) Write(stateDir string) error {
	if err := os.MkdirAll(GenDir(stateDir), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(ManifestPath(stateDir), data, 0o644)
}

// Load reads a manifest from a state dir. Returns os.ErrNotExist if missing.
func Load(stateDir string) (*Manifest, error) {
	data, err := os.ReadFile(ManifestPath(stateDir))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	return &m, nil
}

// StaleReason describes why generated scripts are considered stale.
type StaleReason struct {
	Stale  bool
	Reason string
}

// CheckStale performs a cheap staleness check for the given state dir and the
// shells expected to be present. It re-hashes the config and tracked files.
//
// This is intended for `tau status`/`tau check`, not the hot activation path.
func CheckStale(stateDir string, expectedShells []string) StaleReason {
	m, err := Load(stateDir)
	if err != nil {
		return StaleReason{Stale: true, Reason: "no generated manifest; run `tau sync`"}
	}

	// Config hash.
	got, err := HashFile(m.ConfigPath)
	if err != nil {
		return StaleReason{Stale: true, Reason: "active config is unreadable; run `tau sync`"}
	}
	if got != m.ConfigHash {
		return StaleReason{Stale: true, Reason: "config changed since last sync; run `tau sync`"}
	}

	// Module hashes.
	for path, want := range m.ModuleHashes {
		got, err := HashFile(path)
		if err != nil || got != want {
			return StaleReason{Stale: true, Reason: "a loaded module changed; run `tau sync`"}
		}
	}

	// Sourced function file hashes.
	for path, want := range m.SourceHashes {
		got, err := HashFile(path)
		if err != nil || got != want {
			return StaleReason{Stale: true, Reason: "a sourced function file changed; run `tau sync`"}
		}
	}

	// Ensure expected generated scripts exist.
	for _, sh := range expectedShells {
		for _, kind := range []string{"activate", "deactivate"} {
			p := filepath.Join(GenDir(stateDir), kind+"."+sh)
			if _, err := os.Stat(p); err != nil {
				return StaleReason{Stale: true, Reason: "generated scripts are missing; run `tau sync`"}
			}
		}
	}

	// Ensure tool directories (mise store bins, pip venv, npm prefix) still exist.
	if d := missingToolDir(stateDir); d != "" {
		return StaleReason{Stale: true, Reason: "tool directory " + d + " is missing; run `tau sync`"}
	}

	// exists()/which() probes: a probed file/binary changed state since sync.
	if probesChanged(GenDir(stateDir)) {
		return StaleReason{Stale: true, Reason: "a probed file or binary (exists/which) changed; run `tau sync`"}
	}

	return StaleReason{Stale: false}
}

// SortedShells returns a sorted copy for deterministic manifests.
func SortedShells(shells []string) []string {
	out := append([]string{}, shells...)
	sort.Strings(out)
	return out
}

// --- shell hook fragments ---
//
// These are spliced into the generated hook (see internal/shellhook). They are
// written against the hook skeleton's contract: `detect` fragments may read
// $gen_dir/$manifest and set `stale`/`present`/`probesig` using only builtins
// (they run on every prompt); `token` fragments run only on the stale path and
// append to `_tau_tok` (they may call the stat-based helpers). Each check owns
// its fragments here, next to its Go logic, so a new check is a single addition.

// posix (bash/zsh) fragments.

const posixDetectSources = `  if [ -z "$stale" ] && [ -f "$gen_dir/sources" ]; then
    while IFS= read -r f; do
      [ -n "$f" ] && [ "$f" -nt "$manifest" ] && { stale=1; break; }
    done < "$gen_dir/sources"
  fi
`

const posixDetectToolDirs = `  if [ -f "$gen_dir/tooldirs" ]; then
    while IFS= read -r f; do
      [ -n "$f" ] && [ ! -d "$f" ] && { stale=1; present=0; break; }
    done < "$gen_dir/tooldirs"
  fi
`

// posixDetectProbes always runs (even when already stale) so $probesig is
// complete for the retry token. It uses only builtins: [ -e ] and command -v
// (no command substitution -> no fork).
const posixDetectProbes = `  if [ -f "$gen_dir/probes" ]; then
    local _pk _pa _pw _pn _pe
    while IFS='|' read -r _pk _pa _pw; do
      [ -n "$_pk" ] || continue
      case "$_pk" in
        exists) if [ -e "$_pa" ]; then _pn=1; else _pn=0; fi; _pe="$_pw" ;;
        which) if command -v "$_pa" >/dev/null 2>&1; then _pn=1; else _pn=0; fi; if [ -n "$_pw" ]; then _pe=1; else _pe=0; fi ;;
        *) _pn=0; _pe=0 ;;
      esac
      probesig="$probesig|$_pn"
      [ "$_pn" = "$_pe" ] || stale=1
    done < "$gen_dir/probes"
  fi
`

const posixTokenSources = `    local newest m
    newest="$(_tau_mtime "$(_tau_config_file "$proj")")"
    [ -n "$newest" ] || newest=0
    if [ -f "$gen_dir/sources" ]; then
      while IFS= read -r f; do
        [ -n "$f" ] || continue
        m="$(_tau_mtime "$f")"
        [ -n "$m" ] && [ "$m" -gt "$newest" ] && newest="$m"
      done < "$gen_dir/sources"
    fi
    _tau_tok="$_tau_tok|$newest"
`

const posixTokenProbes = `    _tau_tok="$_tau_tok|$probesig"
`

// fish fragments.

const fishDetectSources = `    if test -z "$stale"; and test -f "$gen_dir/sources"
        for f in (cat "$gen_dir/sources")
            if test -n "$f"; and test "$f" -nt "$manifest"
                set stale 1
                break
            end
        end
    end
`

const fishDetectToolDirs = `    if test -f "$gen_dir/tooldirs"
        for d in (cat "$gen_dir/tooldirs")
            if test -n "$d"; and test ! -d "$d"
                set stale 1; set present 0
                break
            end
        end
    end
`

const fishDetectProbes = `    if test -f "$gen_dir/probes"
        for line in (cat "$gen_dir/probes")
            test -n "$line"; or continue
            set -l parts (string split '|' -- $line)
            set -l _pk $parts[1]
            set -l _pa $parts[2]
            set -l _pw ""
            test (count $parts) -ge 3; and set _pw $parts[3]
            set -l _pn 0
            set -l _pe 0
            switch "$_pk"
                case exists
                    test -e "$_pa"; and set _pn 1
                    test "$_pw" = 1; and set _pe 1
                case which
                    command -s "$_pa" >/dev/null 2>&1; and set _pn 1
                    test -n "$_pw"; and set _pe 1
            end
            set probesig "$probesig|$_pn"
            test "$_pn" = "$_pe"; or set stale 1
        end
    end
`

const fishTokenSources = `        set -l newest (_tau_mtime (_tau_config_file "$proj"))
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
        set _tau_tok "$_tau_tok|$newest"
`

const fishTokenProbes = `        set _tau_tok "$_tau_tok|$probesig"
`
