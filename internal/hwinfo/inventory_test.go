package hwinfo

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

func expectedInventories() map[string]proto.Inventory {
	return map[string]proto.Inventory{
		"thinkpad-t60": {
			Hostname: "savior", Arch: runtime.GOARCH, MachineArch: "i686", Kernel: "6.6.52-savior",
			OSVersion: "SaviorOS v0.2.0",
			CPUModel:  "Genuine Intel(R) CPU T2400 @ 1.83GHz", CPUVendor: "GenuineIntel",
			CPUFlags: []string{"pae", "nx", "sse", "sse2", "sse3", "vmx"},
			Cores:    2, PhysicalCores: 2, CPUMHz: 1833, MemTotalMB: 2011, SwapTotalMB: 1005,
			Disks: []proto.Disk{{Name: "sda", SizeMB: 57231, Model: "HTS541060G9AT00", Rotational: true, Transport: "ata"}},
			NICs: []proto.NIC{
				{Name: "eth0", MAC: "00:16:41:e3:4a:7b", Bus: "pci", Driver: "e1000e", SpeedMb: 1000, Up: true, Carrier: true},
				{Name: "wlan0", MAC: "00:19:d2:5c:31:0e", Wireless: true, Bus: "pci", Driver: "iwl3945"},
			},
			Framebuffers: []proto.FB{{Name: "fb0", Driver: "i915drmfb", Width: 1024, Height: 768, BPP: 32}},
			GPUs:         []proto.GPU{{Card: "card0", Driver: "i915", Vendor: "0x8086", Device: "0x27a2"}},
			Connectors: []proto.Connector{
				{Name: "LVDS-1", Card: "card0", Status: "connected", Enabled: true, Preferred: "1024x768", WidthMM: 285, HeightMM: 214},
				{Name: "VGA-1", Card: "card0", Status: "disconnected"},
			},
			HasBattery: true, IsLaptop: true, Vendor: "LENOVO", Product: "ThinkPad T60 (2007FVG)",
			BIOSDate: "10/23/2006", TempSensor: "coretemp",
		},
		"p4-desktop": {
			Hostname: "savior", Arch: runtime.GOARCH, MachineArch: "i686", Kernel: "6.6.52-savior", OSVersion: "v0.2.0",
			CPUModel: "Intel(R) Pentium(R) 4 CPU 2.80GHz", CPUVendor: "GenuineIntel",
			CPUFlags: []string{"pae", "sse", "sse2"},
			Cores:    2, PhysicalCores: 1, CPUMHz: 2793, MemTotalMB: 497, SwapTotalMB: 248,
			Disks: []proto.Disk{{Name: "sda", SizeMB: 38166, Model: "ST340014A", Rotational: true, Transport: "ata"}},
			NICs: []proto.NIC{
				{Name: "eth0", MAC: "00:0e:a6:3b:12:9f", Bus: "pci", Driver: "8139too", SpeedMb: 100, Up: true, Carrier: true},
			},
			Framebuffers: []proto.FB{{Name: "fb0", Driver: "radeondrmfb", Width: 1280, Height: 1024, BPP: 32}},
			GPUs:         []proto.GPU{{Card: "card0", Driver: "radeon", Vendor: "0x1002", Device: "0x5b60"}},
			Connectors: []proto.Connector{
				{Name: "DVI-I-1", Card: "card0", Status: "disconnected"},
				{Name: "VGA-1", Card: "card0", Status: "connected", Enabled: true, Preferred: "1280x1024", WidthMM: 338, HeightMM: 270},
			},
			Vendor: "ASRock", Product: "P4i65G", BIOSDate: "04/15/2005", TempSensor: "acpitz",
		},
		"atom-netbook": {
			Hostname: "netbook", Arch: runtime.GOARCH, MachineArch: "i686", Kernel: "6.6.52-savior",
			CPUModel: "Intel(R) Atom(TM) CPU N270 @ 1.60GHz", CPUVendor: "GenuineIntel",
			CPUFlags: []string{"pae", "nx", "sse", "sse2", "sse3", "ssse3"},
			Cores:    2, PhysicalCores: 1, CPUMHz: 1600, MemTotalMB: 992, SwapTotalMB: 496,
			Disks: []proto.Disk{
				{Name: "mmcblk0", SizeMB: 3781, Model: "SU04G", Transport: "mmc"},
				{Name: "sda", SizeMB: 7695, Model: "SSDPAMM0008G1", Transport: "ata"},
				{Name: "sdb", SizeMB: 3819, Model: "Cruzer Blade", Rotational: true, Removable: true, Transport: "usb"},
			},
			NICs: []proto.NIC{
				{Name: "eth0", MAC: "00:1e:68:a1:b2:c3", Bus: "pci", Driver: "r8169"},
				{Name: "wlan0", MAC: "00:22:5f:d4:e5:f6", Wireless: true, Bus: "pci", Driver: "ath5k", Up: true, Carrier: true},
			},
			Framebuffers: []proto.FB{{Name: "fb0", Driver: "i915drmfb", Width: 1024, Height: 600, BPP: 32}},
			GPUs:         []proto.GPU{{Card: "card0", Driver: "i915", Vendor: "0x8086", Device: "0x27ae"}},
			Connectors: []proto.Connector{
				{Name: "LVDS-1", Card: "card0", Status: "connected", Enabled: true, Preferred: "1024x600", WidthMM: 196, HeightMM: 114},
				{Name: "VGA-1", Card: "card0", Status: "disconnected"},
			},
			HasBattery: true, IsLaptop: true, Vendor: "Acer", Product: "AOA150", BIOSDate: "10/13/2008",
			TempSensor: "coretemp",
		},
		"amd-desktop": {
			Hostname: "savior", Arch: runtime.GOARCH, MachineArch: "x86_64", Kernel: "6.6.52-savior",
			OSVersion: "SaviorOS v0.2.0",
			CPUModel:  "AMD Athlon(tm) II X2 250 Processor", CPUVendor: "AuthenticAMD",
			CPUFlags: []string{"lm", "pae", "nx", "sse", "sse2", "sse3", "svm"},
			Cores:    2, PhysicalCores: 2, CPUMHz: 3000, MemTotalMB: 3833,
			Disks: []proto.Disk{{Name: "sda", SizeMB: 476940, Model: "WDC WD5000AAKS-00V1A0", Rotational: true, Transport: "ata"}},
			NICs: []proto.NIC{
				{Name: "eth0", MAC: "3a:7f:21:c9:04:5e", Bus: "pci", Driver: "forcedeth", SpeedMb: 1000, Up: true, Carrier: true},
				{Name: "eth1", MAC: "00:0e:c6:88:99:aa", Bus: "usb", Driver: "asix"},
			},
			Vendor: "Gigabyte Technology Co., Ltd.", Product: "GA-MA785GM-US2H", BIOSDate: "11/10/2009",
			TempSensor: "k10temp",
		},
		"qemu": {
			Hostname: "savior", Arch: runtime.GOARCH, MachineArch: "x86_64", Kernel: "6.6.52-savior",
			CPUModel: "QEMU Virtual CPU version 2.5+", CPUVendor: "AuthenticAMD",
			CPUFlags: []string{"lm", "pae", "nx", "sse", "sse2", "sse3", "svm", "hypervisor"},
			Cores:    2, PhysicalCores: 2, CPUMHz: 2394, MemTotalMB: 481,
			Disks: []proto.Disk{{Name: "vda", SizeMB: 8192, Rotational: true, Transport: "virtio"}},
			NICs: []proto.NIC{
				{Name: "eth0", MAC: "52:54:00:12:34:56", Bus: "pci", Driver: "virtio_net", Up: true, Carrier: true},
			},
			Framebuffers: []proto.FB{{Name: "fb0", Driver: "bochs-drmdrmfb", Width: 1024, Height: 768, BPP: 32}},
			GPUs:         []proto.GPU{{Card: "card0", Driver: "bochs-drm", Vendor: "0x1234", Device: "0x1111"}},
			Connectors: []proto.Connector{
				{Name: "Virtual-1", Card: "card0", Status: "connected", Enabled: true, Preferred: "1024x768", WidthMM: 260, HeightMM: 200},
			},
			Vendor: "QEMU", Product: "Standard PC (i440FX + PIIX, 1996)", BIOSDate: "04/01/2014", Virtualized: true,
		},
	}
}

