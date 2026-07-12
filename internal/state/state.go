// Package state manages Taugres per-project metadata under
// <projectRoot>/.taugres/gen (the generated shell scripts and the manifest) and
// provides cheap stale detection.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/edganiukov/taugres/internal/atomicfile"
	"github.com/edganiukov/taugres/internal/model"
)

// GenDirName is the subdirectory holding tau's per-project metadata: the
// generated activation scripts and the manifest.
const GenDirName = "gen"

// ManifestName is the single per-project state file. It records everything the
// staleness checks need — one greppable `tag:payload` line per entry. It is
// written last in a sync, so its own mtime is the "last synced" anchor the
// mtime checks compare against.
//
//	version:2                                      schema version
//	tau:<build-stamp>                              tau build that wrote the manifest
//	input:<sha256>:<mtime>:<size>:<ctime>:<path>   config input and stat identity
//	tooldir:<abs-path>                             a tool bin dir that must exist
//	managerdir:<mgr>:<abs-path>                    explicit tool-dir ownership
//	probe:<kind>|<arg>|<result>                    exists()/which()/env() observation
//	toolsig:<mgr>:<sha256>                         completed manager fingerprint
//	toolpending:<mgr>                              incomplete manager install
const ManifestName = "manifest"

const manifestVersion = 2

// BuildStamp identifies the running tau build. Derived manifest state (mise
// bin dirs, rendered scripts) encodes tau's own logic, so a manifest written
// by a different build is stale: a fix to that logic takes effect on the very
// next sync — the shell hook triggers it automatically — without --force.
// Installs are not forced by this; already-present tools no-op. A clean VCS
// build is identified by its commit; dirty or unstamped builds (local `go
// build`, test binaries) fall back to the executable's stat identity, which
// changes on every rebuild.
var BuildStamp = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision, modified string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if revision != "" && modified == "false" {
			return "vcs:" + revision
		}
	}
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			return fmt.Sprintf("exe:%d:%d", fi.ModTime().UnixNano(), fi.Size())
		}
	}
	return "unknown"
})

// InputMetadata is the cheap filesystem identity recorded for a config input.
// Comparing it with the current file catches forward, backward, and same-size
// timestamp changes without reading file contents on every prompt.
type InputMetadata struct {
	ModTime    int64
	Size       int64
	ChangeTime int64
}

