//go:build !linux

package hwinfo

// liveSupported reports whether the running system can be read.
func liveSupported() error { return ErrUnsupported }

// uname is only available on Linux.
func uname() (release, machine string) { return "", "" }
