//go:build !linux

package storage

import (
	"errors"
	"runtime"
)

var errUnsupported = errors.New("savior storage: not supported on " + runtime.GOOS + " (SaviorOS boot helper)")

func platformOps() sysOps {
	return sysOps{
		mount:       func(string, string, string, mountOpts) error { return errUnsupported },
		unmount:     func(string) error { return errUnsupported },
		rereadPT:    func(string) error { return errUnsupported },
		addPart:     func(string, DataPlan, int) error { return errUnsupported },
		mknod:       func(string, string) error { return errUnsupported },
		loadModules: func([]string) {},
	}
}
