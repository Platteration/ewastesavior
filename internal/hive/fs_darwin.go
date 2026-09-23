package hive

import (
	"bytes"

	"golang.org/x/sys/unix"
)

func statFS(path string) (fsInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fsInfo{}, err
	}
	bs := uint64(st.Bsize)
	fi := fsInfo{free: int64(st.Bavail * bs), total: int64(st.Blocks * bs)}
	name := string(bytes.TrimRight(st.Fstypename[:], "\x00"))
	fi.vfat = name == "msdos"
	return fi, nil
}

func isMountPoint(string) bool { return false }
