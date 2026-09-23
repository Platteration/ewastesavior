package display

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

// sysTree builds a fake sysfs under root from path -> content ("" makes a
// directory).
func sysTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, p)
		if content == "" {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestAutoDevicePath(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"single efifb", map[string]string{"sys/class/graphics/fb0/name": "EFI VGA\n"}, "/dev/fb0"},
		{"connected card wins", map[string]string{
			"sys/class/graphics/fb0/name":                  "simpledrmdrmfb",
			"sys/class/graphics/fb0/device/drm/card0":      "",
			"sys/class/graphics/fb1/name":                  "i915drmfb",
			"sys/class/graphics/fb1/device/drm/card1":      "",
			"sys/class/drm/card0-Unknown-1/status":         "unknown\n",
			"sys/class/drm/card1-LVDS-1/status":            "disconnected\n",
			"sys/class/drm/card1-VGA-1/status":             "connected\n",
			"sys/class/graphics/fb1/device/drm/renderD128": "",
		}, "/dev/fb1"},
		{"nothing connected: first fb", map[string]string{
			"sys/class/graphics/fb2/name":              "a",
			"sys/class/graphics/fb10/name":             "b",
			"sys/class/graphics/fb10/device/drm/card0": "",
			"sys/class/drm/card0-VGA-1/status":         "disconnected",
		}, "/dev/fb2"},
		{"numeric order", map[string]string{
			"sys/class/graphics/fb10/name":             "b",
			"sys/class/graphics/fb10/device/drm/card0": "",
			"sys/class/graphics/fb9/name":              "a",
			"sys/class/graphics/fb9/device/drm/card1":  "",
			"sys/class/drm/card0-HDMI-A-1/status":      "connected",
			"sys/class/drm/card1-HDMI-A-1/status":      "connected",
			"sys/class/graphics/fbcon/uevent":          "x",
		}, "/dev/fb9"},
		{"card prefix is exact", map[string]string{
			"sys/class/graphics/fb0/name":              "a",
			"sys/class/graphics/fb0/device/drm/card1":  "",
			"sys/class/graphics/fb1/name":              "b",
			"sys/class/graphics/fb1/device/drm/card11": "",
			"sys/class/drm/card11-VGA-1/status":        "connected",
		}, "/dev/fb1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := sysTree(t, tt.files)
			got, err := AutoDevicePath(root)
			if err != nil || got != tt.want {
				t.Fatalf("AutoDevicePath = %q, %v; want %q", got, err, tt.want)
			}
			// A root that already is the sys directory works too.
			if got, err := AutoDevicePath(filepath.Join(root, "sys")); err != nil || got != tt.want {
				t.Fatalf("with /sys root: %q, %v", got, err)
			}
		})
	}
	if _, err := AutoDevicePath(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no framebuffer") {
		t.Fatalf("empty tree: %v", err)
	}
	root := sysTree(t, map[string]string{"sys/class/graphics/fb0/name": "VESA VGA\n"})
	if n := fbName(root, "/dev/fb0"); n != "VESA VGA" {
		t.Fatalf("fbName = %q", n)
	}
}

func TestBacklightSelectionAndBlank(t *testing.T) {
	root := sysTree(t, map[string]string{
		"sys/class/backlight/intel_backlight/type":           "raw",
		"sys/class/backlight/intel_backlight/brightness":     "700",
		"sys/class/backlight/intel_backlight/max_brightness": "1000",
		"sys/class/backlight/acpi_video0/type":               "firmware",
		"sys/class/backlight/acpi_video0/brightness":         "7",
		"sys/class/backlight/acpi_video0/max_brightness":     "15",
	})
	bl := findBacklight(root)
	if bl == nil || filepath.Base(bl.dir) != "acpi_video0" {
		t.Fatalf("picked %+v, want acpi_video0 (firmware first)", bl)
	}
	// No bl_power file: brightness is zeroed and saved.
	if err := bl.off(); err != nil {
		t.Fatal(err)
	}
	if v, _ := readInt(filepath.Join(bl.dir, "brightness")); v != 0 || bl.via != "brightness" {
		t.Fatalf("brightness %d via %q", v, bl.via)
	}
	saved, err := os.ReadFile(backlightStateFile)
	if err != nil || !strings.Contains(string(saved), "acpi_video0 7") {
		t.Fatalf("state file %q, %v", saved, err)
	}
	if err := bl.on(); err != nil {
		t.Fatal(err)
	}
	if v, _ := readInt(filepath.Join(bl.dir, "brightness")); v != 7 {
		t.Fatalf("restored brightness %d, want 7", v)
	}
	if _, err := os.Stat(backlightStateFile); !os.IsNotExist(err) {
		t.Fatal("state file not removed after restore")
	}

	// bl_power is preferred when writable.
	os.WriteFile(filepath.Join(bl.dir, "bl_power"), []byte("0"), 0o644)
	if err := bl.off(); err != nil || bl.via != "bl_power" {
		t.Fatalf("off via %q: %v", bl.via, err)
	}
	if v, _ := readInt(filepath.Join(bl.dir, "bl_power")); v != 4 {
		t.Fatalf("bl_power %d, want 4", v)
	}
	if v, _ := readInt(filepath.Join(bl.dir, "brightness")); v != 7 {
		t.Fatalf("brightness touched: %d", v)
	}
	bl.on()
	if v, _ := readInt(filepath.Join(bl.dir, "bl_power")); v != 0 {
		t.Fatalf("bl_power %d after on", v)
	}
	if findBacklight(t.TempDir()) != nil {
		t.Fatal("found a backlight in an empty tree")
	}
}

