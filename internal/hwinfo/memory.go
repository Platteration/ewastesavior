package hwinfo

import (
	"strconv"
	"strings"
)

// memInfo holds the /proc/meminfo fields hwinfo uses, in KiB.
type memInfo struct {
	totalKB, availKB, swapTotalKB, swapFreeKB int64
	ok                                        bool // MemTotal was found
}

// parseMemInfo parses /proc/meminfo. MemAvailable (Linux 3.14+) falls back
// to MemFree + Buffers + Cached on older kernels.
func parseMemInfo(data string) memInfo {
	var m memInfo
	vals := map[string]int64{}
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil || n < 0 {
			continue
		}
		vals[strings.TrimSpace(k)] = n
	}
	m.totalKB, m.ok = vals["MemTotal"]
	if avail, ok := vals["MemAvailable"]; ok {
		m.availKB = avail
	} else {
		m.availKB = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	if m.totalKB > 0 && m.availKB > m.totalKB {
		m.availKB = m.totalKB
	}
	m.swapTotalKB = vals["SwapTotal"]
	m.swapFreeKB = vals["SwapFree"]
	if m.swapFreeKB > m.swapTotalKB {
		m.swapFreeKB = m.swapTotalKB
	}
	return m
}

func readMemInfo(root string) (memInfo, bool) {
	b, err := readFileLimit(rootPath(root, "proc/meminfo"), maxAttrSize)
	if err != nil {
		return memInfo{}, false
	}
	m := parseMemInfo(string(b))
	return m, m.ok
}

// kbToMB converts KiB to MiB (all hwinfo "MB" values are MiB).
func kbToMB(kb int64) int { return int(kb / 1024) }

// MemAvailableMB returns MemAvailable in MiB, or 0 when unknown.
func MemAvailableMB(root string) int {
	m, _ := readMemInfo(root)
	return kbToMB(m.availKB)
}

// MemTotalMB returns MemTotal in MiB, or 0 when unknown.
func MemTotalMB(root string) int {
	m, _ := readMemInfo(root)
	return kbToMB(m.totalKB)
}
