package hwinfo

import (
	"math"
	"strconv"
	"strings"
)

// maxCPUInfoSize caps /proc/cpuinfo reads (about 1.5 KiB per logical CPU).
const maxCPUInfoSize = 4 << 20

// ReportedCPUFlags is the subset of /proc/cpuinfo flags put into
// Inventory.CPUFlags, in this order. /proc/cpuinfo calls SSE3 "pni"; it is
// reported as "sse3".
var ReportedCPUFlags = []string{
	"lm", "pae", "nx", "sse", "sse2", "sse3", "ssse3", "sse4_1", "sse4_2",
	"avx", "avx2", "aes", "vmx", "svm", "hypervisor",
}

// cpuInfo is what /proc/cpuinfo tells about the processors.
type cpuInfo struct {
	vendor, model string
	flags         map[string]bool // of the first processor, "pni" mapped to "sse3"
	logical       int             // "processor" entries
	physical      int             // distinct cores; 0 = unknown
	maxMHz        float64         // highest "cpu MHz" (current clock)
}

type cpuBlock struct {
	physID, coreID string
	cores          int
}

// parseCPUInfo parses x86 /proc/cpuinfo.
func parseCPUInfo(data string) cpuInfo {
	var ci cpuInfo
	var blocks []cpuBlock
	cur := -1
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "processor" {
			blocks = append(blocks, cpuBlock{})
			cur = len(blocks) - 1
			continue
		}
		switch k {
		case "vendor_id":
			if ci.vendor == "" {
				ci.vendor = v
			}
		case "model name":
			if ci.model == "" {
				ci.model = collapseSpace(v)
			}
		case "cpu MHz":
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > ci.maxMHz && f < 1e6 {
				ci.maxMHz = f
			}
		case "flags":
			if ci.flags == nil {
				ci.flags = map[string]bool{}
				for _, f := range strings.Fields(v) {
					if f == "pni" {
						f = "sse3"
					}
					ci.flags[f] = true
				}
			}
		case "physical id":
			if cur >= 0 {
				blocks[cur].physID = v
			}
		case "core id":
			if cur >= 0 {
				blocks[cur].coreID = v
			}
		case "cpu cores":
			if cur >= 0 {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 4096 {
					blocks[cur].cores = n
				}
			}
		}
	}
	ci.logical = len(blocks)
	ci.physical = physicalCores(blocks)
	return ci
}

// physicalCores counts distinct (package, core) pairs. Without topology
// fields it falls back to "cpu cores" per package, and to 1 for a single
// logical CPU; otherwise it returns 0 (unknown).
func physicalCores(blocks []cpuBlock) int {
	if len(blocks) == 0 {
		return 0
	}
	pairs := map[[2]string]bool{}
	pkgCores := map[string]int{}
	complete, perPkg := true, true
	for _, b := range blocks {
		if b.physID == "" || b.coreID == "" {
			complete = false
		}
		if b.physID == "" || b.cores == 0 {
			perPkg = false
		}
		pairs[[2]string{b.physID, b.coreID}] = true
		pkgCores[b.physID] = b.cores
	}
	switch {
	case complete:
		return len(pairs)
	case perPkg:
		n := 0
		for _, c := range pkgCores {
			n += c
		}
		return n
	case len(blocks) == 1:
		return 1
	}
	return 0
}

// cpuFlagSubset returns the ReportedCPUFlags present in flags.
func cpuFlagSubset(flags map[string]bool) []string {
	var out []string
	for _, f := range ReportedCPUFlags {
		if flags[f] {
			out = append(out, f)
		}
	}
	return out
}

// parseCPUList counts the CPUs in a kernel cpulist such as "0-3,6,8-9".
func parseCPUList(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n := 0
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return 0, false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return 0, false
			}
		}
		n += b - a + 1
		if n > 1<<16 {
			return 0, false
		}
	}
	return n, true
}

// onlineCPUs returns the number of online logical CPUs from sysfs, or 0.
func onlineCPUs(root string) int {
	n, ok := parseCPUList(attr(rootPath(root, "sys/devices/system/cpu/online")))
	if !ok {
		return 0
	}
	return n
}

// maxCPUFreqMHz returns the highest cpufreq cpuinfo_max_freq in MHz, or 0.
func maxCPUFreqMHz(root string) int {
	dir := rootPath(root, "sys/devices/system/cpu")
	best := int64(0)
	for _, name := range listDir(dir) {
		if _, ok := numberedName(name, "cpu"); !ok {
			continue
		}
		khz, ok := readInt(rootPath(root, "sys/devices/system/cpu/"+name+"/cpufreq/cpuinfo_max_freq"))
		if ok && khz > best && khz < 100_000_000 {
			best = khz
		}
	}
	return int((best + 500) / 1000)
}

// cpuMHz picks the max frequency when cpufreq knows it, else the current
// clock from /proc/cpuinfo.
func cpuMHz(root string, ci cpuInfo) int {
	if mhz := maxCPUFreqMHz(root); mhz > 0 {
		return mhz
	}
	return int(math.Round(ci.maxMHz))
}
