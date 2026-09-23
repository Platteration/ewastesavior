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

// backlightStateFile remembers a brightness we zeroed, so `savior display
// vt-reset` (run after an agent crash) and the next agent can restore it.
// Without this a laptop could stay dark after a crash while blanked.
var backlightStateFile = "/run/savior/backlight.saved"

// backlight is a /sys/class/backlight/<name> device.
type backlight struct {
	dir   string
	saved int // brightness before we set it to 0; -1 = not saved
	via   string
}

// findBacklight picks the backlight to blank: firmware (ACPI) interfaces
// first, then platform, then raw, as the kernel documentation recommends.
func findBacklight(sysRoot string) *backlight {
	class := sysDir(sysRoot, "class/backlight")
	ents, err := os.ReadDir(class)
	if err != nil {
		return nil
	}
	rank := map[string]int{"firmware": 0, "platform": 1, "raw": 2}
	type cand struct {
		dir  string
		rank int
	}
	var cs []cand
	for _, e := range ents {
		dir := filepath.Join(class, e.Name())
		t, _ := os.ReadFile(filepath.Join(dir, "type"))
		r, ok := rank[strings.TrimSpace(string(t))]
		if !ok {
			r = 3
		}
		cs = append(cs, cand{dir, r})
	}
	if len(cs) == 0 {
		return nil
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].rank != cs[j].rank {
			return cs[i].rank < cs[j].rank
		}
		return cs[i].dir < cs[j].dir
	})
	return &backlight{dir: cs[0].dir, saved: -1}
}

func readInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func writeInt(path string, v int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.Itoa(v))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// off turns the backlight off: bl_power=4 (FB_BLANK_POWERDOWN), else
// brightness=0 with the old value saved (in memory and in
// backlightStateFile).
func (b *backlight) off() error {
	if err := writeInt(filepath.Join(b.dir, "bl_power"), 4); err == nil {
		b.via = "bl_power"
		return nil
	}
	cur, err := readInt(filepath.Join(b.dir, "brightness"))
	if err != nil {
		return fmt.Errorf("backlight %s: %w", filepath.Base(b.dir), err)
	}
	if cur > 0 {
		b.saved = cur
		_ = os.WriteFile(backlightStateFile, []byte(b.dir+" "+strconv.Itoa(cur)+"\n"), 0o600)
	}
	if err := writeInt(filepath.Join(b.dir, "brightness"), 0); err != nil {
		return fmt.Errorf("backlight %s: %w", filepath.Base(b.dir), err)
	}
	b.via = "brightness"
	return nil
}

// on undoes off.
func (b *backlight) on() error {
	switch b.via {
	case "bl_power":
		b.via = ""
		return writeInt(filepath.Join(b.dir, "bl_power"), 0)
	case "brightness":
		b.via = ""
		v := b.saved
		if v <= 0 {
			if m, err := readInt(filepath.Join(b.dir, "max_brightness")); err == nil {
				v = m
			}
		}
		b.saved = -1
		_ = os.Remove(backlightStateFile)
		return writeInt(filepath.Join(b.dir, "brightness"), v)
	}
	return nil
}

// restoreSavedBacklight restores a brightness left at 0 by a crashed agent.
func restoreSavedBacklight() error {
	b, err := os.ReadFile(backlightStateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || !strings.Contains(f[0], "backlight") {
		_ = os.Remove(backlightStateFile)
		return fmt.Errorf("bad %s", backlightStateFile)
	}
	v, err := strconv.Atoi(f[1])
	if err != nil || v <= 0 {
		_ = os.Remove(backlightStateFile)
		return fmt.Errorf("bad %s", backlightStateFile)
	}
	if cur, err := readInt(filepath.Join(f[0], "brightness")); err == nil && cur == 0 {
		if err := writeInt(filepath.Join(f[0], "brightness"), v); err != nil {
			return err
		}
	}
	return os.Remove(backlightStateFile)
}
