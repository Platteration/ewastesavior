//go:build !linux

package hive

import (
	"errors"
	"time"
)

// ntpSynced assumes the host OS keeps its own clock synchronized (the hive
// runs as an ordinary program on macOS and Windows).
func ntpSynced() (synced, ok bool) { return true, true }

func canSetSystemClock() bool { return false }

func setSystemClock(time.Time) error {
	return errors.New("setting the clock is only supported on Linux")
}
