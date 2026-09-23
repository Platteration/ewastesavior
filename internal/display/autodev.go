package display

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// sysDir returns <root>/sys/<rel>, accepting a root that already points at
// /sys (so both "/" and "/sys" work as sysRoot).
func sysDir(root, rel string) string {
	if root == "" {
		root = "/"
	}
	p := filepath.Join(root, "sys", rel)
	if _, err := os.Stat(p); err != nil {
		if alt := filepath.Join(root, rel); dirExists(alt) {
			return alt
		}
	}
	return p
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// fbNumbers lists the N of every fbN under /sys/class/graphics, ascending.
func fbNumbers(sysRoot string) []int {
	ents, err := os.ReadDir(sysDir(sysRoot, "class/graphics"))
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range ents {
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "fb")); err == nil && strings.HasPrefix(e.Name(), "fb") && n >= 0 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// fbHasConnectedDisplay reports whether fbN belongs to a DRM card with a
// connector whose status is "connected".
func fbHasConnectedDisplay(sysRoot string, n int) bool {
	drm := filepath.Join(sysDir(sysRoot, "class/graphics"), "fb"+strconv.Itoa(n), "device", "drm")
	ents, err := os.ReadDir(drm)
	if err != nil {
		return false // legacy fbdev (efifb, vesafb): no DRM card
	}
	for _, e := range ents {
		card := e.Name()
		if !strings.HasPrefix(card, "card") || strings.Contains(card, "-") {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(card, "card")); err != nil {
			continue
		}
		if cardConnected(sysRoot, card) {
			return true
		}
	}
	return false
}

func cardConnected(sysRoot, card string) bool {
	class := sysDir(sysRoot, "class/drm")
	ents, err := os.ReadDir(class)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), card+"-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(class, e.Name(), "status"))
		if err == nil && strings.TrimSpace(string(b)) == "connected" {
			return true
		}
	}
	return false
}

// AutoDevicePath implements display_device=auto (DESIGN 11.1): the first
// /dev/fbN whose DRM card has a connected connector, else the first fb.
// sysRoot is the filesystem root holding sys/ ("/" in production, a fixture
// tree in tests; a path ending in /sys works too). The result is always a
// /dev/fbN path.
func AutoDevicePath(sysRoot string) (string, error) {
	fbs := fbNumbers(sysRoot)
	if len(fbs) == 0 {
		return "", errors.New("no framebuffer found (/sys/class/graphics/fb*)")
	}
	for _, n := range fbs {
		if fbHasConnectedDisplay(sysRoot, n) {
			return fmt.Sprintf("/dev/fb%d", n), nil
		}
	}
	return fmt.Sprintf("/dev/fb%d", fbs[0]), nil
}

// fbName reads /sys/class/graphics/fbN/name (the driver, e.g. "i915drmfb").
func fbName(sysRoot, dev string) string {
	b, err := os.ReadFile(filepath.Join(sysDir(sysRoot, "class/graphics"), filepath.Base(dev), "name"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
