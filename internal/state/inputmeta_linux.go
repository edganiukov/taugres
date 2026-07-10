//go:build linux

package state

import (
	"os"
	"syscall"
)

func inputChangeTime(info os.FileInfo) int64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ctim.Sec*1e9 + stat.Ctim.Nsec
}