func TestCollectFixtures(t *testing.T) {
	want := expectedInventories()
	for _, name := range machines {
		t.Run(name, func(t *testing.T) {
			got, err := Collect(fixture(t, name))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if !reflect.DeepEqual(got, want[name]) {
				diffInventory(t, got, want[name])
			}
		})
	}
}

// diffInventory reports the differing fields one by one.
func diffInventory(t *testing.T, got, want proto.Inventory) {
	t.Helper()
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := 0; i < gv.NumField(); i++ {
		if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
			t.Errorf("%s:\n got  %#v\n want %#v", gv.Type().Field(i).Name, gv.Field(i).Interface(), wv.Field(i).Interface())
		}
	}
}

// A root reached through a symlink (macOS /var -> /private/var, or an
// operator's --root link) must classify devices the same way.
func TestCollectThroughSymlinkedRoot(t *testing.T) {
	src := fixture(t, "atom-netbook")
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(src, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	got, err := Collect(link)
	if err != nil {
		t.Fatal(err)
	}
	want := expectedInventories()["atom-netbook"]
	if !reflect.DeepEqual(got.Disks, want.Disks) || !reflect.DeepEqual(got.NICs, want.NICs) {
		t.Fatalf("through symlink:\n disks %+v\n nics %+v", got.Disks, got.NICs)
	}
}

func TestCollectNotALinuxRoot(t *testing.T) {
	inv, err := Collect(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "Linux root") {
		t.Fatalf("err = %v, want not-a-Linux-root error", err)
	}
	if inv.Arch != runtime.GOARCH {
		t.Fatalf("partial inventory should still carry Arch, got %q", inv.Arch)
	}
}

func TestCollectEdgeCases(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "proc/meminfo", "MemTotal: 262144 kB\nMemFree: 1000 kB\nBuffers: 1000 kB\nCached: 2000 kB\n")
	// Uniprocessor Pentium III with a kernel that prints no topology fields.
	writeFile(t, root, "proc/cpuinfo", "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Pentium III (Coppermine)\ncpu MHz\t\t: 797.515\nflags\t\t: fpu vme pse tsc mmx fxsr sse\n\n")
	// Offline CPUs: sysfs knows better than cpuinfo.
	writeFile(t, root, "sys/devices/system/cpu/online", "0\n")
	writeFile(t, root, "sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq", "garbage\n")
	writeFile(t, root, "sys/hypervisor/type", "xen\n")
	writeFile(t, root, "sys/class/dmi/id/sys_vendor", "System manufacturer\n")
	writeFile(t, root, "sys/class/dmi/id/board_vendor", "ASUSTeK Computer INC.\n")
	writeFile(t, root, "sys/class/dmi/id/product_name", "System Product Name\n")
	writeFile(t, root, "sys/class/dmi/id/board_name", "P5GC-MX\n")
	writeFile(t, root, "sys/class/dmi/id/chassis_type", "9\n")
	writeFile(t, root, "sys/class/dmi/id/bios_date", "Not Specified\n")
	writeFile(t, root, "etc/savior-release", "\n  SaviorOS v0.3.0-rc1 \x07\n")
	// A bridge and a tunnel: the bridge is a virtual NIC, the tunnel is skipped.
	writeFile(t, root, "sys/class/net/br0/type", "1\n")
	writeFile(t, root, "sys/class/net/br0/address", "AA:BB:CC:00:11:22\n")
	writeFile(t, root, "sys/class/net/tun0/type", "65534\n")
	writeFile(t, root, "sys/class/net/bonding_masters", "\n")
	writeFile(t, root, "sys/class/net/eth9/type", "1\n")
	writeFile(t, root, "sys/class/net/eth9/operstate", "unknown\n")
	writeFile(t, root, "sys/class/net/eth9/carrier", "1\n")
	writeFile(t, root, "sys/class/net/eth9/speed", "-1\n")
	writeFile(t, root, "sys/block/sdz/size", "0\n") // card reader without media
	writeFile(t, root, "sys/block/nvme0n1/size", "2048\n")
	writeFile(t, root, "sys/block/nvme0n1/device/model", "  Some   NVMe  \n")
	writeFile(t, root, "sys/block/md0/size", "4096\n")
	writeFile(t, root, "sys/class/graphics/fb0/name", "VESA VGA\n")
	writeFile(t, root, "sys/class/graphics/fb0/virtual_size", "800,600,extra\n")
	writeFile(t, root, "sys/class/graphics/fb0/bits_per_pixel", "sixteen\n")

	inv, err := Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Cores", inv.Cores, 1},
		{"PhysicalCores", inv.PhysicalCores, 1},
		{"CPUMHz", inv.CPUMHz, 798},
		{"CPUFlags", inv.CPUFlags, []string{"sse"}},
		{"Virtualized", inv.Virtualized, true},
		{"Vendor", inv.Vendor, "ASUSTeK Computer INC."},
		{"Product", inv.Product, "P5GC-MX"},
		{"BIOSDate", inv.BIOSDate, ""},
		{"IsLaptop", inv.IsLaptop, true},
		{"HasBattery", inv.HasBattery, false},
		{"OSVersion", inv.OSVersion, "SaviorOS v0.3.0-rc1"},
		{"MemTotalMB", inv.MemTotalMB, 256},
		{"Disks", inv.Disks, []proto.Disk{{Name: "nvme0n1", SizeMB: 1, Model: "Some NVMe", Transport: "nvme"}}},
		{"NICs", inv.NICs, []proto.NIC{
			{Name: "br0", MAC: "aa:bb:cc:00:11:22", Bus: "virtual"},
			{Name: "eth9", Bus: "virtual", Up: true, Carrier: true},
		}},
		{"Framebuffers", inv.Framebuffers, []proto.FB{{Name: "fb0", Driver: "VESA VGA", Width: 800}}},
		{"TempSensor", inv.TempSensor, ""},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
}

