package runner

import (
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
// from the task's own exit code.
const statusFD = 3

// shimArgs holds the parsed sandbox-exec flags.
type shimArgs struct {
	work     string
	uid, gid int
	fsizeMB  int
	sandbox  string // full | none
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
	a, err := parseShimArgs(args)
	if err != nil {
		return shimFail("parse args: " + err.Error())
	}
	// Step 1 (self-join) must happen before anything else when the runner
	// could not hand us a cgroup fd.
	if a.cgroup != "" {
		if err := joinCgroup(a.cgroup); err != nil {
			return shimFail("join cgroup: " + err.Error())
		}
	}
	if err := runShim(a); err != nil {
		return shimFail(err.Error())
	}
	return shimFail("execve returned without an error") // unreachable on success
}

func parseShimArgs(args []string) (shimArgs, error) {
	var a shimArgs
	fs := flag.NewFlagSet("sandbox-exec", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&a.work, "work", "", "task working directory")
	fs.IntVar(&a.uid, "uid", 0, "task uid")
	fs.IntVar(&a.gid, "gid", 0, "task gid")
	fs.IntVar(&a.fsizeMB, "fsize-mb", 0, "RLIMIT_FSIZE in MiB")
	fs.StringVar(&a.sandbox, "sandbox", "full", "full or none")
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
// setuid, while CAP_SETPCAP is still held.
func dropPrivileges(a shimArgs) error {
	root := os.Geteuid() == 0
	clearAmbientCaps()
	if root {
		dropBoundingCaps()
		if err := unix.Setgroups([]int{}); err != nil {
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
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
	return nil
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
// kernel and shim use.
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
	err = unix.Prctl(unix.PR_SET_SECCOMP, uintptr(unix.SECCOMP_MODE_FILTER), uintptr(unsafe.Pointer(fprog)), 0, 0)
	runtime.KeepAlive(filter)
	runtime.KeepAlive(fprog)
	return err
}

// execCommand resolves the command against the task PATH and execve's it
// with the minimal environment (DESIGN 12 step 5).
func execCommand(a shimArgs) error {
	path, ok := lookPath(a.cmd[0], getenv(a.env, "PATH"))
	if !ok {
		return fmt.Errorf("command not found: %s", a.cmd[0])
	}
	if err := syscall.Exec(path, a.cmd, a.env); err != nil {
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
