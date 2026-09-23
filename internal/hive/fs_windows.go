package hive

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func statFS(path string) (fsInfo, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fsInfo{}, err
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return fsInfo{}, err
	}
	fi := fsInfo{free: int64(avail), total: int64(total)}
	if vol := filepath.VolumeName(path); vol != "" {
		root, err := windows.UTF16PtrFromString(vol + `\`)
		if err == nil {
			name := make([]uint16, 64)
			if windows.GetVolumeInformation(root, nil, 0, nil, nil, nil, &name[0], uint32(len(name))) == nil {
				fi.vfat = strings.HasPrefix(strings.ToUpper(windows.UTF16ToString(name)), "FAT")
			}
		}
	}
	return fi, nil
}

func isMountPoint(string) bool { return false }
