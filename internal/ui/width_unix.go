//go:build unix

package ui

import (
	"os"

	"golang.org/x/sys/unix"
)

// terminalWidth returns the column count of the terminal backing f, or 0 if it cannot be determined.
func terminalWidth(f *os.File) int {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws == nil {
		return 0
	}

	return int(ws.Col)
}
