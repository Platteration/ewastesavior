package hwinfo

import (
	"path/filepath"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// maxDisks caps the inventory's disk list.
const maxDisks = 64

// skippedBlockPrefixes are block devices that are not disks worth reporting:
// loop and RAM disks, zram swap, device-mapper, optical, floppy, network
// block devices, md RAID and MTD.
var skippedBlockPrefixes = []string{"loop", "ram", "zram", "dm-", "sr", "fd", "nbd", "md", "mtdblock"}

// collectDisks lists whole disks from /sys/block. Sizes are MiB.
func collectDisks(root string) []proto.Disk {
	var out []proto.Disk
	for _, name := range listDir(rootPath(root, "sys/block")) {
		if skipBlock(name) {
			continue
		}
		dir := rootPath(root, "sys/block/"+name)
		sectors, ok := readInt(filepath.Join(dir, "size"))
		if !ok || sectors <= 0 {
			continue // card readers without media, dead devices
		}
		d := proto.Disk{
			Name:       name,
			SizeMB:     sectors / 2048, // size is always in 512-byte sectors
			Rotational: attr(filepath.Join(dir, "queue/rotational")) == "1",
			Removable:  attr(filepath.Join(dir, "removable")) == "1",
			Transport:  diskTransport(root, dir, name),
		}
		d.Model = collapseSpace(attr(filepath.Join(dir, "device/model")))
		if d.Model == "" {
			d.Model = collapseSpace(attr(filepath.Join(dir, "device/name"))) // mmc
		}
		out = append(out, d)
		if len(out) == maxDisks {
			break
		}
	}
	return out
}

func skipBlock(name string) bool {
	for _, p := range skippedBlockPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// diskTransport derives the bus from the device's sysfs path
// (/sys/block/sda -> ../devices/pci0000:00/.../usb1/1-5/...), falling back to
// the name for NVMe, MMC and virtio.
func diskTransport(root, dir, name string) string {
	if rel, ok := devicesRel(root, dir); ok {
		if t := transportFromPath(rel); t != "" {
			return t
		}
	}
	switch {
	case strings.HasPrefix(name, "nvme"):
		return "nvme"
	case strings.HasPrefix(name, "mmcblk"):
		return "mmc"
	case strings.HasPrefix(name, "vd"):
		return "virtio"
	case strings.HasPrefix(name, "hd"):
		return "ata"
	}
	return ""
}

// transportFromPath classifies a resolved sysfs device path. USB is checked
// first because USB controllers themselves sit on PCI.
func transportFromPath(p string) string {
	segs := strings.Split(filepath.ToSlash(p), "/")
	has := func(prefix string, digits bool) bool {
		for _, s := range segs {
			if digits {
				if _, ok := numberedName(s, prefix); ok {
					return true
				}
			} else if strings.HasPrefix(s, prefix) {
				return true
			}
		}
		return false
	}
	switch {
	case has("usb", true):
		return "usb"
	case has("nvme", false):
		return "nvme"
	case has("mmc_host", false), has("mmc", true):
		return "mmc"
	case has("virtio", true):
		return "virtio"
	case has("ata", true):
		return "ata"
	}
	return ""
}
