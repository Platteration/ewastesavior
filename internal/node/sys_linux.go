//go:build linux

package node

import (
	"time"

	"golang.org/x/sys/unix"
)

// setSystemClock steps the system clock (root only).
func setSystemClock(t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Settimeofday(&tv)
}

// localNTPSynced reports whether the kernel clock is disciplined by NTP.
func localNTPSynced() bool {
	var tx unix.Timex
	state, err := unix.Adjtimex(&tx)
	if err != nil {
		return false
	}
	return state != unix.TIME_ERROR && tx.Status&unix.STA_UNSYNC == 0
}

func setHostname(name string) {
	unix.Sethostname([]byte(name))
}

// freeDiskMB is the free space of the filesystem holding path.
func freeDiskMB(path string) (int, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int(uint64(st.Bavail) * uint64(st.Bsize) >> 20), nil
}
