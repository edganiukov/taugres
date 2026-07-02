// Package lock manages the committed .taugres.lock file, which pins the
// concrete resolved version of every tool/package so `tau sync` is reproducible
// by default. Each entry records both the requested spec (from the config) and
// the resolved concrete version; when the requested spec changes, the entry is
// re-resolved.
package lock

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// FileName is the committed lockfile name at the project root.
const FileName = ".taugres.lock"

// version is the current lockfile schema version.
const version = 1

// Entry pins one tool/package. It is deliberately machine-independent (no
// absolute paths) so the committed lockfile stays reproducible across machines.
type Entry struct {
	// Requested is the spec from the config (e.g. "22", "" for unpinned).
	Requested string `json:"requested"`
	// Resolved is the concrete installed version (e.g. "22.11.0").
	Resolved string `json:"resolved"`
}

// File is the parsed lockfile.
type File struct {
	LockfileVersion int              `json:"lockfileVersion"`
	Mise            map[string]Entry `json:"mise,omitempty"`
	Pip             map[string]Entry `json:"pip,omitempty"`
	Npm             map[string]Entry `json:"npm,omitempty"`
	Uv              map[string]Entry `json:"uv,omitempty"`
}

// Path returns the lockfile path for a project root.
func Path(projectRoot string) string {
	return filepath.Join(projectRoot, FileName)
}

// Load reads the lockfile, returning an empty (but initialized) File if it does
// not exist.
func Load(projectRoot string) (*File, error) {
	data, err := os.ReadFile(Path(projectRoot))
	if os.IsNotExist(err) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}

	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Mise == nil {
		f.Mise = map[string]Entry{}
	}
	if f.Pip == nil {
		f.Pip = map[string]Entry{}
	}
	if f.Npm == nil {
		f.Npm = map[string]Entry{}
	}
	if f.Uv == nil {
		f.Uv = map[string]Entry{}
	}

	return &f, nil
}

// New returns an empty lockfile.
func New() *File {
	return &File{
		LockfileVersion: version,
		Mise:            map[string]Entry{},
		Pip:             map[string]Entry{},
		Npm:             map[string]Entry{},
		Uv:              map[string]Entry{},
	}
}

// Save writes the lockfile to the project root.
func (f *File) Save(projectRoot string) error {
	f.LockfileVersion = version
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(Path(projectRoot), data, 0o644)
}

// InstallVersion decides which version to install for a tool, given its current
// config spec, the existing lock entry (e, present), and whether this is an
// update run.
//
//   - update + unpinned (spec==""): "" (install latest, then re-lock).
//   - locked and the requested spec is unchanged: the locked resolved version
//     (reproducible).
//   - otherwise: the config spec (empty means latest), which will be re-locked.
func InstallVersion(spec string, e Entry, present, update bool) string {
	if update && spec == "" {
		return ""
	}
	if present && e.Requested == spec {
		return e.Resolved
	}
	return spec
}
