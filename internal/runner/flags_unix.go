//go:build unix

package runner

import "syscall"

// oNoFollow and oNonblock are passed to open(2) for files the task controls.
const (
	oNoFollow = syscall.O_NOFOLLOW
	oNonblock = syscall.O_NONBLOCK
)
