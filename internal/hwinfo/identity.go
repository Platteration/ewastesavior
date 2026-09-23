package hwinfo

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Identity is a node's hardware-derived identity (DESIGN 10.1).
type Identity struct {
	// NodeID is "n" + the first 12 hex digits of SHA-256(Source).
	NodeID string `json:"node_id"`
	// MAC is the MAC address the ID was derived from ("00:16:41:e3:4a:7b"),
	// or "" when the ID came from DMI or is random.
	MAC string `json:"mac,omitempty"`
	// Source is the identity source string: "mac:001641e34a7b",
	// "uuid:<product_uuid>", "serial:<serial>" or "random:<hex>".
	Source string `json:"source"`
	// HWIDs lists every valid hardware candidate in source order and the
	// same "kind:value" form, so the hive can recognize the machine after
	// its primary source changes (a NIC swapped, a BIOS update).
	HWIDs []string `json:"hw_ids"`
	// Stable is false when no hardware source was found and the ID is random
	// (a new ID on every agent start).
	Stable bool `json:"stable"`
}

// nicCandidate is a MAC usable as identity.
type nicCandidate struct {
	mac      string
	wireless bool
	busAddr  string
}

// GetIdentity derives the node identity from the hardware below root. The
// source order is: PCI NICs (wired first, then by PCI address) with a
// permanent (addr_assign_type 0), globally administered unicast MAC; a valid
// DMI product_uuid; a valid DMI product_serial or board_serial; a USB NIC
// MAC with the same rules; and finally a random ID (Stable=false, logged).
// It fails only when the live system is read on a non-Linux OS.
func GetIdentity(root string) (Identity, error) {
	if isLive(root) {
		if err := liveSupported(); err != nil {
			return Identity{}, err
		}
	}
	var pciNICs, usbNICs []nicCandidate
	for _, n := range scanNICs(root) {
		if n.assignType != 0 || !usableMAC(n.MAC) {
			continue
		}
		c := nicCandidate{mac: n.MAC, wireless: n.Wireless, busAddr: n.busAddr}
		switch n.Bus {
		case "pci":
			pciNICs = append(pciNICs, c)
		case "usb":
			usbNICs = append(usbNICs, c)
		}
	}
	sortCandidates(pciNICs)
	sortCandidates(usbNICs)
	dmi := readDMI(root)

	id := Identity{HWIDs: []string{}}
	add := func(src string) {
		if !contains(id.HWIDs, src) {
			id.HWIDs = append(id.HWIDs, src)
		}
	}
	for _, c := range pciNICs {
		add("mac:" + macHex(c.mac))
	}
	if dmi.uuid != "" {
		add("uuid:" + dmi.uuid)
	}
	for _, s := range dmi.serials {
		add("serial:" + s)
	}
	for _, c := range usbNICs {
		add("mac:" + macHex(c.mac))
	}

	switch {
	case len(pciNICs) > 0:
		id.MAC, id.Source = pciNICs[0].mac, "mac:"+macHex(pciNICs[0].mac)
	case dmi.uuid != "":
		id.Source = "uuid:" + dmi.uuid
	case len(dmi.serials) > 0:
		id.Source = "serial:" + dmi.serials[0]
	case len(usbNICs) > 0:
		id.MAC, id.Source = usbNICs[0].mac, "mac:"+macHex(usbNICs[0].mac)
	}
	if id.Source != "" {
		id.Stable = true
	} else {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Identity{}, fmt.Errorf("hwinfo: random node id: %w", err)
		}
		id.Source = "random:" + hex.EncodeToString(b[:])
		slog.Warn("no stable hardware identity (no burned-in NIC MAC, DMI UUID or serial); using a random node id that changes on every start",
			"root", root)
	}
	id.NodeID = NodeIDFromSource(id.Source)
	return id, nil
}

// NodeIDFromSource returns "n" + the first 12 hex digits of SHA-256(src).
func NodeIDFromSource(src string) string {
	sum := sha256.Sum256([]byte(src))
	return "n" + hex.EncodeToString(sum[:6])
}

// sortCandidates orders wired NICs before wireless ones, then by bus
// address.
func sortCandidates(c []nicCandidate) {
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].wireless != c[j].wireless {
			return !c[i].wireless
		}
		return c[i].busAddr < c[j].busAddr
	})
}

// DefaultName returns the node's default name: "savior-" + the last 6 hex
// digits of the identity MAC, or of the node ID when there is none.
func DefaultName(id Identity) string {
	src := macHex(id.MAC)
	if src == "" {
		src = strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' {
				return r
			}
			return -1
		}, strings.ToLower(id.NodeID))
	}
	if len(src) > 6 {
		src = src[len(src)-6:]
	}
	return "savior-" + src
}

// BootID returns /proc/sys/kernel/random/boot_id, or "" when unreadable.
func BootID(root string) string {
	return attr(rootPath(root, "proc/sys/kernel/random/boot_id"))
}
