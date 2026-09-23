package storage

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// BlockDev is one block device (whole disk or partition) from sysfs.
type BlockDev struct {
	Name      string // kernel name: sdb1
	Path      string // device node: <sys-root>/dev/sdb1
	Disk      string // whole-disk name: sdb
	Partition int    // partition number, 0 for a whole disk
	Start     uint64 // partition start in 512-byte units (sysfs "start")
	Size      int64  // bytes
	Removable bool   // removable flag set on the disk
	USB       bool   // the disk hangs off a USB bus
	CDROM     bool   // optical drive (sr*)
	ReadOnly  bool
}

// Portable reports whether the device is in the "removable" preference
// group: removable media, USB, optical drives and SD/MMC cards.
func (d BlockDev) Portable() bool { return d.Removable || d.USB || d.CDROM }

func (d BlockDev) String() string {
	var tags []string
	if d.Removable {
		tags = append(tags, "removable")
	}
	if d.USB {
		tags = append(tags, "usb")
	}
	if d.CDROM {
		tags = append(tags, "cdrom")
	}
	if len(tags) == 0 {
		tags = append(tags, "fixed")
	}
	return fmt.Sprintf("%s (%s, %d MiB)", d.Name, strings.Join(tags, ","), d.Size>>20)
}

// skipPrefixes are block devices never considered as config media.
var skipPrefixes = []string{"loop", "ram", "zram", "dm-", "md", "nbd", "mtdblock", "fd", "sg"}

func skipDevice(name string) bool {
	for _, p := range skipPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ListBlockDevices returns the block devices under <sysRoot>/sys/class/block
// in search order: removable, USB, optical and SD/MMC devices first, fixed
// disks last; within a group by disk name, whole disks before their
// partitions. Loop, RAM, zram, device-mapper, md, nbd and floppy devices and
// devices with no medium (size 0) are left out.
func ListBlockDevices(sysRoot string) ([]BlockDev, error) {
	classDir := filepath.Join(sysRoot, "sys", "class", "block")
	entries, err := os.ReadDir(classDir)
	if err != nil {
		return nil, err
	}
	var out []BlockDev
	for _, e := range entries {
		name := e.Name()
		if skipDevice(name) {
			continue
		}
		d, err := readBlockDev(sysRoot, name)
		if err != nil || d.Size == 0 {
			continue
		}
		out = append(out, d)
	}
	SortBlockDevices(out)
	return out, nil
}

// SortBlockDevices sorts devices in search order (see ListBlockDevices).
func SortBlockDevices(devs []BlockDev) {
	sort.SliceStable(devs, func(i, j int) bool {
		a, b := devs[i], devs[j]
		if a.Portable() != b.Portable() {
			return a.Portable()
		}
		if a.CDROM != b.CDROM { // sticks before optical drives
			return !a.CDROM
		}
		if a.Disk != b.Disk {
			if len(a.Disk) != len(b.Disk) && strings.TrimRight(a.Disk, "0123456789") == strings.TrimRight(b.Disk, "0123456789") {
				return len(a.Disk) < len(b.Disk) // sdb2 < sdb10 style ordering for disks like mmcblk10
			}
			return a.Disk < b.Disk
		}
		return a.Partition < b.Partition
	})
}

// readBlockDev reads one /sys/class/block entry.
func readBlockDev(sysRoot, name string) (BlockDev, error) {
	link := filepath.Join(sysRoot, "sys", "class", "block", name)
	dir, err := filepath.EvalSymlinks(link)
	if err != nil {
		return BlockDev{}, err
	}
	d := BlockDev{Name: name, Path: filepath.Join(sysRoot, "dev", name), Disk: name}
	diskDir := dir
	if n, ok := readUint(filepath.Join(dir, "partition")); ok {
		d.Partition = int(n)
		diskDir = filepath.Dir(dir)
		d.Disk = filepath.Base(diskDir)
		d.Start, _ = readUint(filepath.Join(dir, "start"))
	}
	if n, ok := readUint(filepath.Join(dir, "size")); ok {
		d.Size = int64(n) * 512
	}
	if n, ok := readUint(filepath.Join(dir, "ro")); ok {
		d.ReadOnly = n != 0
	}
	if n, ok := readUint(filepath.Join(diskDir, "removable")); ok {
		d.Removable = n != 0
	}
	if strings.HasPrefix(d.Disk, "mmcblk") {
		d.Removable = true // SD/MMC readers report removable=0
	}
	if strings.HasPrefix(d.Disk, "sr") {
		d.CDROM = true
	} else if n, ok := readUint(filepath.Join(diskDir, "device", "type")); ok && n == 5 {
		d.CDROM = true // SCSI peripheral type 5: CD/DVD
	}
	// A USB disk's sysfs path runs through .../usbN/N-M/...
	rel := filepath.ToSlash(diskDir)
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, "usb") && len(part) > 3 && part[3] >= '0' && part[3] <= '9' {
			d.USB = true
			break
		}
	}
	return d, nil
}

// diskPartitions lists the partitions of a whole disk from sysfs.
func diskPartitions(sysRoot, disk string) ([]BlockDev, error) {
	dir, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "sys", "class", "block", disk))
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []BlockDev
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), disk) {
			continue
		}
		if _, ok := readUint(filepath.Join(dir, e.Name(), "partition")); !ok {
			continue
		}
		d, err := readBlockDev(sysRoot, e.Name())
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Partition < out[j].Partition })
	return out, nil
}

// partitionName returns the kernel name of partition n of disk: sdb2,
// mmcblk0p2, nvme0n1p2, loop0p2.
func partitionName(disk string, n int) string {
	if disk != "" && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

// blockDevByNumber resolves a "major:minor" pair to a block device name via
// /sys/dev/block.
func blockDevByNumber(sysRoot, majMin string) (string, error) {
	dir, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "sys", "dev", "block", majMin))
	if err != nil {
		return "", err
	}
	return filepath.Base(dir), nil
}

func readUint(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}

// MountEntry is one line of /proc/self/mountinfo.
type MountEntry struct {
	MajMin     string
	MountPoint string
	FSType     string
	Source     string
	Options    string
}

// ReadMountInfo parses a mountinfo file.
func ReadMountInfo(path string) ([]MountEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []MountEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if e, ok := parseMountInfoLine(sc.Text()); ok {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// parseMountInfoLine parses
// "36 35 8:17 / /media/savior ro,nosuid - vfat /dev/sdb1 ro,fmask=0177".
func parseMountInfoLine(line string) (MountEntry, bool) {
	pre, post, ok := strings.Cut(line, " - ")
	if !ok {
		return MountEntry{}, false
	}
	f := strings.Fields(pre)
	g := strings.Fields(post)
	if len(f) < 6 || len(g) < 2 {
		return MountEntry{}, false
	}
	e := MountEntry{MajMin: f[2], MountPoint: unescapeMount(f[4]), Options: f[5], FSType: g[0], Source: unescapeMount(g[1])}
	return e, true
}

// unescapeMount decodes the octal escapes (\040 etc.) used in mount tables.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// findMount returns the mount entry for mountpoint (the last one wins, as
// with stacked mounts).
func findMount(entries []MountEntry, mountpoint string) (MountEntry, bool) {
	mp := filepath.Clean(mountpoint)
	var found MountEntry
	ok := false
	for _, e := range entries {
		if e.MountPoint == mp {
			found, ok = e, true
		}
	}
	return found, ok
}
