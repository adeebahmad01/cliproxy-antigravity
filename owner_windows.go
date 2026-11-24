//go:build windows

package main

import "os"

// validateStagingDirPrivacy has nothing to check on Windows: Go reports 0777 for
// any writable directory regardless of its ACL, so a mode-mask check would always
// fail. The staging root lives under the current user's %TEMP% (%LOCALAPPDATA%),
// which is already user-scoped, and each turn writes into its own
// os.MkdirTemp directory with the file opened O_EXCL.
func validateStagingDirPrivacy(dir string, info os.FileInfo) error {
	if !info.IsDir() {
		return os.ErrInvalid
	}
	return nil
}
