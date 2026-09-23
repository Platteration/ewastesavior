package hwinfo

import (
	"net"
	"path"
	"strconv"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// maxNICs caps the number of interfaces looked at.
const maxNICs = 64

// arphrdEther is /sys/class/net/X/type for Ethernet and Wi-Fi.
const arphrdEther = 1

// nicInfo is a network interface plus what identity selection needs.
type nicInfo struct {
	proto.NIC
	assignType int    // addr_assign_type; -1 = unreadable
	busAddr    string // PCI address or USB interface of the device; sort key
}

// scanNICs lists Ethernet-type interfaces (wired and wireless) other than
// loopback, in natural name order.
func scanNICs(root string) []nicInfo {
	var out []nicInfo
	for _, name := range listDir(rootPath(root, "sys/class/net")) {
		dir := "sys/class/net/" + name
		if name == "lo" || !isDir(rootPath(root, dir)) {
			continue // loopback, bonding_masters
		}
		if t, ok := readInt(rootPath(root, dir+"/type")); ok && t != arphrdEther {
			continue // tunnels, loopback-likes, CAN ...
		}
		n := nicInfo{assignType: -1}
		n.Name = name
		n.MAC = strings.ToLower(attr(rootPath(root, dir+"/address")))
		n.Wireless = exists(rootPath(root, dir+"/wireless")) || exists(rootPath(root, dir+"/phy80211"))
		if v, ok := readInt(rootPath(root, dir+"/addr_assign_type")); ok {
			n.assignType = int(v)
		}
		n.Bus, n.busAddr, n.Driver = nicDevice(root, dir+"/device")
		oper := attr(rootPath(root, dir+"/operstate"))
		n.Carrier = attr(rootPath(root, dir+"/carrier")) == "1"
		n.Up = oper == "up" || (oper == "unknown" && n.Carrier)
		if n.Up {
			if s, ok := readInt(rootPath(root, dir+"/speed")); ok && s > 0 && s < 1_000_000 {
				n.SpeedMb = int(s)
			}
		}
		out = append(out, n)
		if len(out) == maxNICs {
			break
		}
	}
	return out
}

// nicDevice classifies the device behind /sys/class/net/X/device. It walks
// from the device towards the root until a device on the pci or usb bus is
// found, so virtio-net (virtioN below a PCI function) counts as pci with the
// PCI function's address. Interfaces without a device are "virtual".
func nicDevice(root, devLink string) (bus, addr, driver string) {
	p := rootPath(root, devLink)
	if !exists(p) {
		return "virtual", "", ""
	}
	driver = linkBase(p + "/driver")
	addr = linkBase(p)
	rel, ok := devicesRel(root, p)
	if !ok {
		// Unresolvable link: trust the device's own subsystem link.
		if sub := linkBase(p + "/subsystem"); sub == "pci" || sub == "usb" {
			return sub, addr, driver
		}
		return "", addr, driver
	}
	for i := 0; i < 8 && rel != "." && rel != "" && rel != "/"; i++ {
		if sub := linkBase(rootPath(root, "sys/devices/"+rel+"/subsystem")); sub == "pci" || sub == "usb" {
			return sub, path.Base(rel), driver
		}
		rel = path.Dir(rel)
	}
	return "", addr, driver
}

// nicAddrs returns the interface's addresses as CIDR strings, skipping IPv6
// link-local ones. Only meaningful for the live system.
func nicAddrs(name string) []string {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || (ipn.IP.To4() == nil && ipn.IP.IsLinkLocalUnicast()) {
			continue
		}
		out = append(out, ipn.String())
	}
	return out
}

// parseMAC parses "00:11:22:33:44:55" into 6 bytes.
func parseMAC(s string) ([6]byte, bool) {
	var m [6]byte
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return m, false
	}
	for i, p := range parts {
		if len(p) != 2 {
			return m, false
		}
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return m, false
		}
		m[i] = byte(v)
	}
	return m, true
}

// usableMAC reports whether mac is a burned-in unicast address: not zero,
// not broadcast or multicast, not locally administered.
func usableMAC(mac string) bool {
	m, ok := parseMAC(mac)
	if !ok {
		return false
	}
	if m == [6]byte{} || m[0]&0x01 != 0 || m[0]&0x02 != 0 {
		return false // zero, multicast/broadcast, locally administered
	}
	return true
}

// macHex returns the MAC as 12 lowercase hex digits.
func macHex(mac string) string {
	return strings.ReplaceAll(strings.ToLower(mac), ":", "")
}
