//go:build darwin

package cache

import (
	"os"
	"syscall"
)

func platformFileIdentity(info os.FileInfo) (int64, uint64) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return stat.Ctimespec.Sec*1_000_000_000 + stat.Ctimespec.Nsec, stat.Ino
}
