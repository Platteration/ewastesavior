//go:build !linux

package node

import (
	"errors"
	"time"
)

func setSystemClock(time.Time) error { return errors.New("not supported on this OS") }
func localNTPSynced() bool           { return false }
func setHostname(string)             {}
func freeDiskMB(string) (int, error) { return 0, errors.New("not supported on this OS") }
