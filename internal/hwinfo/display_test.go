package hwinfo

import (
	"testing"
)

func TestHasDisplayFixtures(t *testing.T) {
	want := map[string]bool{
		"thinkpad-t60": true, "p4-desktop": true, "atom-netbook": true, "amd-desktop": false, "qemu": true,
	}
	for _, m := range machines {
		if got := HasDisplay(fixture(t, m)); got != want[m] {
			t.Errorf("HasDisplay(%s) = %v, want %v", m, got, want[m])
		}
	}
}

func TestHasDisplaySynthetic(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  bool
	}{
		{"nothing", nil, false},
		{"fbcon only", []string{"sys/class/graphics/fbcon/rotate"}, false},
		{"drm without cards", []string{"sys/class/drm/version", "sys/class/drm/renderD128/dev"}, false},
		{"firmware framebuffer", []string{"sys/class/graphics/fb0/name"}, true},
		{"second card only", []string{"sys/class/drm/card1/dev"}, true},
		{"connector implies card", []string{"sys/class/drm/card0-VGA-1/status"}, true},
		{"card without digit", []string{"sys/class/drm/cardX/dev"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range c.files {
				writeFile(t, root, f, "x\n")
			}
			if got := HasDisplay(root); got != c.want {
				t.Fatalf("HasDisplay = %v, want %v", got, c.want)
			}
		})
	}
}

func TestExternalDisplayConnected(t *testing.T) {
	want := map[string]bool{
		"thinkpad-t60": false, // LVDS connected, VGA disconnected
		"p4-desktop":   true,  // VGA monitor
		"atom-netbook": false,
		"amd-desktop":  false, // headless
		"qemu":         true,  // Virtual-1 is not a built-in panel
	}
	for _, m := range machines {
		if got := ExternalDisplayConnected(fixture(t, m)); got != want[m] {
			t.Errorf("ExternalDisplayConnected(%s) = %v, want %v", m, got, want[m])
		}
	}

	cases := []struct {
		name  string
		conns map[string]string // connector -> status
		want  bool
	}{
		{"eDP only", map[string]string{"card0-eDP-1": "connected", "card0-HDMI-A-1": "disconnected"}, false},
		{"DSI panel", map[string]string{"card0-DSI-1": "connected"}, false},
		{"DisplayPort monitor", map[string]string{"card0-eDP-1": "connected", "card0-DP-2": "connected"}, true},
		{"second card HDMI", map[string]string{"card0-LVDS-1": "connected", "card1-HDMI-A-1": "connected"}, true},
		{"unknown status is not connected", map[string]string{"card0-VGA-1": "unknown"}, false},
		{"S-video TV", map[string]string{"card0-SVIDEO-1": "connected"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			for conn, st := range c.conns {
				writeFile(t, root, "sys/class/drm/"+conn+"/status", st+"\n")
			}
			if got := ExternalDisplayConnected(root); got != c.want {
				t.Fatalf("ExternalDisplayConnected = %v, want %v", got, c.want)
			}
		})
	}
}

func TestConnectorNames(t *testing.T) {
	types := map[string]string{
		"LVDS-1": "LVDS", "eDP-1": "eDP", "HDMI-A-1": "HDMI-A", "DVI-I-2": "DVI-I", "DSI-1": "DSI",
		"Virtual-1": "Virtual", "VGA": "VGA", "DP-": "DP-", "x-y": "x-y",
	}
	for name, want := range types {
		if got := connectorType(name); got != want {
			t.Errorf("connectorType(%q) = %q, want %q", name, got, want)
		}
	}
	internal := map[string]bool{"LVDS-1": true, "eDP-1": true, "DSI-1": true, "eDP": true, "VGA-1": false, "HDMI-A-1": false, "Virtual-1": false}
	for name, want := range internal {
		if got := InternalConnector(name); got != want {
			t.Errorf("InternalConnector(%q) = %v, want %v", name, got, want)
		}
	}
	splits := []struct {
		in, card, conn string
		ok             bool
	}{
		{"card0", "card0", "", true},
		{"card0-LVDS-1", "card0", "LVDS-1", true},
		{"card12-HDMI-A-3", "card12", "HDMI-A-3", true},
		{"card0-", "", "", false},
		{"renderD128", "", "", false},
		{"version", "", "", false},
		{"controlD64", "", "", false},
		{"card-1", "", "", false},
	}
	for _, s := range splits {
		card, conn, ok := splitDRMName(s.in)
		if card != s.card || conn != s.conn || ok != s.ok {
			t.Errorf("splitDRMName(%q) = %q, %q, %v", s.in, card, conn, ok)
		}
	}
}

