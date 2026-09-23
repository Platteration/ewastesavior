//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package ctl

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
