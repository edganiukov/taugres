// Package discover implements Taugres workspace/project discovery and root
// resolution.
package discover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	WorkspaceFile = "workspace.tg"
	ProjectFile   = "project.tg"
)

// Discovery is the result of walking upward from a starting directory to find
// the active config and repository root.
type Discovery struct {
	// ConfigPath is the absolute path of the nearest active config file
	// (project.tg or workspace.tg).
	ConfigPath string
	// ProjectRoot is the directory containing ConfigPath.
	ProjectRoot string
	// RepoRoot is the nearest enclosing workspace.tg directory, or ProjectRoot
	// if none is found above.
	RepoRoot string
	// IsWorkspace reports whether the active config is a workspace.tg.
	IsWorkspace bool
}

// ErrNotFound is returned when no config is found walking up from start.
var ErrNotFound = errors.New("no workspace.tg or project.tg found in this directory or any parent")

// BothFilesError indicates a directory contains both config files.
type BothFilesError struct {
	Dir string
}

func (e *BothFilesError) Error() string {
	return fmt.Sprintf(
		"directory %q contains both %s and %s; a directory may contain only one",
		e.Dir, WorkspaceFile, ProjectFile,
	)
}

// Discover walks upward from start to locate the nearest active config, then
// resolves the repository root. start is treated as an absolute path.
func Discover(start string) (*Discovery, error) {
	start, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}

	configPath, isWorkspace, err := findNearestConfig(start)
	if err != nil {
		return nil, err
	}

	projectRoot := filepath.Dir(configPath)

	var repoRoot string
	if isWorkspace {
		// A workspace.tg is itself a repo-root marker.
		repoRoot = projectRoot
	} else {
		ws, err := findNearestWorkspace(projectRoot)
		if err != nil {
			return nil, err
		}

		if ws != "" {
			repoRoot = filepath.Dir(ws)
		} else {
			repoRoot = projectRoot
		}
	}

	return &Discovery{
		ConfigPath:  configPath,
		ProjectRoot: projectRoot,
		RepoRoot:    repoRoot,
		IsWorkspace: isWorkspace,
	}, nil
}

// findNearestConfig walks upward from dir returning the first directory that
// contains a config file. Reports an error if a directory has both files.
func findNearestConfig(dir string) (configPath string, isWorkspace bool, err error) {
	for {
		hasWorkspace := fileExists(filepath.Join(dir, WorkspaceFile))
		hasProject := fileExists(filepath.Join(dir, ProjectFile))
		if hasWorkspace && hasProject {
			return "", false, &BothFilesError{Dir: dir}
		}

		if hasProject {
			return filepath.Join(dir, ProjectFile), false, nil
		}

		if hasWorkspace {
			return filepath.Join(dir, WorkspaceFile), true, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, ErrNotFound
		}

		dir = parent
	}
}

// findNearestWorkspace walks upward from dir (inclusive) returning the first
// workspace.tg path found, or "" if none.
func findNearestWorkspace(dir string) (string, error) {
	for {
		p := filepath.Join(dir, WorkspaceFile)
		if fileExists(p) {
			// Guard against the both-files invariant at this level too.
			if fileExists(filepath.Join(dir, ProjectFile)) {
				return "", &BothFilesError{Dir: dir}
			}
			return p, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}

		dir = parent
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
