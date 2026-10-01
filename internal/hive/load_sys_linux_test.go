package hive

import (
	"bufio"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// loadCPUSeconds returns this process's user+system CPU time.
func loadCPUSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	tv := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return tv(ru.Utime) + tv(ru.Stime)
}

// loadDiskWrites returns the bytes this process caused to be written to
// storage (/proc/self/io write_bytes), or -1.
func loadDiskWrites() int64 {
	f, err := os.Open("/proc/self/io")
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "write_bytes: "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return -1
}

// loadSourceAddr gives simulated node i its own loopback source address
// (127.10.x.y), so per-source limits (hello rate) see one machine per node,
// as on a real LAN. Linux routes all of 127/8 to lo.
func loadSourceAddr(i int) net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 10, byte(i/250), byte(i%250+1))}
}
