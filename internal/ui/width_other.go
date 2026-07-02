//go:build !unix

package ui

import "os"

// terminalWidth cannot be determined on this platform; callers fall back to a default width.
func terminalWidth(f *os.File) int { return 0 }
