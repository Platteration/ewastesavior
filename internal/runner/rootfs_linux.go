package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	msRO     = unix.MS_RDONLY
	msBind   = unix.MS_BIND
	msRec    = unix.MS_REC
	msNoSUID = unix.MS_NOSUID
	msNoDev  = unix.MS_NODEV
	msNoExec = unix.MS_NOEXEC
	msRemnt  = unix.MS_REMOUNT
)

// procMasks are /proc files bind-mounted over with /dev/null (DESIGN 6.5).
var procMasks = []string{
	"cmdline", "kcore", "keys", "kmsg", "sysrq-trigger",
	"timer_list", "sched_debug", "config.gz",
}

// devNodes are the /dev entries bind-mounted from the host. /dev/tty is
// left out: the task runs in its own session without a controlling
// terminal, and the node must never hand it the agent's.
var devNodes = []string{"null", "zero", "full", "random", "urandom"}

// procReadOnly are the /proc subtrees made read-only (DESIGN 6.5).
var procReadOnly = []string{"sys", "irq", "bus"}

// buildRoot builds the task's new root on a tmpfs mounted on a.rootMnt and
// returns its path (DESIGN 6.5 step 2). The caller pivot_roots into it.
// a.rootMnt is a fixed, empty, root-only directory the runner owns; the
// tmpfs lives only in this shim's mount namespace, so nothing is left
// behind on the host.
func buildRoot(a shimArgs) (string, error) {
	// Make all mounts private so nothing propagates back to the host.
	if err := unix.Mount("", "/", "", msRec|unix.MS_PRIVATE, ""); err != nil {
		return "", fmt.Errorf("make / private: %w", err)
	}
	newroot := a.rootMnt
	if fi, err := os.Lstat(newroot); err != nil || !fi.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return "", fmt.Errorf("new root dir: %w", err)
	}
	if err := unix.Mount("tmpfs", newroot, "tmpfs", msNoSUID, "mode=0755"); err != nil {
		return "", fmt.Errorf("new root tmpfs: %w", err)
	}
	j := func(p string) string { return filepath.Join(newroot, p) }

	// Read-only system directories.
	for _, d := range []string{"/bin", "/sbin", "/usr", "/lib", "/lib64", "/lib32"} {
		if !exists(d) {
			continue
		}
		if err := bindRO(d, j(d)); err != nil {
			return "", fmt.Errorf("bind %s: %w", d, err)
		}
	}
	if err := buildEtc(a, j("/etc")); err != nil {
		return "", err
	}
	// /work: the task working directory, writable.
	if err := os.MkdirAll(j("/work"), 0o700); err != nil {
		return "", err
	}
	if err := unix.Mount(a.work, j("/work"), "", msBind, ""); err != nil {
		return "", fmt.Errorf("bind /work: %w", err)
	}
	if err := unix.Mount("", j("/work"), "", msBind|msRemnt|msNoSUID|msNoDev, ""); err != nil {
		return "", fmt.Errorf("remount /work: %w", err)
	}
	// /tmp: a small private tmpfs.
	if err := mkTmpfs(j("/tmp"), "size=64m", msNoSUID|msNoDev); err != nil {
		return "", err
	}
	if err := buildProc(j("/proc"), a.strict); err != nil {
		return "", err
	}
	if err := buildDev(j("/dev")); err != nil {
		return "", err
	}
	return newroot, nil
}

// buildEtc creates a minimal read-only /etc on a tmpfs.
func buildEtc(a shimArgs, etc string) error {
	if err := mkTmpfs(etc, "mode=0755", msNoSUID|msNoDev); err != nil {
		return err
	}
	passwd := fmt.Sprintf("root:x:0:0::/root:/bin/sh\nsavior-job:x:%d:%d::/work:/bin/sh\n", a.uid, a.gid)
	group := fmt.Sprintf("root:x:0:\nsavior-job:x:%d:\n", a.gid)
	files := map[string]string{
		"passwd":        passwd,
		"group":         group,
		"hosts":         "127.0.0.1 localhost\n::1 localhost\n",
		"nsswitch.conf": "passwd: files\ngroup: files\nhosts: files dns\n",
	}
	if a.network {
		files["resolv.conf"] = hostResolvConf()
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(etc, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("write /etc/%s: %w", name, err)
		}
	}
	// TLS trust store, when the host has one.
	if exists("/etc/ssl/certs") {
		dst := filepath.Join(etc, "ssl/certs")
		if err := os.MkdirAll(dst, 0o755); err == nil {
			bindRO("/etc/ssl/certs", dst) // best effort
		}
	}
	// Freeze /etc.
	return unix.Mount("", etc, "", msBind|msRemnt|msRO|msNoSUID|msNoDev, "")
}

