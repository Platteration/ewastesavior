package hive

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// SaviorOS paths used by hive_data=auto (DESIGN 13.4). Variables so tests
// can point them elsewhere.
var (
	saviorReleaseFile = "/etc/savior-release"
	saviorDataMount   = "/var/lib/savior/data"
	saviorRAMDir      = "/var/lib/savior/hive"
)

// dataDir is the resolved hive state directory.
type dataDir struct {
	path       string
	persistent bool // false = RAM, lost on reboot
	vfat       bool // FAT: 4 GiB file limit, slow metadata writes
	ram        bool
	warning    string
}

// fsInfo describes the filesystem holding a path.
type fsInfo struct {
	free, total int64 // bytes; 0 = unknown
	ram, vfat   bool
}

// resolveDataDir applies DESIGN 13.4: on SaviorOS "auto" is the mounted
// SAVIOR-DATA partition, else a RAM directory (with a warning); elsewhere
// it is the XDG data directory.
func resolveDataDir(v string) (dataDir, error) {
	var d dataDir
	if v == "" || v == "auto" {
		switch {
		case fileExists(saviorReleaseFile) && isMountPoint(saviorDataMount):
			d.path = filepath.Join(saviorDataMount, "hive")
		case fileExists(saviorReleaseFile):
			d.path = saviorRAMDir
			d.ram = true
			d.warning = "no SAVIOR-DATA partition is mounted: hive state is kept in RAM and lost on reboot"
		default:
			base, err := xdgDataHome()
			if err != nil {
				return d, err
			}
			d.path = filepath.Join(base, "savior", "hive")
		}
	} else {
		d.path = v
	}
	abs, err := filepath.Abs(d.path)
	if err != nil {
		return d, fmt.Errorf("hive data dir: %w", err)
	}
	d.path = abs
	if err := os.MkdirAll(d.path, 0o700); err != nil {
		return d, fmt.Errorf("create hive data dir: %w", err)
	}
	if fi, err := statFS(d.path); err == nil {
		d.vfat = fi.vfat
		if fi.ram && !d.ram {
			d.ram = true
			d.warning = fmt.Sprintf("hive data dir %s is in RAM (tmpfs): state is lost on reboot", d.path)
		}
	}
	d.persistent = !d.ram
	return d, nil
}

// xdgDataHome returns $XDG_DATA_HOME, %LOCALAPPDATA% on Windows, else
// ~/.local/share.
func xdgDataHome() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
		return x, nil
	}
	if runtime.GOOS == "windows" {
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return la, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory for the hive data dir (set --data): %w", err)
	}
	return filepath.Join(home, ".local", "share"), nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
