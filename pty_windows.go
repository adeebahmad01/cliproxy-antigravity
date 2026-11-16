//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

func startInPTY(*exec.Cmd) (*os.File, error) {
	return nil, errors.New("adding Antigravity accounts from CLIProxyAPI is not supported on Windows yet; sign in with agy and use accounts_dir")
}