// buildProc mounts a fresh /proc, masks sensitive files and makes the
// procReadOnly subtrees read-only. With strict, failing to lock one of
// those subtrees down is an error.
func buildProc(proc string, strict bool) error {
	if err := os.MkdirAll(proc, 0o555); err != nil {
		return err
	}
	if err := unix.Mount("proc", proc, "proc", msNoSUID|msNoDev|msNoExec, "hidepid=2"); err != nil {
		// hidepid may be unsupported on old kernels; retry without it.
		if err := unix.Mount("proc", proc, "proc", msNoSUID|msNoDev|msNoExec, ""); err != nil {
			return fmt.Errorf("mount /proc: %w", err)
		}
	}
	for _, name := range procMasks {
		p := filepath.Join(proc, name)
		if exists(p) {
			unix.Mount("/dev/null", p, "", msBind, "") // best effort
		}
	}
	for _, sub := range procReadOnly {
		p := filepath.Join(proc, sub)
		if !exists(p) {
			continue
		}
		if err := remountRO(p); err != nil && strict {
			return fmt.Errorf("read-only /proc/%s: %w", sub, err)
		}
	}
	return nil
}

// remountRO makes the directory p a read-only, nosuid, nodev, noexec
// mount. p is bind-mounted onto itself first: a remount only applies to a
// mountpoint.
func remountRO(p string) error {
	if err := unix.Mount(p, p, "", msBind, ""); err != nil {
		return err
	}
	return unix.Mount("", p, "", msBind|msRemnt|msRO|msNoSUID|msNoDev|msNoExec, "")
}

// buildDev builds a minimal /dev on a tmpfs.
func buildDev(dev string) error {
	if err := mkTmpfs(dev, "mode=0755", msNoSUID); err != nil {
		return err
	}
	for _, name := range devNodes {
		src := "/dev/" + name
		if !exists(src) {
			continue
		}
		dst := filepath.Join(dev, name)
		if f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.Close()
		}
		unix.Mount(src, dst, "", msBind, "") // best effort
	}
	if err := os.MkdirAll(filepath.Join(dev, "pts"), 0o755); err != nil {
		return err
	}
	if err := unix.Mount("devpts", filepath.Join(dev, "pts"), "devpts", msNoSUID|msNoExec, "newinstance,ptmxmode=0666,mode=0620"); err != nil {
		return fmt.Errorf("mount devpts: %w", err)
	}
	os.Symlink("pts/ptmx", filepath.Join(dev, "ptmx"))
	if err := mkTmpfs(filepath.Join(dev, "shm"), "mode=1777,size=16m", msNoSUID|msNoDev); err != nil {
		return err
	}
	return nil
}

// pivotInto makes newroot the root filesystem and detaches the old one.
func pivotInto(newroot string) error {
	if err := unix.Chdir(newroot); err != nil {
		return fmt.Errorf("chdir new root: %w", err)
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	return nil
}

func bindRO(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(src, dst, "", msBind|msRec, ""); err != nil {
		return err
	}
	return unix.Mount("", dst, "", msBind|msRemnt|msRec|msRO|msNoSUID|msNoDev, "")
}

func mkTmpfs(dst, opts string, flags uintptr) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", dst, "tmpfs", flags, opts); err != nil {
		return fmt.Errorf("mount tmpfs %s: %w", dst, err)
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func hostResolvConf() string {
	if b, err := os.ReadFile("/etc/resolv.conf"); err == nil && len(b) > 0 {
		return string(b)
	}
	return "nameserver 1.1.1.1\n"
}
