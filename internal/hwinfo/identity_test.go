package hwinfo

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

var nodeIDRE = regexp.MustCompile(`^n[0-9a-f]{12}$`)

func TestGetIdentityFixtures(t *testing.T) {
	cases := []struct {
		machine, nodeID, source, mac, name string
		hwids                              []string
	}{
		{"thinkpad-t60", "n6ead4f09ab5f", "mac:001641e34a7b", "00:16:41:e3:4a:7b", "savior-e34a7b", []string{
			"mac:001641e34a7b", "mac:0019d25c310e", "uuid:8a3c1e01-4b2f-11cb-9a8e-c5e1f0a3b2d4",
			"serial:l3a1b2c", "serial:vf1bc6al0ab",
		}},
		// Placeholder UUID and "To Be Filled By O.E.M." serials: only the NIC counts.
		{"p4-desktop", "n5f2d344a590d", "mac:000ea63b129f", "00:0e:a6:3b:12:9f", "savior-3b129f", []string{
			"mac:000ea63b129f",
		}},
		// Wired r8169 wins over the ath5k card with the lower PCI address.
		{"atom-netbook", "n46ef100f86f2", "mac:001e68a1b2c3", "00:1e:68:a1:b2:c3", "savior-a1b2c3", []string{
			"mac:001e68a1b2c3", "mac:00225fd4e5f6", "uuid:6b8d9f10-2a3c-4e5f-8071-92a3b4c5d6e7",
			"serial:lus050b0718320f4a92500",
		}},
		// forcedeth has a random MAC (addr_assign_type 1): the DMI UUID wins,
		// and the USB NIC comes last.
		{"amd-desktop", "n89c6cc3f885b", "uuid:4f6e2a10-8d3b-11de-8a39-0800200c9a66", "", "savior-3f885b", []string{
			"uuid:4f6e2a10-8d3b-11de-8a39-0800200c9a66", "serial:sn0912a0034512", "mac:000ec68899aa",
		}},
	}
	for _, c := range cases {
		t.Run(c.machine, func(t *testing.T) {
			id, err := GetIdentity(fixture(t, c.machine))
			if err != nil {
				t.Fatal(err)
			}
			want := Identity{NodeID: c.nodeID, MAC: c.mac, Source: c.source, HWIDs: c.hwids, Stable: true}
			if !reflect.DeepEqual(id, want) {
				t.Fatalf("got  %+v\nwant %+v", id, want)
			}
			if got := DefaultName(id); got != c.name {
				t.Errorf("DefaultName = %q, want %q", got, c.name)
			}
			if !proto.ValidNodeID(id.NodeID) || !proto.ValidNodeName(DefaultName(id)) {
				t.Errorf("invalid node id %q or name %q", id.NodeID, DefaultName(id))
			}
		})
	}
}

// QEMU's 52:54:00 OUI is locally administered and the VM has an all-zero
// UUID and no serial: nothing is stable, so the ID is random.
func TestGetIdentityQEMUIsRandom(t *testing.T) {
	root := fixture(t, "qemu")
	a, err := GetIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GetIdentity(root)
	if a.Stable || a.MAC != "" || len(a.HWIDs) != 0 || a.HWIDs == nil {
		t.Fatalf("identity = %+v, want unstable with empty (non-nil) HWIDs", a)
	}
	if !strings.HasPrefix(a.Source, "random:") || len(a.Source) != len("random:")+32 {
		t.Fatalf("source = %q", a.Source)
	}
	if !nodeIDRE.MatchString(a.NodeID) || a.NodeID != NodeIDFromSource(a.Source) {
		t.Fatalf("node id %q doesn't derive from %q", a.NodeID, a.Source)
	}
	if a.NodeID == b.NodeID {
		t.Fatal("two random identities are equal")
	}
	if name := DefaultName(a); name != "savior-"+a.NodeID[7:] || !proto.ValidNodeName(name) {
		t.Fatalf("DefaultName = %q", name)
	}
}

// With a -uuid the same VM gets a stable identity (the QEMU test harness
// relies on this).
func TestGetIdentityQEMUWithUUID(t *testing.T) {
	root := copyFixture(t, "qemu")
	writeFile(t, root, "sys/class/dmi/id/product_uuid", "5A0C3F2E-1111-4222-8333-944455556666\n")
	id, err := GetIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Stable || id.Source != "uuid:5a0c3f2e-1111-4222-8333-944455556666" {
		t.Fatalf("identity = %+v", id)
	}
}

// nic describes a synthetic interface for identity tests.
type nic struct {
	name, mac string
	assign    string // "" = no addr_assign_type file
	bus       string // pci, usb, "" (virtual)
	addr      string // device dir name (PCI address or USB interface)
	wireless  bool
}

