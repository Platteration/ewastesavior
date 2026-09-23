//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package ctl

import (
	"context"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

// readSecretLine reads a line from the terminal f with echo turned off.
// The terminal is restored on return and when ctx ends (Ctrl-C).
func readSecretLine(ctx context.Context, f *os.File) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return "", err
	}
	t := *old
	t.Lflag &^= unix.ECHO
	t.Lflag |= unix.ICANON | unix.ISIG
	t.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &t); err != nil {
		return "", err
	}
	restore := sync.OnceFunc(func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) })
	defer restore()
	return readLineCtx(ctx, f, restore)
}
