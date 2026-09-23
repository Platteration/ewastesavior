//go:build !linux

package display

import "errors"

var errNoVT = errors.New("virtual terminals are only supported on Linux")

func openVT(path string, onRelease, onAcquire func()) (vtHandle, error) { return nil, errNoVT }

func resetVT(path string) error { return errNoVT }

func unblankFB(path string) error { return errNotLinux }