func addNIC(t *testing.T, root string, n nic) {
	t.Helper()
	dir := "sys/class/net/" + n.name + "/"
	writeFile(t, root, dir+"address", n.mac+"\n")
	writeFile(t, root, dir+"type", "1\n")
	if n.assign != "" {
		writeFile(t, root, dir+"addr_assign_type", n.assign+"\n")
	}
	if n.wireless {
		writeFile(t, root, dir+"wireless/.keep", "")
	}
	if n.bus == "" {
		return
	}
	dev := "sys/devices/pci0000_00/" + n.addr
	writeFile(t, root, dev+"/uevent", "")
	symlink(t, root, dev+"/subsystem", "../../../bus/"+n.bus)
	symlink(t, root, dir+"device", "../../../devices/pci0000_00/"+n.addr)
}

func TestGetIdentitySourceOrder(t *testing.T) {
	const uuid = "4c4c4544-0044-3510-8052-b4c04f4b3032"
	cases := []struct {
		name   string
		nics   []nic
		dmi    map[string]string
		source string
		mac    string
		hwids  []string
	}{
		{
			name: "PCI address order, not name order",
			nics: []nic{
				{name: "eth0", mac: "00:11:11:11:11:11", assign: "0", bus: "pci", addr: "0000_00_05.0"},
				{name: "eth1", mac: "00:22:22:22:22:22", assign: "0", bus: "pci", addr: "0000_00_03.0"},
			},
			source: "mac:002222222222", mac: "00:22:22:22:22:22",
			hwids: []string{"mac:002222222222", "mac:001111111111"},
		},
		{
			name: "locally administered wired MAC skipped",
			nics: []nic{
				{name: "eth0", mac: "02:11:11:11:11:11", assign: "0", bus: "pci", addr: "0000_00_03.0"},
				{name: "wlan0", mac: "00:33:33:33:33:33", assign: "0", bus: "pci", addr: "0000_00_04.0", wireless: true},
			},
			source: "mac:003333333333", mac: "00:33:33:33:33:33", hwids: []string{"mac:003333333333"},
		},
		{
			name: "only permanent addresses (assign type 0) count",
			nics: []nic{
				{name: "eth0", mac: "00:11:11:11:11:11", assign: "1", bus: "pci", addr: "0000_00_01.0"},
				{name: "eth1", mac: "00:22:22:22:22:22", assign: "3", bus: "pci", addr: "0000_00_02.0"},
				{name: "eth2", mac: "00:44:44:44:44:44", assign: "", bus: "pci", addr: "0000_00_03.0"},
				{name: "eth3", mac: "00:55:55:55:55:55", assign: "0", bus: "pci", addr: "0000_00_09.0"},
			},
			source: "mac:005555555555", mac: "00:55:55:55:55:55", hwids: []string{"mac:005555555555"},
		},
		{
			name: "zero, broadcast and multicast MACs skipped",
			nics: []nic{
				{name: "eth0", mac: "00:00:00:00:00:00", assign: "0", bus: "pci", addr: "0000_00_01.0"},
				{name: "eth1", mac: "ff:ff:ff:ff:ff:ff", assign: "0", bus: "pci", addr: "0000_00_02.0"},
				{name: "eth2", mac: "01:00:5e:00:00:01", assign: "0", bus: "pci", addr: "0000_00_03.0"},
				{name: "eth3", mac: "garbage", assign: "0", bus: "pci", addr: "0000_00_04.0"},
			},
			dmi:    map[string]string{"product_uuid": strings.ToUpper(uuid)},
			source: "uuid:" + uuid, hwids: []string{"uuid:" + uuid},
		},
		{
			name:   "virtual interfaces never count",
			nics:   []nic{{name: "br0", mac: "00:66:66:66:66:66", assign: "0"}},
			dmi:    map[string]string{"product_serial": "CZC1234XYZ"},
			source: "serial:czc1234xyz", hwids: []string{"serial:czc1234xyz"},
		},
		{
			name: "placeholder UUID falls through to the board serial",
			dmi: map[string]string{
				"product_uuid":   "03000200-0400-0500-0006-000700080009",
				"product_serial": "To be filled by O.E.M.",
				"board_serial":   "  MB-2201-7788  ",
			},
			source: "serial:mb-2201-7788", hwids: []string{"serial:mb-2201-7788"},
		},
		{
			name: "USB NIC is the last hardware source",
			nics: []nic{{name: "eth0", mac: "00:0e:c6:01:02:03", assign: "0", bus: "usb", addr: "0000_00_1d.0-usb2-1_1.0"}},
			dmi: map[string]string{
				"product_uuid": "FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF", "product_serial": "0123456789",
				"board_serial": "None",
			},
			source: "mac:000ec6010203", mac: "00:0e:c6:01:02:03", hwids: []string{"mac:000ec6010203"},
		},
		{
			name: "every candidate listed in source order",
			nics: []nic{
				{name: "eth0", mac: "00:0e:c6:01:02:03", assign: "0", bus: "usb", addr: "usb-1_1.0"},
				{name: "eth1", mac: "00:aa:aa:aa:aa:aa", assign: "0", bus: "pci", addr: "0000_00_19.0"},
			},
			dmi: map[string]string{
				"product_uuid": uuid, "product_serial": "SERIAL-1", "board_serial": "serial-1",
			},
			source: "mac:00aaaaaaaaaa", mac: "00:aa:aa:aa:aa:aa",
			hwids: []string{"mac:00aaaaaaaaaa", "uuid:" + uuid, "serial:serial-1", "mac:000ec6010203"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "sys/devices/.keep", "")
			for _, n := range c.nics {
				addNIC(t, root, n)
			}
			for k, v := range c.dmi {
				writeFile(t, root, "sys/class/dmi/id/"+k, v+"\n")
			}
			id, err := GetIdentity(root)
			if err != nil {
				t.Fatal(err)
			}
			want := Identity{NodeID: NodeIDFromSource(c.source), MAC: c.mac, Source: c.source, HWIDs: c.hwids, Stable: true}
			if !reflect.DeepEqual(id, want) {
				t.Fatalf("got  %+v\nwant %+v", id, want)
			}
		})
	}
}

