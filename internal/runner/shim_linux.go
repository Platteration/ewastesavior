package runner

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// statusFD is the descriptor (ExtraFiles[0]) the shim writes its failure
// reason to before exiting, so the runner can tell a setup failure apart
// from the task's own exit code. It is close-on-exec: the task never
// inherits it.
const statusFD = 3

// shimArgs holds the parsed sandbox-exec flags.
type shimArgs struct {
	work     string
	rootMnt  string // mountpoint for the private root tmpfs (sandbox=full)
	uid, gid int
	fsizeMB  int
	sandbox  string // full | none
	strict   bool   // sandbox=strict: hardening steps must not be skipped
	seccomp  bool
	netns    bool
	network  bool
	cgroup   string
	env      []string
	cmd      []string
}

// SandboxExecMain is the `savior sandbox-exec` shim (DESIGN 6.5). It runs in
// the task's namespaces, builds the task root, drops privileges, installs
// the seccomp filter and finally execve's the command. On any failure
// before execve it prints a one-line reason to stderr and the status pipe
// and exits with a distinctive code.
func SandboxExecMain(args []string) int {
	// Capabilities, no_new_privs, the parent-death signal and (without
	// TSYNC) the seccomp filter are per-thread: every step must run on the
	// thread that finally execs the command. Never unlocked.
	runtime.LockOSThread()
	// The status pipe is for the shim only; the task must not be able to
	// write a forged setup failure into it.
	syscall.CloseOnExec(statusFD)
	// The agent runs with umask 077 (the run wrapper). The sandbox's /etc,
	// mount points and the task itself need the usual 022, or the task
	// user can't read /etc/passwd, resolv.conf or the CA bundle.
	syscall.Umask(0o022)

	a, err := parseShimArgs(args)
	if err != nil {
		return shimFail("parse args: " + err.Error())
	}
	// Step 1 (self-join) must happen before any real work when the runner
	// could not hand us a cgroup fd.
	if a.cgroup != "" {
		if err := joinCgroup(a.cgroup); err != nil {
			return shimFail("join cgroup: " + err.Error())
		}
	}
	if err := runShim(a); err != nil {
		var ee execError
		if errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, "savior: "+ee.msg)
			return ee.code
		}
		return shimFail(err.Error())
	}
	return shimFail("execve returned without an error") // unreachable on success
}

