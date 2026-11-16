//go:build !windows

package main

/*
#define _XOPEN_SOURCE 600
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <termios.h>

static int open_pty(char *name, size_t len, unsigned short cols, unsigned short rows) {
	int fd = posix_openpt(O_RDWR | O_NOCTTY);
	if (fd < 0) return -1;
	if (grantpt(fd) != 0 || unlockpt(fd) != 0) return -1;
	char *n = ptsname(fd);
	if (n == NULL || strlen(n) >= len) return -1;
	strcpy(name, n);
	struct winsize ws;
	memset(&ws, 0, sizeof(ws));
	ws.ws_col = cols;
	ws.ws_row = rows;
	ioctl(fd, TIOCSWINSZ, &ws);
	return fd;
}
*/
import "C"

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// startInPTY starts cmd attached to a new pseudo-terminal and returns its controlling side.
// agy only offers sign-in through its interactive terminal UI, which needs a real TTY.
// The terminal is very wide so long URLs are printed on one line.
func startInPTY(cmd *exec.Cmd) (*os.File, error) {
	name := make([]byte, 256)
	fd := C.open_pty((*C.char)(unsafe.Pointer(&name[0])), C.size_t(len(name)), 4000, 50)
	if fd < 0 {
		return nil, fmt.Errorf("open pseudo-terminal failed")
	}
	master := os.NewFile(uintptr(fd), "ptmx")
	slaveName := C.GoString((*C.char)(unsafe.Pointer(&name[0])))
	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open pseudo-terminal %s: %w", slaveName, err)
	}
	defer slave.Close()

	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	return master, nil
}
