//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validateStagingDirPrivacy keeps the staged prompt inside a user-scoped
// directory. Go reports 0777 for any writable directory on Windows regardless of
// its ACL and stdlib cannot inspect ACLs, so instead of a mode check the root must
// live under %LOCALAPPDATA% (os.UserCacheDir) or the user profile. That way a
// TEMP/TMP redirected to a shared location cannot expose staged prompts to other
// local accounts, without pulling in golang.org/x/sys/windows for a DACL check.
func validateStagingDirPrivacy(dir string, info os.FileInfo) error {
	if !info.IsDir() {
		return os.ErrInvalid
	}
	var bases []string
	if cache, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cache) != "" {
		bases = append(bases, cache)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		bases = append(bases, home)
	}
	for _, base := range bases {
		if isPathWithin(dir, base) {
			return nil
		}
	}
	return fmt.Errorf("staging directory %s is outside the user profile; point TEMP at a private directory or leave max_prompt_bytes unset", dir)
}

// isPathWithin reports whether path is base or below it, comparing cleaned
// absolute paths case-insensitively as Windows does.
func isPathWithin(path, base string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	absPath = strings.ToLower(filepath.Clean(absPath))
	absBase = strings.ToLower(filepath.Clean(absBase))
	if absPath == absBase {
		return true
	}
	return strings.HasPrefix(absPath, absBase+string(filepath.Separator))
}