func parseShimArgs(args []string) (shimArgs, error) {
	var a shimArgs
	fs := flag.NewFlagSet("sandbox-exec", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&a.work, "work", "", "task working directory")
	fs.StringVar(&a.rootMnt, "root-mnt", "", "empty directory to mount the task root on (sandbox=full)")
	fs.IntVar(&a.uid, "uid", 0, "task uid")
	fs.IntVar(&a.gid, "gid", 0, "task gid")
	fs.IntVar(&a.fsizeMB, "fsize-mb", 0, "RLIMIT_FSIZE in MiB")
	fs.StringVar(&a.sandbox, "sandbox", "full", "full or none")
	fs.BoolVar(&a.strict, "strict", false, "fail instead of skipping a hardening step")
	fs.BoolVar(&a.seccomp, "seccomp", true, "install the seccomp filter")
	fs.BoolVar(&a.netns, "netns", true, "a fresh network namespace exists")
	fs.BoolVar(&a.network, "network", false, "the task is allowed network access")
	fs.StringVar(&a.cgroup, "cgroup", "", "cgroup to self-join (self-join fallback)")
	var env stringList
	fs.Var(&env, "env", "environment entry KEY=VALUE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	a.env = env
	a.cmd = fs.Args()
	if a.work == "" {
		return a, fmt.Errorf("--work is required")
	}
	if len(a.cmd) == 0 {
		return a, fmt.Errorf("no command given")
	}
	if a.sandbox == "full" && a.rootMnt == "" {
		return a, fmt.Errorf("--root-mnt is required with --sandbox full")
	}
	return a, nil
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// runShim performs steps 2-7 of DESIGN 6.5 and never returns on success.
func runShim(a shimArgs) error {
	if a.sandbox == "full" {
		newroot, err := buildRoot(a)
		if err != nil {
			return err
		}
		if err := pivotInto(newroot); err != nil {
			return err
		}
		if err := unix.Chdir("/work"); err != nil {
			return fmt.Errorf("chdir /work: %w", err)
		}
	} else {
		if err := unix.Chdir(a.work); err != nil {
			return fmt.Errorf("chdir %s: %w", a.work, err)
		}
	}
	if a.netns {
		if err := ifUp("lo"); err != nil {
			return fmt.Errorf("bring lo up: %w", err)
		}
	}
	if err := setRlimits(a.fsizeMB); err != nil {
		return err
	}
	if err := dropPrivileges(a); err != nil {
		return err
	}
	if a.seccomp {
		if err := installSeccomp(); err != nil {
			return fmt.Errorf("install seccomp: %w", err)
		}
	}
	return execCommand(a)
}

// setRlimits applies the task rlimits (DESIGN 6.5 step 4).
func setRlimits(fsizeMB int) error {
	set := func(res int, cur, max uint64) error {
		return unix.Setrlimit(res, &unix.Rlimit{Cur: cur, Max: max})
	}
	if err := set(unix.RLIMIT_CORE, 0, 0); err != nil {
		return fmt.Errorf("rlimit core: %w", err)
	}
	if err := set(unix.RLIMIT_NOFILE, 4096, 4096); err != nil {
		return fmt.Errorf("rlimit nofile: %w", err)
	}
	// RLIMIT_NPROC is per-uid; keep the current hard limit as the ceiling.
	set(unix.RLIMIT_NPROC, 512, 512)
	if fsizeMB > 0 {
		sz := uint64(fsizeMB) << 20
		if err := set(unix.RLIMIT_FSIZE, sz, sz); err != nil {
			return fmt.Errorf("rlimit fsize: %w", err)
		}
	}
	return nil
}

// dropPrivileges drops to the task uid/gid and locks down capabilities
// (DESIGN 6.5 step 5). Bounding-set capabilities are dropped before the
// setuid, while CAP_SETPCAP is still held. The per-thread steps rely on
// SandboxExecMain having locked the goroutine to its OS thread; the
// syscall package's Setgroups/Setresgid/Setresuid apply to every thread.
func dropPrivileges(a shimArgs) error {
	root := os.Geteuid() == 0
	clearAmbientCaps()
	if root {
		dropBoundingCaps()
		if err := syscall.Setgroups([]int{}); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := unix.Setresgid(a.gid, a.gid, a.gid); err != nil {
			return fmt.Errorf("setresgid: %w", err)
		}
		if err := unix.Setresuid(a.uid, a.uid, a.uid); err != nil {
			return fmt.Errorf("setresuid: %w", err)
		}
		if ruid, euid, suid := unix.Getresuid(); ruid != a.uid || euid != a.uid || suid != a.uid {
			return fmt.Errorf("uid did not drop: %d/%d/%d", ruid, euid, suid)
		}
		if rgid, egid, sgid := unix.Getresgid(); rgid != a.gid || egid != a.gid || sgid != a.gid {
			return fmt.Errorf("gid did not drop: %d/%d/%d", rgid, egid, sgid)
		}
	}
	// The uid change cleared the parent-death signal the runner asked for
	// (SysProcAttr.Pdeathsig): arm it again, then make sure the runner did
	// not die before that, when the signal could not fire any more.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
		return fmt.Errorf("pdeathsig: %w", err)
	}
	if peerGone(statusFD) {
		return errors.New("the runner exited")
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
	return nil
}

// peerGone reports whether fd is the write end of a pipe whose read end is
// closed everywhere. The runner holds the read end of the status pipe
// until the shim execs or exits, so this tells whether the runner is
// still alive (getppid is useless in a pid namespace: it reads 0).
func peerGone(fd int) bool {
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
	for {
		n, err := unix.Poll(pfd, 0)
		if err == unix.EINTR {
			continue
		}
		return err == nil && n == 1 && pfd[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 && pfd[0].Revents&unix.POLLNVAL == 0
	}
}

func clearAmbientCaps() {
	unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0)
}

func dropBoundingCaps() {
	for cap := 0; cap <= unix.CAP_LAST_CAP; cap++ {
		unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(cap), 0, 0, 0)
	}
}

// installSeccomp builds and installs the task filter for the ABIs this
// kernel and shim use. seccomp(2) with SECCOMP_FILTER_FLAG_TSYNC puts it on
// every thread of the shim; kernels without seccomp(2) get it on the
// calling (locked, exec'ing) thread through prctl.
func installSeccomp() error {
	machine, _ := unameMachine()
	prog, err := buildSeccompProgram(filterArches(runtime.GOARCH, machine))
	if err != nil {
		return err
	}
	filter := make([]unix.SockFilter, len(prog))
	for i, in := range prog {
		filter[i] = unix.SockFilter{Code: in.Code, Jt: in.Jt, Jf: in.Jf, K: in.K}
	}
	fprog := &unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	r1, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(fprog)))
	switch {
	case errno == unix.ENOSYS || errno == unix.EINVAL:
		err = unix.Prctl(unix.PR_SET_SECCOMP, uintptr(unix.SECCOMP_MODE_FILTER), uintptr(unsafe.Pointer(fprog)), 0, 0)
	case errno != 0:
		err = errno
	case r1 != 0:
		// TSYNC names the thread it could not synchronize.
		err = fmt.Errorf("thread %d could not be synchronized", r1)
	}
	runtime.KeepAlive(filter)
	runtime.KeepAlive(fprog)
	return err
}