func TestKernelInfoFallbacks(t *testing.T) {
	root := t.TempDir()
	rel, mach := kernelInfo(root, false)
	if rel != "" {
		t.Errorf("release = %q, want empty", rel)
	}
	want := map[string]string{"amd64": "x86_64", "386": "i686"}[runtime.GOARCH]
	if want == "" {
		want = runtime.GOARCH
	}
	if mach != want {
		t.Errorf("machine = %q, want %q", mach, want)
	}
}

func TestOSVersion(t *testing.T) {
	cases := []struct{ content, want string }{
		{"SaviorOS v0.2.0\n", "SaviorOS v0.2.0"},
		{"NAME=SaviorOS\nVERSION=\"v0.2.1\"\n", "v0.2.1"},
		{"NAME=SaviorOS\nVERSION='v0.2.2'\nBUILD=1\n", "v0.2.2"},
		{"NAME=SaviorOS\nBUILD_ID=5\n", ""},
		{"\n\n", ""},
		{strings.Repeat("x", 200), strings.Repeat("x", 64)},
	}
	for _, c := range cases {
		root := t.TempDir()
		writeFile(t, root, "etc/savior-release", c.content)
		if got := osVersion(root); got != c.want {
			t.Errorf("osVersion(%q) = %q, want %q", c.content, got, c.want)
		}
	}
	if got := osVersion(t.TempDir()); got != "" {
		t.Errorf("missing file: %q", got)
	}
}