// makeEDID builds a 128-byte EDID base block like a real panel's.
func makeEDID(dtW, dtH, cmW, cmH int, pixelClock bool) []byte {
	b := make([]byte, 128)
	copy(b, edidHeader[:])
	b[18], b[19] = 1, 3
	b[20] = 0x80
	b[21], b[22] = byte(cmW), byte(cmH)
	if pixelClock {
		b[54], b[55] = 0x64, 0x19 // 65 MHz
	}
	b[56], b[58] = 0x00, 0x40 // 1024 active
	b[66], b[67] = byte(dtW), byte(dtH)
	b[68] = byte((dtW>>8)&0xf)<<4 | byte((dtH>>8)&0xf)
	var sum byte
	for _, v := range b[:127] {
		sum += v
	}
	b[127] = -sum
	return b
}

func TestEDIDSizeMM(t *testing.T) {
	badHeader := makeEDID(285, 214, 29, 21, true)
	badHeader[0] = 0x01
	withExtension := append(makeEDID(476, 268, 48, 27, true), make([]byte, 128)...)
	cases := []struct {
		name string
		edid []byte
		w, h int
	}{
		{"detailed timing mm", makeEDID(285, 214, 29, 21, true), 285, 214},
		{"large panel uses the high nibbles", makeEDID(1872, 1053, 187, 105, true), 1872, 1053},
		{"no size in detailed timing", makeEDID(0, 0, 26, 20, true), 260, 200},
		{"descriptor is not a detailed timing", makeEDID(285, 214, 29, 21, false), 290, 210},
		{"detailed timing holds cm (panel bug)", makeEDID(52, 32, 52, 32, true), 520, 320},
		{"cm field is an aspect ratio", makeEDID(0, 0, 0x4f, 0, false), 0, 0},
		{"projector: no size at all", makeEDID(0, 0, 0, 0, true), 0, 0},
		{"only one detailed dimension", makeEDID(300, 0, 30, 20, true), 300, 200},
		{"implausibly large", makeEDID(4095, 4095, 0, 0, true), 0, 0},
		{"extension blocks ignored", withExtension, 476, 268},
		{"bad header", badHeader, 0, 0},
		{"truncated", makeEDID(285, 214, 29, 21, true)[:100], 0, 0},
		{"empty (disconnected connector)", nil, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := EDIDSizeMM(c.edid)
			if w != c.w || h != c.h {
				t.Fatalf("EDIDSizeMM = %dx%d, want %dx%d", w, h, c.w, c.h)
			}
		})
	}
}

func TestCollectConnectorsEdgeCases(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "sys/class/drm/card0-HDMI-A-1/status", "connected\n")
	writeFile(t, root, "sys/class/drm/card0-HDMI-A-1/enabled", "enabled\n")
	writeFile(t, root, "sys/class/drm/card0-HDMI-A-1/modes", "\n1920x1080\n")
	writeFile(t, root, "sys/class/drm/card0-HDMI-A-1/edid", string(append(makeEDID(509, 286, 51, 29, true), 0xff, 0xff)))
	writeFile(t, root, "sys/class/drm/card10-VGA-1/status", "disconnected\n")
	writeFile(t, root, "sys/class/drm/card2-VGA-1/status", "disconnected\n")
	got := collectConnectors(root)
	if len(got) != 3 {
		t.Fatalf("got %d connectors: %+v", len(got), got)
	}
	if got[0].Name != "HDMI-A-1" || got[0].Preferred != "1920x1080" || got[0].WidthMM != 509 || got[0].HeightMM != 286 || !got[0].Enabled {
		t.Errorf("HDMI connector = %+v", got[0])
	}
	if got[1].Card != "card2" || got[2].Card != "card10" {
		t.Errorf("connectors not in natural card order: %+v", got)
	}
}
