package hwinfo

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Collect gathers the static hardware inventory below root ("" or "/" = the
// running system). Every field it can't determine stays at its zero value;
// BenchScore is never set here because Benchmark takes seconds.
//
// It returns an error, together with the partial inventory, when root holds
// neither /proc/cpuinfo nor /proc/meminfo (not a Linux root), or when the
// live system is read on a non-Linux OS.
func Collect(root string) (proto.Inventory, error) {
	live := isLive(root)
	inv := proto.Inventory{Arch: runtime.GOARCH}
	if live {
		if err := liveSupported(); err != nil {
			inv.Cores = runtime.NumCPU()
			inv.Hostname, _ = os.Hostname()
			return inv, err
		}
	}

	inv.Hostname = attr(rootPath(root, "proc/sys/kernel/hostname"))
	if inv.Hostname == "" && live {
		inv.Hostname, _ = os.Hostname()
	}
	inv.Kernel, inv.MachineArch = kernelInfo(root, live)
	inv.OSVersion = osVersion(root)

	cpuData, cpuErr := readFileLimit(rootPath(root, "proc/cpuinfo"), maxCPUInfoSize)
	ci := parseCPUInfo(string(cpuData))
	inv.CPUModel = ci.model
	inv.CPUVendor = ci.vendor
	inv.CPUFlags = cpuFlagSubset(ci.flags)
	inv.Cores = onlineCPUs(root)
	if inv.Cores == 0 {
		inv.Cores = ci.logical
	}
	if inv.Cores == 0 && live {
		inv.Cores = runtime.NumCPU()
	}
	inv.PhysicalCores = ci.physical
	if inv.PhysicalCores > inv.Cores && inv.Cores > 0 {
		inv.PhysicalCores = 0 // offline CPUs make the topology unreliable
	}
	inv.CPUMHz = cpuMHz(root, ci)
	inv.Virtualized = ci.flags["hypervisor"] || attr(rootPath(root, "sys/hypervisor/type")) != ""

	mem, memOK := readMemInfo(root)
	inv.MemTotalMB = kbToMB(mem.totalKB)
	inv.SwapTotalMB = kbToMB(mem.swapTotalKB)

	inv.Disks = collectDisks(root)
	for _, n := range scanNICs(root) {
		if live {
			n.Addrs = nicAddrs(n.Name)
		}
		inv.NICs = append(inv.NICs, n.NIC)
	}
	inv.Framebuffers = collectFramebuffers(root)
	inv.GPUs = collectGPUs(root)
	inv.Connectors = collectConnectors(root)

	dmi := readDMI(root)
	inv.Vendor, inv.Product, inv.BIOSDate = dmi.vendor, dmi.product, dmi.biosDate
	inv.HasBattery = summarizeBatteries(readSupplies(root)).present
	inv.IsLaptop = laptopChassis[dmi.chassisType] || inv.HasBattery

	t := pickCPUTemp([][]sensorReading{readHwmonCPU(root), readACPIZones(root)}, sensorReading.plausible)
	inv.TempSensor = t.chip

	if cpuErr != nil && !memOK {
		return inv, fmt.Errorf("hwinfo: %s has no readable proc/cpuinfo or proc/meminfo; is it a Linux root?", rootPath(root, ""))
	}
	return inv, nil
}

// kernelInfo returns the kernel release and uname machine. For the live
// system uname(2) is authoritative; fixture roots provide
// proc/sys/kernel/osrelease and proc/sys/kernel/arch. The machine falls back
// to the one matching this binary's GOARCH.
func kernelInfo(root string, live bool) (release, machine string) {
	if live {
		release, machine = uname()
	}
	if release == "" {
		release = attr(rootPath(root, "proc/sys/kernel/osrelease"))
	}
	if machine == "" {
		machine = attr(rootPath(root, "proc/sys/kernel/arch"))
	}
	if machine == "" {
		switch runtime.GOARCH {
		case "amd64":
			machine = "x86_64"
		case "386":
			machine = "i686"
		default:
			machine = runtime.GOARCH
		}
	}
	return release, machine
}

// osVersion reads /etc/savior-release: the VERSION= value when the file is
// in os-release style, else its first non-empty line.
func osVersion(root string) string {
	b, err := readFileLimit(rootPath(root, "etc/savior-release"), 4096)
	if err != nil {
		return ""
	}
	first := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "VERSION="); ok {
			return cleanVersion(strings.Trim(v, `"'`))
		}
		if first == "" && line != "" && !strings.Contains(line, "=") {
			first = line
		}
	}
	return cleanVersion(first)
}

func cleanVersion(s string) string {
	return strings.TrimSpace(proto.Sanitize(s, 64, false))
}
