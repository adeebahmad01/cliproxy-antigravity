//go:build windows

package main

import "os"

// fileOwnerID is unavailable on Windows; the permission-mode check in
// ensurePrivateStagingDir still runs, and the per-turn directory keeps the staged
// file unpredictable.
func fileOwnerID(info os.FileInfo) (int, bool) {
	return 0, false
}

func stagingOwnedByCurrentUser(uid int) bool {
	return true
}
