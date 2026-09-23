package hive

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
	msdosMagic = 0x4d44
)

func statFS(path string) (fsInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fsInfo{}, err
	}
	bs := uint64(st.Bsize)
	fi := fsInfo{free: int64(uint64(st.Bavail) * bs), total: int64(uint64(st.Blocks) * bs)}
	switch uint32(st.Type) {
	case tmpfsMagic, ramfsMagic:
		fi.ram = true
	case msdosMagic:
		fi.vfat = true
	}
	return fi, nil
}

// isMountPoint reports whether dir is listed as a mount point in
// /proc/self/mountinfo.
func isMountPoint(dir string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	want := filepath.Clean(dir)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescapeMount(fields[4]) == want {
			return true
		}
	}
	return false
}

// unescapeMount decodes the \ooo octal escapes used in mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