// execError is an execve failure that is the command's own fault. The shim
// exits with the shell's code for it (127 not found, 126 not executable)
// instead of the sandbox failure code, so the task fails like any other
// non-zero exit and uses up an attempt.
type execError struct {
	code int
	msg  string
}

func (e execError) Error() string { return e.msg }

// execCommand resolves the command against the task PATH and execve's it
// with the minimal environment (DESIGN 12 step 5).
func execCommand(a shimArgs) error {
	path, ok := lookPath(a.cmd[0], getenv(a.env, "PATH"))
	if !ok {
		return execError{127, "command not found: " + a.cmd[0]}
	}
	if err := syscall.Exec(path, a.cmd, a.env); err != nil {
		switch err {
		case syscall.ENOENT, syscall.ENOTDIR, syscall.ELOOP, syscall.ENAMETOOLONG:
			// Also a missing #! interpreter.
			return execError{127, fmt.Sprintf("%s: %v", a.cmd[0], err)}
		case syscall.EACCES, syscall.ENOEXEC, syscall.EISDIR, syscall.ETXTBSY:
			// ENOEXEC: not a program for this machine (wrong architecture).
			return execError{126, fmt.Sprintf("%s: %v", a.cmd[0], err)}
		}
		return fmt.Errorf("execve %s: %w", path, err)
	}
	return nil
}

// joinCgroup writes the current pid to <cgroup>/cgroup.procs.
func joinCgroup(dir string) error {
	pid := fmt.Sprintf("%d", os.Getpid())
	return os.WriteFile(dir+"/cgroup.procs", []byte(pid), 0o644)
}

// shimFail reports reason on stderr and the status pipe and returns the
// distinctive exit code the runner maps to ErrorKind "sandbox".
func shimFail(reason string) int {
	fmt.Fprintln(os.Stderr, "savior sandbox-exec: "+reason)
	if f := os.NewFile(statusFD, "status"); f != nil {
		fmt.Fprint(f, reason)
		f.Close()
	}
	return shimFailCode
}

// ifUp brings an interface up via SIOCSIFFLAGS.
func ifUp(name string) error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(sock)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(sock, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(sock, unix.SIOCSIFFLAGS, ifr)
}
