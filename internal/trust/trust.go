// Package trust implements a direnv-style allow/deny mechanism. Approving a
// project with `tau allow` records that its config path is trusted; it stays
// trusted until `tau deny`, so a single `tau allow` is enough even as the config
// evolves.
//
// Trade-off: unlike direnv, edits to an already-trusted config do not require
// re-approval. Trust here means "I trust this project", not "I trust this exact
// file content".
package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// record is the on-disk trust marker for a config path.
type record struct {
	ConfigPath string `json:"configPath"`
	AllowedAt  string `json:"allowedAt"`
}

// storeDir returns the directory holding trust records.
func storeDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "taugres", "trust"), nil
}

// recordPath maps a config path to its trust file (keyed by config path hash).
func recordPath(configPath string) (string, error) {
	dir, err := storeDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(configPath))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}

// Allow records that the given config path is trusted.
func Allow(configPath string) error {
	path, err := recordPath(configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record{
		ConfigPath: configPath,
		AllowedAt:  time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

// IsAllowed reports whether the given config path has been trusted.
func IsAllowed(configPath string) (bool, error) {
	path, err := recordPath(configPath)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Deny removes any stored trust for the given config path.
func Deny(configPath string) error {
	path, err := recordPath(configPath)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Prune removes trust records whose project config file no longer exists,
// returning the pruned config paths (sorted). This reclaims records orphaned
// when a project is deleted, since trust lives outside the project.
func Prune() ([]string, error) {
	dir, err := storeDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pruned []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r record
		if json.Unmarshal(data, &r) != nil || r.ConfigPath == "" {
			continue
		}
		if _, err := os.Stat(r.ConfigPath); os.IsNotExist(err) {
			if os.Remove(p) == nil {
				pruned = append(pruned, r.ConfigPath)
			}
		}
	}
	sort.Strings(pruned)
	return pruned, nil
}