func TestRestoreSavedBacklight(t *testing.T) {
	root := sysTree(t, map[string]string{"sys/class/backlight/acpi_video0/brightness": "0"})
	dir := filepath.Join(root, "sys/class/backlight/acpi_video0")
	os.WriteFile(backlightStateFile, []byte(dir+" 9\n"), 0o600)
	if err := restoreSavedBacklight(); err != nil {
		t.Fatal(err)
	}
	if v, _ := readInt(filepath.Join(dir, "brightness")); v != 9 {
		t.Fatalf("brightness %d, want 9", v)
	}
	// A brightness someone changed since is left alone.
	os.WriteFile(filepath.Join(dir, "brightness"), []byte("3"), 0o644)
	os.WriteFile(backlightStateFile, []byte(dir+" 9\n"), 0o600)
	restoreSavedBacklight()
	if v, _ := readInt(filepath.Join(dir, "brightness")); v != 3 {
		t.Fatalf("brightness %d, want 3", v)
	}
	// Garbage is removed, not trusted.
	os.WriteFile(backlightStateFile, []byte("/etc/passwd 1\n"), 0o600)
	if err := restoreSavedBacklight(); err == nil {
		t.Fatal("expected error for a bogus state file")
	}
	if _, err := os.Stat(backlightStateFile); !os.IsNotExist(err) {
		t.Fatal("bogus state file kept")
	}
	if err := restoreSavedBacklight(); err != nil {
		t.Fatalf("no state file: %v", err)
	}
}

func TestLinkHint(t *testing.T) {
	tests := []struct {
		link      proto.HiveLink
		addr, err string
		want      string
	}{
		{proto.LinkConnected, "10.0.0.1:7700", "", ""},
		{proto.LinkUnreachable, "192.168.1.20:7700", "connection refused",
			"Hive found at 192.168.1.20 but port 7700 is blocked. Allow savior through that computer's firewall."},
		{proto.LinkUnreachable, "https://[fd00::5]:8443/", "", "Hive found at fd00::5 but port 8443 is blocked."},
		{proto.LinkUnreachable, "hive.lan", "", "Hive found at hive.lan but port 7700"},
		{proto.LinkUnreachable, "", "", "can't be reached"},
		{proto.LinkKeyMismatch, "", "", "A hive is on the network but its swarm key differs. Check swarm_key in savior.conf."},
		{proto.LinkNoSwarmKey, "", "", "Put swarm_key = ... in savior.conf on the stick."},
		{proto.LinkNoNetwork, "", "", "network cable"},
		{proto.LinkSearching, "", "", "savior hive"},
		{proto.LinkFingerprintMismatch, "10.0.0.2:7700", "", "hive_fingerprint"},
		{proto.LinkVersionMismatch, "", "", "version"},
		{proto.LinkRateLimited, "", "", "retry"},
		{proto.LinkRejected, "", "", "swarm_key"},
		{proto.LinkRejected, "10.0.0.3:7700", "the hive refused this node's registration: invalid node_id", "Fix node_id in savior.conf"},
		{proto.LinkPending, "", "", "Approve"},
		{proto.LinkDuplicate, "", "", "node_id"},
		{"weird_state", "", "boom\x07", "boom"},
		{"", "", "", ""},
	}
	for _, tt := range tests {
		got := LinkHint(tt.link, tt.addr, tt.err)
		if tt.want == "" && got != "" || !strings.Contains(got, tt.want) {
			t.Errorf("LinkHint(%q, %q) = %q, want %q", tt.link, tt.addr, got, tt.want)
		}
		if strings.ContainsAny(got, "\x07\n") {
			t.Errorf("hint contains control characters: %q", got)
		}
	}
}
