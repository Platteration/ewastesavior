package hwinfo

import (
	"reflect"
	"sort"
	"testing"
)

func TestParseCPUInfo(t *testing.T) {
	cases := []struct {
		name     string
		data     string
		logical  int
		physical int
		model    string
		flags    []string
		mhz      float64
	}{
		{"empty", "", 0, 0, "", nil, 0},
		{
			"uniprocessor without topology",
			"processor\t: 0\nvendor_id\t: CentaurHauls\nmodel name\t: VIA Nehemiah\ncpu MHz\t\t: 999.600\nflags\t\t: fpu tsc sse\n",
			1, 1, "VIA Nehemiah", []string{"sse"}, 999.6,
		},
		{
			"two CPUs without topology",
			"processor : 0\nflags : sse sse2\n\nprocessor : 1\nflags : sse sse2\n",
			2, 0, "", []string{"sse", "sse2"}, 0,
		},
		{
			"cpu cores without core id",
			"processor : 0\nphysical id : 0\ncpu cores : 2\n\nprocessor : 1\nphysical id : 0\ncpu cores : 2\n",
			2, 2, "", nil, 0,
		},
		{
			"two sockets, HT",
			"processor : 0\nphysical id : 0\ncore id : 0\nprocessor : 1\nphysical id : 0\ncore id : 0\n" +
				"processor : 2\nphysical id : 1\ncore id : 0\nprocessor : 3\nphysical id : 1\ncore id : 0\n",
			4, 2, "", nil, 0,
		},
		{
			"pni reported as sse3, highest MHz kept, junk ignored",
			"processor : 0\nmodel name :   Mobile   AMD  Sempron(tm)  \ncpu MHz : 800.0\nflags : pni lm nx 3dnow\n" +
				"processor : 1\ncpu MHz : 1800.4\ncpu MHz : banana\ncpu cores : -1\nno colon here\n",
			2, 0, "Mobile AMD Sempron(tm)", []string{"lm", "nx", "sse3"}, 1800.4,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ci := parseCPUInfo(c.data)
			flags := cpuFlagSubset(ci.flags)
			sort.Strings(flags)
			if ci.logical != c.logical || ci.physical != c.physical || ci.model != c.model || ci.maxMHz != c.mhz ||
				!reflect.DeepEqual(flags, c.flags) {
				t.Fatalf("got %+v (flags %v)", ci, flags)
			}
		})
	}
}

func TestCPUFlagSubsetOrder(t *testing.T) {
	flags := map[string]bool{}
	for _, f := range ReportedCPUFlags {
		flags[f] = true
	}
	flags["mmx"] = true
	if got := cpuFlagSubset(flags); !reflect.DeepEqual(got, ReportedCPUFlags) {
		t.Fatalf("cpuFlagSubset = %v", got)
	}
}

func TestParseCPUList(t *testing.T) {
	cases := []struct {
		in string
		n  int
		ok bool
	}{
		{"0", 1, true},
		{"0-3\n", 4, true},
		{"0-1,4,6-7", 5, true},
		{"", 0, false},
		{"3-1", 0, false},
		{"a", 0, false},
		{"0-", 0, false},
		{"-1", 0, false},
		{"0-99999999", 0, false},
	}
	for _, c := range cases {
		n, ok := parseCPUList(c.in)
		if n != c.n || ok != c.ok {
			t.Errorf("parseCPUList(%q) = %d, %v; want %d, %v", c.in, n, ok, c.n, c.ok)
		}
	}
}

func TestParseMemInfo(t *testing.T) {
	cases := []struct {
		name                       string
		data                       string
		total, avail, swapT, swapF int64
		ok                         bool
	}{
		{"modern", "MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 600 kB\nSwapTotal: 50 kB\nSwapFree: 20 kB\n",
			1000, 600, 50, 20, true},
		{"pre-3.14 kernel estimates MemAvailable", "MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 200 kB\n",
			1000, 350, 0, 0, true},
		{"available clamped to total", "MemTotal: 1000 kB\nMemAvailable: 5000 kB\n", 1000, 1000, 0, 0, true},
		{"swap free clamped", "MemTotal: 1 kB\nSwapTotal: 10 kB\nSwapFree: 20 kB\n", 1, 0, 10, 10, true},
		{"garbage", "MemTotal: lots\nMemFree -5 kB\nnonsense\n", 0, 0, 0, 0, false},
		{"empty", "", 0, 0, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := parseMemInfo(c.data)
			if m.totalKB != c.total || m.availKB != c.avail || m.swapTotalKB != c.swapT || m.swapFreeKB != c.swapF || m.ok != c.ok {
				t.Fatalf("got %+v", m)
			}
		})
	}
}

func TestMemFunctions(t *testing.T) {
	root := fixture(t, "p4-desktop")
	if got := MemTotalMB(root); got != 497 {
		t.Errorf("MemTotalMB = %d", got)
	}
	if got := MemAvailableMB(root); got != 391 {
		t.Errorf("MemAvailableMB = %d", got)
	}
	empty := t.TempDir()
	if MemTotalMB(empty) != 0 || MemAvailableMB(empty) != 0 {
		t.Error("missing meminfo should read as 0")
	}
}

