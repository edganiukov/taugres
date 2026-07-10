//go:build !linux && !darwin

package state

import "os"

func inputChangeTime(info os.FileInfo) int64 {
	return 0
}