func TestGetIdentityEmptyRootIsRandom(t *testing.T) {
	id, err := GetIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if id.Stable || !nodeIDRE.MatchString(id.NodeID) {
		t.Fatalf("identity = %+v", id)
	}
}

func TestValidUUID(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"4c4c4544-0044-3510-8052-b4c04f4b3032", "4c4c4544-0044-3510-8052-b4c04f4b3032", true},
		{" 4C4C4544-0044-3510-8052-B4C04F4B3032\n", "4c4c4544-0044-3510-8052-b4c04f4b3032", true},
		{"00000000-0000-0000-0000-000000000000", "", false},
		{"FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF", "", false},
		{"03000200-0400-0500-0006-000700080009", "", false},
		{"00020003-0004-0005-0006-000700080009", "", false},
		{"4c4c4544004435108052b4c04f4b3032", "", false}, // not canonical
		{"4c4c4544-0044-3510-8052-b4c04f4b303g", "", false},
		{"4c4c4544-0044-3510-8052_b4c04f4b3032", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := validUUID(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("validUUID(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestValidSerial(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"L3A1B2C", "l3a1b2c", true},
		{"  CZC 1234   XYZ ", "czc 1234 xyz", true},
		{"To Be Filled By O.E.M.", "", false},
		{"to be filled by o.e.m.", "", false},
		{"System Serial Number", "", false},
		{"Default string", "", false},
		{"0123456789", "", false},
		{"None", "", false},
		{"Not Specified", "", false},
		{"Chassis Serial Number", "", false},
		{"00000000", "", false},
		{"........", "", false},
		{"xxxxxxxxxxxx", "", false},
		{"ABC", "", false}, // too short
		{"\x01\x02\x03\x04", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := validSerial(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("validSerial(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestUsableMAC(t *testing.T) {
	cases := map[string]bool{
		"00:16:41:e3:4a:7b": true,
		"00:16:41:E3:4A:7B": true,
		"52:54:00:12:34:56": false, // QEMU OUI is locally administered
		"02:00:00:00:00:01": false,
		"00:00:00:00:00:00": false,
		"ff:ff:ff:ff:ff:ff": false,
		"01:00:5e:00:00:fb": false,
		"00:16:41:e3:4a":    false,
		"00:16:41:e3:4a:7":  false,
		"0016.41e3.4a7b":    false,
		"":                  false,
		"zz:16:41:e3:4a:7b": false,
	}
	for mac, want := range cases {
		if got := usableMAC(mac); got != want {
			t.Errorf("usableMAC(%q) = %v, want %v", mac, got, want)
		}
	}
}

func TestDefaultName(t *testing.T) {
	cases := []struct {
		id   Identity
		want string
	}{
		{Identity{NodeID: "n6ead4f09ab5f", MAC: "00:16:41:E3:4A:7B"}, "savior-e34a7b"},
		{Identity{NodeID: "n89c6cc3f885b"}, "savior-3f885b"},
		{Identity{NodeID: "ab"}, "savior-ab"},
		{Identity{NodeID: "N-X_Y.1234"}, "savior-xy1234"},
	}
	for _, c := range cases {
		if got := DefaultName(c.id); got != c.want {
			t.Errorf("DefaultName(%+v) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestNodeIDFromSource(t *testing.T) {
	// Pinned: changing the derivation would re-identify every node in the field.
	if got := NodeIDFromSource("mac:001641e34a7b"); got != "n6ead4f09ab5f" {
		t.Fatalf("NodeIDFromSource = %q", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NodeIDFromSource("serial:" + strconv.Itoa(i))
		if !nodeIDRE.MatchString(id) || seen[id] {
			t.Fatalf("bad or duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestBootID(t *testing.T) {
	if got := BootID(fixture(t, "thinkpad-t60")); got != "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b" {
		t.Errorf("BootID = %q", got)
	}
	if got := BootID(t.TempDir()); got != "" {
		t.Errorf("BootID of empty root = %q", got)
	}
}
