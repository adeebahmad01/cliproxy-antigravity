package main

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "agy-test-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	if runtime.GOOS != "windows" {
		os.Setenv("PATH", home+":/usr/bin:/bin")
	}
	binarySearchDirectories = func() []string { return nil }
	if found := resolveBinaryPath("agy"); isExecutableFile(found) {
		fmt.Fprintf(os.Stderr, "a real agy is reachable at %s; tests must use a fake binary (set cfg.BinaryPath to a script) so they never start the real agy on a real account\n", found)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
