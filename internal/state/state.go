// Package state manages Taugres per-project metadata under
// <projectRoot>/.taugres/gen (the generated shell scripts and the manifest) and
// provides cheap stale detection.
package state

import (
	"crypto/sha256"
	"encoding/hex"
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

// GenDirName is the subdirectory holding tau's per-project metadata: the
// generated activation scripts and the manifest.
const GenDirName = "gen"

// ManifestName is the single per-project state file. It records everything the
// staleness checks need — one tagged line per entry — so the shell hook can
// read it with pure builtins (no JSON parsing, no subprocess) while Go reads the
// same file. It is written last in a sync, so its own mtime is the "last
// synced" anchor the mtime checks compare against.
//
// Each line is `tag:payload`, greppable by tag:
//
//	input:<sha256>:<abs-path>     a config input (config file, loaded module, fn/hook source file)
//	tooldir:<abs-path>            a tool bin dir that must exist
//	probe:<kind>|<arg>|<result>   an exists()/which() observation
//	toolsig:<mgr>:<sha256>        per-manager fingerprint of its tools + locked versions
//
// The shell hook only reads input/tooldir/probe lines and ignores the rest, so
// toolsig is Go-only and safe to add without touching the hook.
const ManifestName = "manifest"

// Manifest is the parsed state file.
type Manifest struct {
	// Inputs maps each config-input file (the config, loaded modules, and
	// fn/hook source files) to its content hash. A newer mtime (the cheap hook
	// check) or a changed hash (the thorough `tau status` check) is stale.
	Inputs map[string]string
	// ToolDirs are bin dirs that must exist on disk; a missing one is stale. It
	// holds the mise store bin dirs followed by each package manager's prefix bin;
	// a shell-only resync recovers the mise subset from here (the package dirs are
	// derivable from the plan), so no dir is recorded twice.
	ToolDirs []string
	// Probes are recorded exists()/which() results; a changed result is stale.
	Probes []model.Probe
	// ToolSig maps each tool manager (mise/pip/npm/uv) to a fingerprint of its
	// declared tools/packages joined with their locked versions. When a manager's
	// signature is unchanged (and its bin dirs exist) its install is skipped; when
	// every manager is fresh, the whole install phase is skipped and only the
	// shell scripts are regenerated. Managers with nothing declared are absent.
	ToolSig map[string]string
}

// GenDir returns the generated directory for a project state dir
// (<projectRoot>/.taugres/gen).
func GenDir(stateDir string) string { return filepath.Join(stateDir, GenDirName) }

// ManifestPath returns the manifest path within a state dir.
func ManifestPath(stateDir string) string {
	return filepath.Join(GenDir(stateDir), ManifestName)
}

// Write serializes the manifest to its tagged line format, creating the gen dir.
// Inputs are sorted for a stable file.
func (m *Manifest) Write(stateDir string) error {
	if err := os.MkdirAll(GenDir(stateDir), 0o755); err != nil {
		return err
	}
	paths := make([]string, 0, len(m.Inputs))
	for p := range m.Inputs {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	for _, p := range paths {
		b.WriteString("input:")
		b.WriteString(m.Inputs[p])
		b.WriteByte(':')
		b.WriteString(p)
		b.WriteByte('\n')
	}
	for _, d := range m.ToolDirs {
		b.WriteString("tooldir:")
		b.WriteString(d)
		b.WriteByte('\n')
	}
	for _, p := range m.Probes {
		b.WriteString("probe:")
		b.WriteString(p.Kind)
		b.WriteByte('|')
		b.WriteString(p.Arg)
		b.WriteByte('|')
		b.WriteString(p.Result)
		b.WriteByte('\n')
	}
	mgrs := make([]string, 0, len(m.ToolSig))
	for mgr := range m.ToolSig {
		mgrs = append(mgrs, mgr)
	}
	sort.Strings(mgrs)
	for _, mgr := range mgrs {
		b.WriteString("toolsig:")
		b.WriteString(mgr)
		b.WriteByte(':')
		b.WriteString(m.ToolSig[mgr])
		b.WriteByte('\n')
	}
	return os.WriteFile(ManifestPath(stateDir), []byte(b.String()), 0o644)
}

// Load parses the manifest, or returns the os error (e.g. os.ErrNotExist) if it
// cannot be read.
func Load(stateDir string) (*Manifest, error) {
	data, err := os.ReadFile(ManifestPath(stateDir))
	if err != nil {
		return nil, err
	}
	m := &Manifest{Inputs: map[string]string{}, ToolSig: map[string]string{}}
	for line := range strings.SplitSeq(string(data), "\n") {
		tag, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch tag {
		case "input":
			if hash, path, ok := strings.Cut(rest, ":"); ok {
				m.Inputs[path] = hash
			}
		case "tooldir":
			m.ToolDirs = append(m.ToolDirs, rest)
		case "toolsig":
			if mgr, hash, ok := strings.Cut(rest, ":"); ok {
				m.ToolSig[mgr] = hash
			}
		case "probe":
			// kind|arg|result; arg (a path) may contain '|', so take kind from
			// the front and result from the back.
			kind, r, ok := strings.Cut(rest, "|")
			i := strings.LastIndexByte(r, '|')
			if !ok || i < 0 {
				continue
			}
			m.Probes = append(m.Probes, model.Probe{Kind: kind, Arg: r[:i], Result: r[i+1:]})
		}
	}
	return m, nil
}

// TouchManifest re-anchors the manifest's mtime to now. The cheap mtime
// staleness check treats any input newer than the manifest as stale, so after
// confirming (by hash) that a mtime-only change was a no-op, bumping the
// manifest mtime stops the shell hook from re-triggering on every prompt.
func TouchManifest(stateDir string) error {
	now := time.Now()
	return os.Chtimes(ManifestPath(stateDir), now, now)
}

// --- auto-sync retry guard ---

// TriedName is the retry-guard file (gen/tried). A failed or refused auto-sync
// records the attempt here; the shell hook then re-runs `tau sync --if-stale`
// only when an input is newer than this file or the recorded state token
// changed. It is shared by all shells and cleared on a successful sync (and by
// `tau allow`, which invalidates a refused attempt).
//
// Content is one line — <present><probesig> — mirroring exactly what the hook
// computes with builtins: present is "1"/"0" for "activate scripts, manifest,
// and every tool dir exist", and probesig is "|<0/1>" per recorded probe, in
// manifest order.
const TriedName = "tried"

// TriedPath returns the retry-guard path within a state dir.
func TriedPath(stateDir string) string {
	return filepath.Join(GenDir(stateDir), TriedName)
}

// WriteTried records a failed/refused auto-sync attempt for the hook's retry
// guard, computing the state token from the last-synced manifest.
func WriteTried(stateDir string, shells []string) error {
	present := "1"
	var sig strings.Builder
	m, err := Load(stateDir)
	if err != nil {
		present = "0"
	} else {
		for _, sh := range shells {
			if _, err := os.Stat(filepath.Join(GenDir(stateDir), "activate."+sh)); err != nil {
				present = "0"
				break
			}
		}
		if missingDir(m.ToolDirs) != "" {
			present = "0"
		}
		for _, p := range m.Probes {
			sig.WriteString("|")
			sig.WriteString(boolResult(probeNow(p.Kind, p.Arg)))
		}
	}
	if err := os.MkdirAll(GenDir(stateDir), 0o755); err != nil {
		return err
	}
	return os.WriteFile(TriedPath(stateDir), []byte(present+sig.String()+"\n"), 0o644)
}

// ClearTried removes the retry guard so the hook syncs again on the next prompt.
func ClearTried(stateDir string) error {
	err := os.Remove(TriedPath(stateDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
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

// --- staleness ---

// missingDir returns the first directory in dirs that no longer exists, or "".
func missingDir(dirs []string) string {
	for _, d := range dirs {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			return d
		}
	}
	return ""
}

// inputsNewer reports whether any input file's mtime is after the manifest's —
// the cheap check the shell hook mirrors with `-nt`.
func inputsNewer(inputs map[string]string, manifestMod time.Time) bool {
	for p := range inputs {
		if si, err := os.Stat(p); err == nil && si.ModTime().After(manifestMod) {
			return true
		}
	}
	return false
}

// boolResult renders a probe boolean as the "1"/"0" recorded form.
func boolResult(ok bool) string {
	if ok {
		return "1"
	}
	return "0"
}

// probeNow returns a probe's current boolean observation — the value the shell
// hook folds into its probe signal with `[ -e ]` / `command -v`.
func probeNow(kind, arg string) bool {
	switch kind {
	case "exists":
		_, err := os.Stat(arg)
		return err == nil
	case "which":
		_, err := exec.LookPath(arg)
		return err == nil
	}
	return false
}

// probeDrifted reports whether a recorded probe's result differs from the
// current world (a probed path appeared/vanished, or a binary was
// installed/removed/moved).
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

// probesChanged reports whether any recorded probe has drifted.
func probesChanged(probes []model.Probe) bool {
	for _, p := range probes {
		if probeDrifted(p.Kind, p.Arg, p.Result) {
			return true
		}
	}
	return false
}

// NeedsSync reports whether a sync is needed. It mirrors the shell hook's cheap
// check so the two never disagree: sync is needed when there is no completed
// sync (manifest absent) or any staleness dimension reports drift — a config
// input newer than the manifest, a removed tool directory, or a changed
// exists()/which() probe. The dimensions run concurrently. The manifest is
// written only at the end of a successful sync, so a failed/partial sync never
// suppresses a retry.
func NeedsSync(stateDir, configPath string) (bool, error) {
	mi, err := os.Stat(ManifestPath(stateDir))
	if err != nil {
		return true, nil // no completed sync yet
	}
	m, err := Load(stateDir)
	if err != nil {
		return true, nil
	}
	// Fallback for an empty/legacy manifest: at least watch the config file.
	inputs := m.Inputs
	if len(inputs) == 0 {
		inputs = map[string]string{configPath: ""}
	}
	mod := mi.ModTime()

	var results [3]bool
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); results[0] = inputsNewer(inputs, mod) }()
	go func() { defer wg.Done(); results[1] = missingDir(m.ToolDirs) != "" }()
	go func() { defer wg.Done(); results[2] = probesChanged(m.Probes) }()
	wg.Wait()
	return results[0] || results[1] || results[2], nil
}

// StaleReason describes why generated scripts are considered stale.
type StaleReason struct {
	Stale  bool
	Reason string
}

// CheckStale performs a thorough staleness check for `tau status`/`tau check`:
// it re-hashes every config input (not just compares mtimes), verifies the
// generated scripts exist, and checks tool dirs and probes. Not the hot path.
func CheckStale(stateDir string, expectedShells []string) StaleReason {
	m, err := Load(stateDir)
	if err != nil {
		return StaleReason{Stale: true, Reason: "no generated manifest; run `tau sync`"}
	}

	// Config inputs: content hashes must match.
	for path, want := range m.Inputs {
		got, err := HashFile(path)
		if err != nil || got != want {
			return StaleReason{Stale: true, Reason: "a config input changed; run `tau sync`"}
		}
	}

	// Generated scripts must exist.
	for _, sh := range expectedShells {
		for _, kind := range []string{"activate", "deactivate"} {
			p := filepath.Join(GenDir(stateDir), kind+"."+sh)
			if _, err := os.Stat(p); err != nil {
				return StaleReason{Stale: true, Reason: "generated scripts are missing; run `tau sync`"}
			}
		}
	}

	// Tool directories (mise store bins, pip/uv venv, npm prefix) must exist.
	if d := missingDir(m.ToolDirs); d != "" {
		return StaleReason{Stale: true, Reason: "tool directory " + d + " is missing; run `tau sync`"}
	}

	// exists()/which() probes: a probed file/binary changed state since sync.
	if probesChanged(m.Probes) {
		return StaleReason{Stale: true, Reason: "a probed file or binary (exists/which) changed; run `tau sync`"}
	}

	return StaleReason{Stale: false}
}
