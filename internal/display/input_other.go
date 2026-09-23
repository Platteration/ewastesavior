//go:build !linux

package display

import "context"

// watchInput is a no-op on systems without evdev.
func watchInput(ctx context.Context, dir string, activity func()) {}
