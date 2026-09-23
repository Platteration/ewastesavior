//go:build linux

package hwinfo

import "golang.org/x/sys/unix"

// liveSupported reports whether the running system can be read.
func liveSupported() error { return nil }

// uname returns the kernel release and machine of the running system.
func uname() (release, machine string) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "", ""
	}
	return unix.ByteSliceToString(u.Release[:]), unix.ByteSliceToString(u.Machine[:])
}
