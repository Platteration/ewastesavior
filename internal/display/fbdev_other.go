//go:build !linux

package display

import "errors"

// errNotLinux is returned by the hardware paths on other systems.
var errNotLinux = errors.New("framebuffer displays are only supported on Linux")

// OpenFramebuffer opens a Linux framebuffer; on this OS it always fails.
func OpenFramebuffer(path string, rotate int) (Device, error) { return nil, errNotLinux }

func openAuto(sysRoot string, rotate int) (Device, error) { return nil, errNotLinux }
