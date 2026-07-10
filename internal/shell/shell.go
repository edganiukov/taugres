// Package shell defines the shells supported across hooks, rendering, and
// validation. Keeping the registry here prevents those layers from drifting.
package shell

import "slices"

const (
	Bash = "bash"
	Zsh  = "zsh"
	Fish = "fish"
)

// Supported is ordered deterministically for generated files and diagnostics.
var Supported = []string{Bash, Zsh, Fish}

// IsSupported reports whether name is a supported shell.
func IsSupported(name string) bool {
	return slices.Contains(Supported, name)
}
