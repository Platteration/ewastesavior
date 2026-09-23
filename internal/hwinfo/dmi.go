package hwinfo

import (
	"strconv"
	"strings"
)

// dmiInfo is what /sys/class/dmi/id tells about the machine.
type dmiInfo struct {
	vendor, product, biosDate string
	chassisType               int
	uuid                      string // valid product_uuid (lowercase) or ""
	serials                   []string
}

// laptopChassis are SMBIOS chassis types of portable machines: portable,
// laptop, notebook, hand held, sub notebook, tablet, convertible, detachable.
var laptopChassis = map[int]bool{8: true, 9: true, 10: true, 11: true, 14: true, 30: true, 31: true, 32: true}

// placeholderUUIDs are product_uuid values firmware ships unset (both byte
// orders of the classic AMI placeholder).
var placeholderUUIDs = map[string]bool{
	"03000200-0400-0500-0006-000700080009": true,
	"00020003-0004-0005-0006-000700080009": true,
}

// placeholderStrings are DMI strings that vendors leave unset. The first six
// are the ones DESIGN 10.1 names; the rest are other common spellings.
var placeholderStrings = map[string]bool{
	"to be filled by o.e.m.":   true,
	"system serial number":     true,
	"default string":           true,
	"0123456789":               true,
	"none":                     true,
	"not specified":            true,
	"123456789":                true,
	"1234567890":               true,
	"system manufacturer":      true,
	"system product name":      true,
	"system version":           true,
	"base board serial number": true,
	"chassis serial number":    true,
	"serial number":            true,
	"not applicable":           true,
	"n/a":                      true,
	"na":                       true,
	"oem":                      true,
	"o.e.m.":                   true,
	"unknown":                  true,
	"invalid":                  true,
	"empty":                    true,
	"x.x":                      true,
}

// dmiString cleans a DMI string: printable ASCII only, whitespace collapsed.
func dmiString(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b = append(b, c)
		}
	}
	return collapseSpace(string(b))
}

// isPlaceholder reports whether a cleaned DMI string carries no information.
func isPlaceholder(s string) bool {
	return s == "" || placeholderStrings[strings.ToLower(s)] || allSameChar(s)
}

func allSameChar(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return len(s) > 1
}

// validSerial returns the normalized serial and whether it identifies the
// machine: not a placeholder, at least 4 characters, not one repeated
// character ("00000000", "xxxxxxxx").
func validSerial(s string) (string, bool) {
	s = dmiString(s)
	if len(s) < 4 || isPlaceholder(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// validUUID returns the lowercase UUID and whether it identifies the
// machine: canonical 8-4-4-4-12 hex, not all one digit (all-zero, all-F),
// not a known placeholder.
func validUUID(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 36 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return "", false
			}
		}
	}
	if allSameChar(strings.ReplaceAll(s, "-", "")) || placeholderUUIDs[s] {
		return "", false
	}
	return s, true
}

// readDMI reads /sys/class/dmi/id. product_uuid and the serials are
// readable only by root; without them the identity falls back further.
func readDMI(root string) dmiInfo {
	f := func(name string) string { return dmiString(attr(rootPath(root, "sys/class/dmi/id/"+name))) }
	var d dmiInfo
	d.vendor = f("sys_vendor")
	if isPlaceholder(d.vendor) {
		d.vendor = f("board_vendor")
		if isPlaceholder(d.vendor) {
			d.vendor = ""
		}
	}
	name, version := f("product_name"), f("product_version")
	switch {
	case strings.EqualFold(d.vendor, "LENOVO") && !isPlaceholder(version) && !isPlaceholder(name):
		// Lenovo's product_name is the machine type ("2007FVG"); the model
		// ("ThinkPad T60") is in product_version.
		d.product = version + " (" + name + ")"
	case strings.EqualFold(d.vendor, "LENOVO") && !isPlaceholder(version):
		d.product = version
	case !isPlaceholder(name):
		d.product = name
	default:
		if board := f("board_name"); !isPlaceholder(board) {
			d.product = board
		}
	}
	d.biosDate = f("bios_date")
	if isPlaceholder(d.biosDate) {
		d.biosDate = ""
	}
	d.chassisType, _ = strconv.Atoi(f("chassis_type"))
	d.uuid, _ = validUUID(attr(rootPath(root, "sys/class/dmi/id/product_uuid")))
	for _, name := range []string{"product_serial", "board_serial"} {
		if s, ok := validSerial(attr(rootPath(root, "sys/class/dmi/id/"+name))); ok && !contains(d.serials, s) {
			d.serials = append(d.serials, s)
		}
	}
	return d
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
