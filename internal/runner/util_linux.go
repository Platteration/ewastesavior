package runner

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// unameMachine returns uname -m (e.g. x86_64, i686).
func unameMachine() (string, error) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "", err
	}
	return string(u.Machine[:clen(u.Machine[:])]), nil
}

func clen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}

// isCgroup2 reports whether dir is on a cgroup2 filesystem.
func isCgroup2(dir string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false
	}
	return uint32(st.Type) == uint32(unix.CGROUP2_SUPER_MAGIC)
}

// wipeDir removes everything inside dir without following symlinks out of
// it, unmounting anything mounted there first (leftover scratch tmpfs).
func wipeDir(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		unix.Unmount(p, unix.MNT_DETACH)
		os.RemoveAll(p)
	}
}

// capList returns the present capabilities in canonical order.
func capList(caps map[string]bool) []string {
	var out []string
	for _, c := range AllCaps {
		if caps[c] {
			out = append(out, c)
		}
	}
	return out
}

// lookPath resolves a command name against the given PATH, like execvp. A
// name containing a slash is returned unchanged.
func lookPath(name, pathEnv string) (string, bool) {
	if strings.Contains(name, "/") {
		return name, true
	}
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand, true
		}
	}
	return name, false
}

// getenv returns the value of key in an "K=V" environment slice.
func getenv(env []string, key string) string {
	pfx := key + "="
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, pfx); ok {
			return v
		}
	}
	return ""
}
