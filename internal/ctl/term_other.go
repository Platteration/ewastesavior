//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package ctl

import (
	"context"
	"errors"
	"os"
)

func isTerminal(*os.File) bool { return false }

func readSecretLine(context.Context, *os.File) (string, error) {
	return "", errors.New("reading a hidden password is not supported on this system")
}