// Manifest is the parsed state file.
type Manifest struct {
	// Version is the parsed manifest schema. Version zero is the legacy format.
	Version int
	// TauBuild is the BuildStamp of the tau that wrote the manifest. A manifest
	// from a different build is stale (its derived state may be wrong), so the
	// next sync re-derives everything. Write fills it for new manifests; Load
	// preserves it so TouchManifest never launders a foreign build's state.
	TauBuild string

	// Inputs maps each config-input file (the config, loaded modules, and
	// fn/hook source files) to its content hash. Changed stat metadata is the
	// cheap NeedsSync trigger; a changed hash is the thorough status check.
	Inputs map[string]string
	// InputMetadata records the stat tuple observed when each input hash was
	// written. Legacy manifests have no entries and use the old mtime fallback.
	InputMetadata map[string]InputMetadata
	// ToolDirs are all bin dirs that must exist on disk; a missing one is stale.
	ToolDirs []string
	// ManagerDirs records ownership explicitly so adding a manager never relies
	// on subtracting convention-derived paths from the flat ToolDirs list.
	ManagerDirs map[string][]string
	// Probes are recorded exists()/which()/env() results; a changed result is stale.
	Probes []model.Probe
	// ToolSig maps each tool manager (mise/pip/npm/uv) to a fingerprint of its
	// declared tools/packages joined with their locked versions. When a manager's
	// signature is unchanged (and its bin dirs exist) its install is skipped; when
	// every manager is fresh, the whole install phase is skipped and only the
	// shell scripts are regenerated. Managers with nothing declared are absent.
	ToolSig map[string]string
	// PendingManagers records managers whose latest install did not complete.
	// Their signatures are not committed, and the manifest remains stale so a
	// manual sync retries while hook retries remain fingerprint-guarded.
	PendingManagers []string
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

	if m.InputMetadata == nil {
		m.InputMetadata = map[string]InputMetadata{}
	}

	if m.TauBuild == "" {
		m.TauBuild = BuildStamp()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "version:%d\n", manifestVersion)
	fmt.Fprintf(&b, "tau:%s\n", m.TauBuild)
	for _, p := range paths {
		meta, ok := m.InputMetadata[p]
		if !ok {
			meta, _ = statInput(p)
			m.InputMetadata[p] = meta
		}
		fmt.Fprintf(&b, "input:%s:%d:%d:%d:%s\n", m.Inputs[p], meta.ModTime, meta.Size, meta.ChangeTime, p)
	}
	for _, d := range m.ToolDirs {
		b.WriteString("tooldir:")
		b.WriteString(d)
		b.WriteByte('\n')
	}
	managerNames := make([]string, 0, len(m.ManagerDirs))
	for manager := range m.ManagerDirs {
		managerNames = append(managerNames, manager)
	}
	sort.Strings(managerNames)
	for _, manager := range managerNames {
		for _, dir := range m.ManagerDirs[manager] {
			fmt.Fprintf(&b, "managerdir:%s:%s\n", manager, dir)
		}
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
	pending := append([]string(nil), m.PendingManagers...)
	sort.Strings(pending)
	for _, mgr := range pending {
		b.WriteString("toolpending:")
		b.WriteString(mgr)
		b.WriteByte('\n')
	}
	m.Version = manifestVersion
	return atomicfile.Write(ManifestPath(stateDir), []byte(b.String()), 0o644)
}

// Load parses the manifest, or returns the os error (e.g. os.ErrNotExist) if it
// cannot be read.
func Load(stateDir string) (*Manifest, error) {
	data, err := os.ReadFile(ManifestPath(stateDir))
	if err != nil {
		return nil, err
	}
	m := &Manifest{
		Inputs:        map[string]string{},
		InputMetadata: map[string]InputMetadata{},
		ManagerDirs:   map[string][]string{},
		ToolSig:       map[string]string{},
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		tag, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch tag {
		case "version":
			m.Version, _ = strconv.Atoi(rest)
		case "tau":
			m.TauBuild = rest
		case "input":
			hash, payload, ok := strings.Cut(rest, ":")
			if !ok {
				continue
			}
			if m.Version < manifestVersion {
				m.Inputs[payload] = hash
				continue
			}
			modText, payload, ok := strings.Cut(payload, ":")
			if !ok {
				continue
			}
			sizeText, payload, ok := strings.Cut(payload, ":")
			if !ok {
				continue
			}
			changeText, path, ok := strings.Cut(payload, ":")
			if !ok {
				continue
			}
			modTime, modErr := strconv.ParseInt(modText, 10, 64)
			size, sizeErr := strconv.ParseInt(sizeText, 10, 64)
			changeTime, changeErr := strconv.ParseInt(changeText, 10, 64)
			if modErr != nil || sizeErr != nil || changeErr != nil {
				continue
			}
			m.Inputs[path] = hash
			m.InputMetadata[path] = InputMetadata{ModTime: modTime, Size: size, ChangeTime: changeTime}
		case "tooldir":
			m.ToolDirs = append(m.ToolDirs, rest)
		case "managerdir":
			if manager, dir, ok := strings.Cut(rest, ":"); ok {
				m.ManagerDirs[manager] = append(m.ManagerDirs[manager], dir)
			}
		case "toolsig":
			if mgr, hash, ok := strings.Cut(rest, ":"); ok {
				m.ToolSig[mgr] = hash
			}
		case "toolpending":
			if rest != "" {
				m.PendingManagers = append(m.PendingManagers, rest)
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

// TouchManifest refreshes the recorded input metadata after a thorough hash
// check confirmed that a metadata-only change was a no-op. Rewriting also
// re-anchors the manifest's mtime for compatibility with legacy readers.
func TouchManifest(stateDir string) error {
	m, err := Load(stateDir)
	if err != nil {
		return err
	}
	metadata := make(map[string]InputMetadata, len(m.Inputs))
	for path, expectedHash := range m.Inputs {
		_, hash, value, err := ReadInput(path)
		if err != nil {
			return err
		}
		if hash != expectedHash {
			return fmt.Errorf("input changed while refreshing manifest: %s", path)
		}
		metadata[path] = value
	}
	m.InputMetadata = metadata
	return m.Write(stateDir)
}

func statInput(path string) (InputMetadata, error) {
	info, err := os.Stat(path)
	if err != nil {
		return InputMetadata{}, err
	}
	return InputMetadata{
		ModTime:    info.ModTime().UnixNano(),
		Size:       info.Size(),
		ChangeTime: inputChangeTime(info),
	}, nil
}

// SyncFingerprint hashes the complete cheap trigger state. It changes when an
// input's metadata changes in either direction, a generated script/tool dir
// appears or vanishes, a probe changes, or the set of pending managers changes.
// The hex result contains no '|' so it can ride in the hook session token.
func SyncFingerprint(stateDir, configPath string, shells []string) string {
	h := sha256.New()
	// The computing build is part of the trigger state: replacing the tau
	// binary invalidates outstanding hook session tokens, so the next prompt
	// re-inspects and resyncs instead of trusting a stale token.
	fmt.Fprintf(h, "tau:%s\x00", BuildStamp())
	writeStat := func(path string) {
		meta, err := statInput(path)
		if err != nil {
			fmt.Fprintf(h, "%s\x00missing\x00", path)
			return
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\x00", path, meta.ModTime, meta.Size, meta.ChangeTime)
	}

	writeStat(configPath)
	m, err := Load(stateDir)
	if err != nil {
		h.Write([]byte("manifest:missing\x00"))
	} else {
		paths := make([]string, 0, len(m.Inputs))
		for path := range m.Inputs {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			writeStat(path)
		}
		for _, sh := range shells {
			writeStat(filepath.Join(GenDir(stateDir), "activate."+sh))
			writeStat(filepath.Join(GenDir(stateDir), "deactivate."+sh))
		}
		for _, dir := range m.ToolDirs {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				fmt.Fprintf(h, "dir:%s:1\x00", dir)
			} else {
				fmt.Fprintf(h, "dir:%s:0\x00", dir)
			}
		}
		for _, p := range m.Probes {
			fmt.Fprintf(h, "probe:%s\x00%s\x00%s\x00", p.Kind, p.Arg, currentProbeResult(p.Kind, p.Arg))
		}
		pending := append([]string(nil), m.PendingManagers...)
		sort.Strings(pending)
		for _, mgr := range pending {
			fmt.Fprintf(h, "pending:%s\x00", mgr)
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}

// ReadInput reads a stable snapshot of an input, returning the bytes, content
// hash, and fd metadata from the same file instance. A file that changes during
// the read is rejected so evaluation and the manifest cannot describe different
// contents.
func ReadInput(path string) ([]byte, string, InputMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", InputMetadata{}, err
	}
	defer file.Close()

	beforeInfo, err := file.Stat()
	if err != nil {
		return nil, "", InputMetadata{}, err
	}
	before := InputMetadata{ModTime: beforeInfo.ModTime().UnixNano(), Size: beforeInfo.Size(), ChangeTime: inputChangeTime(beforeInfo)}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, "", InputMetadata{}, err
	}
	afterInfo, err := file.Stat()
	if err != nil {
		return nil, "", InputMetadata{}, err
	}
	after := InputMetadata{ModTime: afterInfo.ModTime().UnixNano(), Size: afterInfo.Size(), ChangeTime: inputChangeTime(afterInfo)}
	if before != after {
		return nil, "", InputMetadata{}, fmt.Errorf("input changed while reading: %s", path)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), after, nil
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
// MissingDir returns the first directory in dirs that no longer exists, or "".
func MissingDir(dirs []string) string {
	for _, d := range dirs {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			return d
		}
	}
	return ""
}

// inputsChanged compares each input's current stat tuple with the tuple
// recorded at sync. Legacy manifests fall back to the old newer-than-manifest
// check, but treat a missing input as stale.
func inputsChanged(inputs map[string]string, metadata map[string]InputMetadata, manifestModTime int64) bool {
	for path := range inputs {
		current, err := statInput(path)
		if err != nil {
			return true
		}
		if recorded, ok := metadata[path]; ok {
			if current != recorded {
				return true
			}
			continue
		}
		if current.ModTime > manifestModTime {
			return true
		}
	}
	return false
}

// currentProbeResult returns a probe's current observation in the same recorded
// form config uses (see model.Probe / model.EnvProbeResult), so a plain string
// comparison detects drift for every kind.
func currentProbeResult(kind, arg string) string {
	switch kind {
	case "exists":
		_, err := os.Stat(arg)
		if err != nil {
			return "0"
		}

		return "1"
	case "which":
		path, err := exec.LookPath(arg)
		if err != nil {
			return ""
		}

		return path
	case "env":
		val, ok := os.LookupEnv(arg)
		return model.EnvProbeResult(val, ok)
	}

	return ""
}

// probeDrifted reports whether a recorded probe's result differs from the
// current world (a probed path appeared/vanished, a binary was
// installed/removed/moved, or an env var changed).
func probeDrifted(kind, arg, recorded string) bool {
	return currentProbeResult(kind, arg) != recorded
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

func needsSyncLoaded(manifestInfo os.FileInfo, manifest *Manifest, configPath string) bool {
	if manifest.Version < manifestVersion {
		return true
	}
	if manifest.TauBuild != BuildStamp() {
		return true // a different tau build derived this state
	}
	inputs := manifest.Inputs
	if len(inputs) == 0 {
		inputs = map[string]string{configPath: ""}
	}
	if inputsChanged(inputs, manifest.InputMetadata, manifestInfo.ModTime().UnixNano()) {
		return true
	}
	if MissingDir(manifest.ToolDirs) != "" {
		return true
	}
	if probesChanged(manifest.Probes) {
		return true
	}
	return len(manifest.PendingManagers) > 0
}

// HookInspection is the state snapshot needed by one hook invocation.
type HookInspection struct {
	NeedsSync       bool
	ActivationPath  string
	ActivationStamp string
}

// InspectHook loads the manifest once, evaluates cheap staleness, and inspects
// the current shell's generated scripts. It centralizes the prompt hot path and
// reuses the activation stat as the session-token stamp.
func InspectHook(stateDir, configPath, shell string) HookInspection {
	activation := filepath.Join(GenDir(stateDir), "activate."+shell)
	deactivation := filepath.Join(GenDir(stateDir), "deactivate."+shell)
	result := HookInspection{ActivationPath: activation}

	manifestInfo, err := os.Stat(ManifestPath(stateDir))
	if err != nil {
		result.NeedsSync = true
	} else if manifest, err := Load(stateDir); err != nil {
		result.NeedsSync = true
	} else {
		result.NeedsSync = needsSyncLoaded(manifestInfo, manifest, configPath)
	}
	if info, err := os.Stat(activation); err == nil {
		result.ActivationStamp = strconv.FormatInt(info.ModTime().UnixNano(), 10)
	} else {
		result.NeedsSync = true
	}
	if _, err := os.Stat(deactivation); err != nil {
		result.NeedsSync = true
	}
	return result
}

// NeedsSync reports whether a sync is needed. It mirrors the shell hook's cheap
// check so the two never disagree: sync is needed when there is no completed
// sync (manifest absent) or any staleness dimension reports drift — changed
// input metadata, a removed tool directory, a changed exists()/which()/env()
// probe, or an incomplete manager install. Checks short-circuit in hot-path
// order and allocate no goroutines for the common small manifest. The manifest
// is written only at the end of a sync, so a failed manager remains explicit.
func NeedsSync(stateDir, configPath string) (bool, error) {
	mi, err := os.Stat(ManifestPath(stateDir))
	if err != nil {
		return true, nil // no completed sync yet
	}

	m, err := Load(stateDir)
	if err != nil {
		return true, nil
	}

	return needsSyncLoaded(mi, m, configPath), nil
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

	if m.TauBuild != BuildStamp() {
		return StaleReason{Stale: true, Reason: "tau was updated since the last sync; run `tau sync`"}
	}

	if len(m.PendingManagers) > 0 {
		return StaleReason{Stale: true, Reason: "tool installation is incomplete; run `tau sync`"}
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
	if d := MissingDir(m.ToolDirs); d != "" {
		return StaleReason{Stale: true, Reason: "tool directory " + d + " is missing; run `tau sync`"}
	}

	// exists()/which()/env() probes: a probed file, binary, or env var changed
	// state since sync.
	if probesChanged(m.Probes) {
		return StaleReason{Stale: true, Reason: "a probed file, binary, or env var (exists/which/env) changed; run `tau sync`"}
	}

	return StaleReason{Stale: false}
}
