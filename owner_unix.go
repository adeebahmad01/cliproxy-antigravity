//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// validateStagingDirPrivacy rejects a staging root another local user could
// control: MkdirAll keeps an existing directory's mode and owner, and a shared
// /tmp entry created first by someone else would otherwise be accepted.
func validateStagingDirPrivacy(dir string, info os.FileInfo) error {
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("staging directory %s has permissions %04o, want 0700", dir, perm)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if uid := int(stat.Uid); uid != os.Getuid() {
			return fmt.Errorf("staging directory %s is owned by uid %d, want %d", dir, uid, os.Getuid())
		}
	}
	return nil
}
