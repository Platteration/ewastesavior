package hwinfo

import (
	"strconv"
	"strings"

	"github.com/platteration/ewastesavior/internal/proto"
)

// maxEDIDRead is how much of an EDID file is read: the base block is all
// the physical size needs.
const maxEDIDRead = 128

// HasDisplay reports whether any DRM card (/sys/class/drm/cardN) or
// framebuffer (/sys/class/graphics/fbN) exists. It drives roles=auto
// (DESIGN 5.3) and is cheap enough to poll.
func HasDisplay(root string) bool {
	for _, name := range listDir(rootPath(root, "sys/class/drm")) {
		if strings.HasPrefix(name, "card") && len(name) > 4 && isDigit(name[4]) {
			return true
		}
	}
	for _, name := range listDir(rootPath(root, "sys/class/graphics")) {
		if strings.HasPrefix(name, "fb") && len(name) > 2 && isDigit(name[2]) {
			return true
		}
	}
	return false
}

// ExternalDisplayConnected reports whether a connector other than a built-in
// panel (eDP, LVDS, DSI) has status "connected". With the lid closed and no
// external display the screen is blanked (DESIGN 10.4).
func ExternalDisplayConnected(root string) bool {
	for _, c := range collectConnectors(root) {
		if c.Status == "connected" && !InternalConnector(c.Name) {
			return true
		}
	}
	return false
}

// InternalConnector reports whether a DRM connector name ("LVDS-1",
// "eDP-1", "DSI-1") is a built-in laptop panel.
func InternalConnector(name string) bool {
	switch connectorType(name) {
	case "eDP", "LVDS", "DSI":
		return true
	}
	return false
}

// connectorType strips the trailing "-N" index: "HDMI-A-1" -> "HDMI-A".
func connectorType(name string) string {
	if i := strings.LastIndexByte(name, '-'); i > 0 {
		if _, err := strconv.Atoi(name[i+1:]); err == nil {
			return name[:i]
		}
	}
	return name
}

// splitDRMName splits "card0-LVDS-1" into ("card0", "LVDS-1"), and "card0"
// into ("card0", ""). ok is false for anything else (renderD128, version).
func splitDRMName(name string) (card, conn string, ok bool) {
	card, conn, _ = strings.Cut(name, "-")
	if _, isCard := numberedName(card, "card"); !isCard {
		return "", "", false
	}
	if strings.Contains(name, "-") && conn == "" {
		return "", "", false
	}
	return card, conn, true
}

// collectGPUs lists DRM cards with their driver and PCI IDs.
func collectGPUs(root string) []proto.GPU {
	var out []proto.GPU
	for _, name := range listDir(rootPath(root, "sys/class/drm")) {
		card, conn, ok := splitDRMName(name)
		if !ok || conn != "" {
			continue
		}
		dev := rootPath(root, "sys/class/drm/"+card+"/device")
		out = append(out, proto.GPU{
			Card:   card,
			Driver: linkBase(dev + "/driver"),
			Vendor: strings.ToLower(attr(dev + "/vendor")),
			Device: strings.ToLower(attr(dev + "/device")),
		})
	}
	return out
}

// collectConnectors lists DRM connectors with status, preferred mode and
// the physical size from their EDID.
func collectConnectors(root string) []proto.Connector {
	var out []proto.Connector
	for _, name := range listDir(rootPath(root, "sys/class/drm")) {
		card, conn, ok := splitDRMName(name)
		if !ok || conn == "" {
			continue
		}
		dir := rootPath(root, "sys/class/drm/"+name)
		c := proto.Connector{
			Name:    conn,
			Card:    card,
			Status:  attr(dir + "/status"),
			Enabled: attr(dir+"/enabled") == "enabled",
		}
		if modes := attr(dir + "/modes"); modes != "" {
			first, _, _ := strings.Cut(modes, "\n")
			c.Preferred = strings.TrimSpace(first)
		}
		if b, err := readFileLimit(dir+"/edid", maxEDIDRead); err == nil {
			c.WidthMM, c.HeightMM = EDIDSizeMM(b)
		}
		out = append(out, c)
	}
	return out
}

// collectFramebuffers lists /sys/class/graphics/fbN.
func collectFramebuffers(root string) []proto.FB {
	var out []proto.FB
	for _, name := range listDir(rootPath(root, "sys/class/graphics")) {
		if _, ok := numberedName(name, "fb"); !ok {
			continue
		}
		dir := rootPath(root, "sys/class/graphics/"+name)
		fb := proto.FB{Name: name, Driver: attr(dir + "/name")}
		if w, h, ok := strings.Cut(attr(dir+"/virtual_size"), ","); ok {
			fb.Width, _ = strconv.Atoi(strings.TrimSpace(w))
			fb.Height, _ = strconv.Atoi(strings.TrimSpace(h))
		}
		fb.BPP, _ = strconv.Atoi(attr(dir + "/bits_per_pixel"))
		if fb.Width < 0 || fb.Height < 0 || fb.BPP < 0 {
			fb.Width, fb.Height, fb.BPP = 0, 0, 0
		}
		out = append(out, fb)
	}
	return out
}