func TestNaturalLess(t *testing.T) {
	in := []string{"fb10", "fb2", "card0-VGA-1", "card0", "card10", "card1", "hwmon9", "hwmon10", "a", "", "a0", "a00", "sdb", "sda"}
	sort.Slice(in, func(i, j int) bool { return naturalLess(in[i], in[j]) })
	want := []string{"", "a", "a0", "a00", "card0", "card0-VGA-1", "card1", "card10", "fb2", "fb10", "hwmon9", "hwmon10", "sda", "sdb"}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("natural order = %q", in)
	}
}

func TestNumberedName(t *testing.T) {
	cases := []struct {
		name, prefix string
		n            int
		ok           bool
	}{
		{"cpu0", "cpu", 0, true},
		{"cpu127", "cpu", 127, true},
		{"cpufreq", "cpu", 0, false},
		{"cpu", "cpu", 0, false},
		{"cpu+1", "cpu", 0, false},
		{"cpu-1", "cpu", 0, false},
		{"cpu1234567", "cpu", 0, false},
		{"fb0", "cpu", 0, false},
	}
	for _, c := range cases {
		n, ok := numberedName(c.name, c.prefix)
		if n != c.n || ok != c.ok {
			t.Errorf("numberedName(%q, %q) = %d, %v", c.name, c.prefix, n, ok)
		}
	}
}

func TestTransportFromPath(t *testing.T) {
	cases := map[string]string{
		"pci0000_00/0000_00_1f.2/ata1/host0/target0_0_0/0_0_0_0/block/sda":             "ata",
		"pci0000_00/0000_00_1d.7/usb1/1-5/1-5_1.0/host4/target4_0_0/4_0_0_0/block/sdb": "usb",
		"pci0000_00/0000_00_1c.0/0000_02_00.0/nvme/nvme0/nvme0n1":                      "nvme",
		"pci0000_00/0000_00_1e.0/0000_04_00.0/mmc_host/mmc0/mmc0_e624/block/mmcblk0":   "mmc",
		"pci0000_00/0000_00_04.0/virtio1/block/vda":                                    "virtio",
		"pci0000_00/0000_00_10.0/host2/target2_0_0/2_0_0_0/block/sdc":                  "",
		"virtual/block/loop0": "",
	}
	for p, want := range cases {
		if got := transportFromPath(p); got != want {
			t.Errorf("transportFromPath(%q) = %q, want %q", p, got, want)
		}
	}
}

// Without a resolvable sysfs path the transport comes from the name.
func TestDiskTransportByName(t *testing.T) {
	root := t.TempDir()
	for name, size := range map[string]string{"hda": "80", "mmcblk1": "80", "vdb": "80", "nvme1n1": "80", "sdq": "80"} {
		writeFile(t, root, "sys/block/"+name+"/size", size+"\n")
	}
	got := map[string]string{}
	for _, d := range collectDisks(root) {
		got[d.Name] = d.Transport
	}
	want := map[string]string{"hda": "ata", "mmcblk1": "mmc", "vdb": "virtio", "nvme1n1": "nvme", "sdq": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transports = %v, want %v", got, want)
	}
}

func TestNICDeviceFallbacks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "sys/devices/.keep", "")
	// A device link that dangles: only the name is known.
	symlink(t, root, "sys/class/net/eth0/device", "../../../devices/pci0000_00/0000_00_19.0")
	// A device directory outside sys/devices (hand-made tree): trust its own
	// subsystem link.
	writeFile(t, root, "sys/class/net/eth1/device/vendor", "0x8086\n")
	symlink(t, root, "sys/class/net/eth1/device/subsystem", "../../../../bus/usb")
	symlink(t, root, "sys/class/net/eth1/device/driver", "../../../../bus/usb/drivers/r8152")
	writeFile(t, root, "sys/class/net/eth2/device/vendor", "0x8086\n")
	// A device on some other bus with no PCI or USB parent (platform NIC).
	writeFile(t, root, "sys/devices/platform/eth3dev/uevent", "")
	symlink(t, root, "sys/devices/platform/eth3dev/subsystem", "../../../bus/platform")
	symlink(t, root, "sys/class/net/eth3/device", "../../../devices/platform/eth3dev")

	cases := []struct{ nic, bus, addr, driver string }{
		{"eth0", "", "0000_00_19.0", ""},
		{"eth1", "usb", "", "r8152"},
		{"eth2", "", "", ""},
		{"eth3", "", "eth3dev", ""},
	}
	for _, c := range cases {
		bus, addr, driver := nicDevice(root, "sys/class/net/"+c.nic+"/device")
		if bus != c.bus || addr != c.addr || driver != c.driver {
			t.Errorf("nicDevice(%s) = %q, %q, %q; want %q, %q, %q", c.nic, bus, addr, driver, c.bus, c.addr, c.driver)
		}
	}
}
