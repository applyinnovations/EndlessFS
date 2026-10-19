//go:build linux || darwin

package main

import (
	"os"
	"syscall"
)

func sameUID(raw uint64, uid int) bool { return uid >= 0 && raw == uint64(uid) }
func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && sameUID(uint64(stat.Uid), os.Geteuid())
}
