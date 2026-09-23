package ctl

import (
	"context"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// isTerminal reports whether f is an interactive console.
func isTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

// readSecretLine reads a line from the console f with echo turned off.
// The console mode is restored on return and when ctx ends (Ctrl-C).
func readSecretLine(ctx context.Context, f *os.File) (string, error) {
	h := windows.Handle(f.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return "", err
	}
	mode := old&^windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT
	if err := windows.SetConsoleMode(h, mode); err != nil {
		return "", err
	}
	restore := sync.OnceFunc(func() { _ = windows.SetConsoleMode(h, old) })
	defer restore()
	return readLineCtx(ctx, f, restore)
}
