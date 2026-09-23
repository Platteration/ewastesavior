package hwinfo

import (
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Sampler takes live metrics samples. It keeps the state that spans
// samples: the previous CPU counters (CPU% is a delta) and sensor histories
// (stuck-sensor detection). It is safe for concurrent use.
type Sampler struct {
	root     string
	maxTempC float64

	mu      sync.Mutex
	now     func() time.Time
	prevCPU cpuTimes
	lastPct float64
	stuck   stuckFilter
}

// NewSampler returns a sampler for root. maxTempC is the configured
// max_temp_c, the upper bound of Metrics.CPUTempLimitC. It reads the CPU
// counters once, so the first Sample already reports CPU% since this call.
func NewSampler(root string, maxTempC float64) *Sampler {
	s := &Sampler{root: root, maxTempC: maxTempC, now: time.Now}
	s.prevCPU, _ = readCPUTimes(root)
	return s
}

// SetClock replaces the clock used for Metrics.Time and stuck-sensor
// detection (tests). The default is time.Now, whose monotonic reading keeps
// wall-clock steps from marking sensors stuck.
func (s *Sampler) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Sample reads all metrics. Unknown values are left at zero, except
// BatteryPercent, which is -1 without a battery.
func (s *Sampler) Sample() proto.Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	m := proto.Metrics{Time: now, BatteryPercent: -1}

	if f := firstFields(rootPath(s.root, "proc/uptime")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil && v >= 0 && v < 1e15 {
			m.UptimeS = int64(v)
		}
	}
	if f := firstFields(rootPath(s.root, "proc/loadavg")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil && v >= 0 && !math.IsInf(v, 0) {
			m.Load1 = v
		}
	}
	m.CPUPercent = s.cpuPercent()

	if mi, ok := readMemInfo(s.root); ok {
		m.MemAvailableMB = kbToMB(mi.availKB)
		m.SwapUsedMB = kbToMB(mi.swapTotalKB - mi.swapFreeKB)
	}
	m.MemPressure = readPSIMemory(s.root)

	groups := [][]sensorReading{readHwmonCPU(s.root), readACPIZones(s.root)}
	t := pickCPUTemp(groups, s.stuck.check(now, groups))
	m.CPUTempC = t.celsius
	m.CPUTempLimitC = tempLimit(s.maxTempC, t.limits)
	m.ThrottleEvents = throttleEvents(s.root)

	bs := summarizeBatteries(readSupplies(s.root))
	m.OnBattery = bs.onBattery
	if bs.present {
		m.BatteryPercent = bs.percent
		m.BatteryHealthPercent = bs.health
		m.BatteryStatus = bs.status
	}
	m.LidClosed = lidClosed(s.root)
	m.NetRxBytes, m.NetTxBytes = netBytes(s.root)
	return m
}

// cpuTimes are the aggregate /proc/stat counters.
type cpuTimes struct {
	total, idle uint64
	ok          bool
}

// readCPUTimes parses the "cpu" line of /proc/stat. guest and guest_nice
// are already included in user and nice, so they're left out of the total.
func readCPUTimes(root string) (cpuTimes, bool) {
	b, err := readFileLimit(rootPath(root, "proc/stat"), maxAttrSize)
	if err != nil {
		return cpuTimes{}, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return cpuTimes{}, false
	}
	var t cpuTimes
	for i, s := range f[1:] {
		if i >= 8 { // user nice system idle iowait irq softirq steal
			break
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		t.total += v
		if i == 3 || i == 4 { // idle, iowait
			t.idle += v
		}
	}
	t.ok = true
	return t, true
}

// cpuPercent returns busy time since the previous call. Without progress
// (called twice within a tick) or when the counters went backwards (CPU
// hot-unplug), it repeats the last value and re-baselines.
func (s *Sampler) cpuPercent() float64 {
	cur, ok := readCPUTimes(s.root)
	if !ok {
		return 0
	}
	prev := s.prevCPU
	s.prevCPU = cur
	if !prev.ok || cur.total < prev.total || cur.idle < prev.idle {
		return s.lastPct
	}
	dt := cur.total - prev.total
	if dt == 0 {
		return s.lastPct
	}
	di := min(cur.idle-prev.idle, dt)
	pct := float64(dt-di) * 100 / float64(dt)
	s.lastPct = math.Round(pct*10) / 10
	return s.lastPct
}

// firstFields returns the fields of the first line of a file.
func firstFields(p string) []string {
	b, err := readFileLimit(p, maxAttrSize)
	if err != nil {
		return nil
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.Fields(line)
}

// readPSIMemory returns the "some avg10" memory pressure, 0 when PSI is
// unavailable (kernel without CONFIG_PSI, or psi=0).
func readPSIMemory(root string) float64 {
	b, err := readFileLimit(rootPath(root, "proc/pressure/memory"), maxAttrSize)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "some" {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg10="); ok {
				p, err := strconv.ParseFloat(v, 64)
				if err != nil || p < 0 || p > 100 {
					return 0
				}
				return p
			}
		}
	}
	return 0
}

// throttleEvents sums the thermal throttle counters of all CPUs.
func throttleEvents(root string) uint64 {
	var sum uint64
	dir := rootPath(root, "sys/devices/system/cpu")
	for _, cpu := range listDir(dir) {
		if _, ok := numberedName(cpu, "cpu"); !ok {
			continue
		}
		tdir := filepath.Join(dir, cpu, "thermal_throttle")
		for _, f := range listDir(tdir) {
			if !strings.HasSuffix(f, "_throttle_count") {
				continue
			}
			if v, err := strconv.ParseUint(attr(filepath.Join(tdir, f)), 10, 64); err == nil {
				sum += v
			}
		}
	}
	return sum
}

// lidClosed reports whether any ACPI lid reports "closed".
func lidClosed(root string) bool {
	dir := rootPath(root, "proc/acpi/button/lid")
	for _, lid := range listDir(dir) {
		if strings.Contains(attr(filepath.Join(dir, lid, "state")), "closed") {
			return true
		}
	}
	return false
}

// netBytes sums received and transmitted bytes of all non-loopback
// interfaces from /proc/net/dev.
func netBytes(root string) (rx, tx uint64) {
	b, err := readFileLimit(rootPath(root, "proc/net/dev"), 1<<20)
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 == nil && err2 == nil {
			rx += r
			tx += t
		}
	}
	return rx, tx
}
