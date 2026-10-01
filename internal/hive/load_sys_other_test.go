//go:build !linux

package hive

import "net"

func loadCPUSeconds() float64 { return 0 }

func loadDiskWrites() int64 { return -1 }

// loadSourceAddr: only Linux routes all of 127/8 to the loopback device,
// so elsewhere every simulated node connects from 127.0.0.1.
func loadSourceAddr(int) net.Addr { return nil }
