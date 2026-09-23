package hive

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// ntpSynced reports whether the kernel considers the clock synchronized
// (adjtimex status without STA_UNSYNC). ok is false when unknown.
func ntpSynced() (synced, ok bool) {
	var tx unix.Timex
	state, err := unix.Adjtimex(&tx)
	if err != nil {
		return false, false
	}
	return tx.Status&unix.STA_UNSYNC == 0 && state != unix.TIME_ERROR, true
}

func canSetSystemClock() bool { return os.Geteuid() == 0 }

// setSystemClock steps the system clock and, when hwclock exists, writes
// it to the RTC.
func setSystemClock(t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	if err := unix.Settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday: %w", err)
	}
	if p, err := exec.LookPath("hwclock"); err == nil {
		_ = exec.Command(p, "-w", "-u").Run()
	}
	return nil
}
