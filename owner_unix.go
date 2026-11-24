//go:build !windows

package main

import (
	"os"
	"syscall"
)

// fileOwnerID reports the owning uid of a file on platforms that track it.
func fileOwnerID(info os.FileInfo) (int, bool) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid), true
	}
	return 0, false
}

// stagingOwnedByCurrentUser reports whether uid owns the current process's user.
func stagingOwnedByCurrentUser(uid int) bool {
	return uid == os.Getuid()
}
