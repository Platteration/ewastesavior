//go:build !linux && !darwin && !windows

package hive

import "errors"

func statFS(string) (fsInfo, error) { return fsInfo{}, errors.New("statfs not supported") }

func isMountPoint(string) bool { return false }
