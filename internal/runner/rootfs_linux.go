package runner

import (
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

// devNodes are the /dev entries bind-mounted from the host.
var devNodes = []string{"null", "zero", "full", "random", "urandom", "tty"}

// buildRoot builds the task's new root on a tmpfs and returns its path
// (DESIGN 6.5 step 2). The caller pivot_roots into it.
func buildRoot(a shimArgs) (string, error) {
	// Make all mounts private so nothing propagates back to the host.
	if err := unix.Mount("", "/", "", msRec|unix.MS_PRIVATE, ""); err != nil {
		return "", fmt.Errorf("make / private: %w", err)
	}
	newroot, err := os.MkdirTemp("", "savior-root-")
	if err != nil {
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
	if err := buildProc(j("/proc")); err != nil {
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

// buildProc mounts a fresh /proc and masks sensitive files.
func buildProc(proc string) error {
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
	for _, sub := range []string{"sys", "irq", "bus"} {
		p := filepath.Join(proc, sub)
		if exists(p) {
			unix.Mount("", p, "", msBind|msRemnt|msRO|msNoSUID|msNoDev, "")
		}
	}
	return nil
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
