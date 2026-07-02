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
	"path/filepath"
	"sort"
	"strings"
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
	for _, d := range readLines(ToolDirsPath(stateDir)) {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			return d
		}
	}
	return ""
}

// NeedsSync reports whether a sync is needed. It mirrors the shell hook's cheap
// check so the two never disagree: sync is needed when there is no completed
// sync (manifest absent), any config input (config file, loaded module, or
// fn.source file) is newer than the manifest, or a recorded tool directory has
// been removed. The manifest is written only at the end of a successful sync,
// so a failed/partial sync never suppresses a retry.
func NeedsSync(stateDir, configPath string) (bool, error) {
	mi, err := os.Stat(ManifestPath(stateDir))
	if err != nil {
		return true, nil // no completed sync yet
	}
	// Sources recorded by the last sync; fall back to just the config file.
	sources := readLines(SourcesPath(stateDir))
	if len(sources) == 0 {
		sources = []string{configPath}
	}
	for _, s := range sources {
		if si, err := os.Stat(s); err == nil && si.ModTime().After(mi.ModTime()) {
			return true, nil
		}
	}
	return missingToolDir(stateDir) != "", nil
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

	return StaleReason{Stale: false}
}

// SortedShells returns a sorted copy for deterministic manifests.
func SortedShells(shells []string) []string {
	out := append([]string{}, shells...)
	sort.Strings(out)
	return out
}
