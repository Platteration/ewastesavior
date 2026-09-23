package hwinfo

import (
	"crypto/sha256"
	"math"
	"runtime"
	"time"
)

// benchChunk is hashed per step: small enough to stay in the L1/L2 caches
// of a Pentium 4 and to check the clock often on slow CPUs.
const benchChunk = 16 << 10

// Benchmark measures single-core SHA-256 throughput in MiB/s over about d
// (1 s when d <= 0), a rough CPU speed indicator for Inventory.BenchScore.
// It keeps one core busy for the whole duration. The result is at least 1.
func Benchmark(d time.Duration) int {
	if d <= 0 {
		d = time.Second
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	buf := make([]byte, benchChunk)
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	h := sha256.New()
	start := time.Now()
	var n int64
	var elapsed time.Duration
	for elapsed < d {
		h.Write(buf)
		n += benchChunk
		elapsed = time.Since(start)
	}
	_ = h.Sum(nil)
	mibps := float64(n) / (1 << 20) / elapsed.Seconds()
	return max(1, int(math.Round(mibps)))
}
