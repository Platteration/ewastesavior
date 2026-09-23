package runner

import (
	"errors"
	"fmt"
)

// Classic BPF opcodes used by the seccomp filter (linux/filter.h).
const (
	bpfLD   = 0x00
	bpfJMP  = 0x05
	bpfRET  = 0x06
	bpfW    = 0x00
	bpfABS  = 0x20
	bpfJA   = 0x00
	bpfJEQ  = 0x10
	bpfJSET = 0x40
	bpfK    = 0x00
)

// Seccomp return actions and audit arch values (linux/seccomp.h, linux/audit.h).
const (
	seccompRetKillProcess = 0x80000000
	seccompRetErrno       = 0x00050000
	seccompRetAllow       = 0x7fff0000

	auditArchX86_64 = 0xc000003e
	auditArchI386   = 0x40000003

	// x32SyscallBit marks x32 ABI syscalls on x86_64.
	x32SyscallBit = 0x40000000

	// cloneNewMask is every CLONE_NEW* flag valid for clone(2):
	// NEWNS|NEWCGROUP|NEWUTS|NEWIPC|NEWUSER|NEWPID|NEWNET. CLONE_NEWTIME
	// (0x80) overlaps the exit-signal bits of clone(2) and is only accepted
	// by clone3/unshare, which are refused outright.
	cloneNewMask = 0x7e020000

	errnoEPERM  = 1
	errnoENOSYS = 38
)

// Offsets into struct seccomp_data (little-endian x86).
const (
	offNr      = 0
	offArch    = 4
	offArgs0Lo = 16
)

// bpfInsn mirrors struct sock_filter; it is converted to unix.SockFilter on
// Linux, so the program can be generated and tested on any OS.
type bpfInsn struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

// seccompArch describes one syscall ABI accepted by the filter.
type seccompArch struct {
	Name  string
	Audit uint32
	// X32Check kills syscalls carrying the x32 bit (x86_64 only).
	X32Check bool
	Clone    uint32
	Clone3   uint32
	Denied   []uint32 // syscalls answered with EPERM
}

// deniedNames is the DESIGN 6.5 step 6 denylist, plus the per-arch aliases
// of the same operations (see syscallTable).
var deniedNames = []string{
	"unshare", "setns", "mount", "umount", "umount2", "pivot_root", "chroot",
	"fsopen", "fsconfig", "fsmount", "fspick", "open_tree", "open_tree_attr",
	"move_mount", "mount_setattr", "keyctl", "add_key", "request_key", "bpf",
	"perf_event_open", "userfaultfd", "io_uring_setup", "io_uring_enter",
	"io_uring_register", "kexec_load", "kexec_file_load", "init_module",
	"finit_module", "delete_module", "open_by_handle_at", "name_to_handle_at",
	"acct", "swapon", "swapoff", "reboot", "settimeofday", "stime",
	"clock_settime", "clock_settime64", "adjtimex", "clock_adjtime",
	"clock_adjtime64", "iopl", "ioperm", "ptrace", "process_vm_readv",
	"process_vm_writev", "quotactl", "quotactl_fd", "lookup_dcookie",
	"vhangup", "syslog",
}

// syscallTable maps syscall names to numbers per arch. Names absent from an
// arch (e.g. kexec_file_load on i386, stime on x86_64) are simply skipped.
// The tables are checked against golang.org/x/sys/unix in per-arch tests.
var syscallTable = map[string]map[string]uint32{
	"amd64": {
		"clone": 56, "clone3": 435,
		"unshare": 272, "setns": 308, "mount": 165, "umount2": 166,
		"pivot_root": 155, "chroot": 161, "fsopen": 430, "fsconfig": 431,
		"fsmount": 432, "fspick": 433, "open_tree": 428, "open_tree_attr": 467,
		"move_mount": 429, "mount_setattr": 442, "keyctl": 250, "add_key": 248,
		"request_key": 249, "bpf": 321, "perf_event_open": 298,
		"userfaultfd": 323, "io_uring_setup": 425, "io_uring_enter": 426,
		"io_uring_register": 427, "kexec_load": 246, "kexec_file_load": 320,
		"init_module": 175, "finit_module": 313, "delete_module": 176,
		"open_by_handle_at": 304, "name_to_handle_at": 303, "acct": 163,
		"swapon": 167, "swapoff": 168, "reboot": 169, "settimeofday": 164,
		"clock_settime": 227, "adjtimex": 159, "clock_adjtime": 305,
		"iopl": 172, "ioperm": 173, "ptrace": 101, "process_vm_readv": 310,
		"process_vm_writev": 311, "quotactl": 179, "quotactl_fd": 443,
		"lookup_dcookie": 212, "vhangup": 153, "syslog": 103,
	},
	"386": {
		"clone": 120, "clone3": 435,
		"unshare": 310, "setns": 346, "mount": 21, "umount": 22, "umount2": 52,
		"pivot_root": 217, "chroot": 61, "fsopen": 430, "fsconfig": 431,
		"fsmount": 432, "fspick": 433, "open_tree": 428, "open_tree_attr": 467,
		"move_mount": 429, "mount_setattr": 442, "keyctl": 288, "add_key": 286,
		"request_key": 287, "bpf": 357, "perf_event_open": 336,
		"userfaultfd": 374, "io_uring_setup": 425, "io_uring_enter": 426,
		"io_uring_register": 427, "kexec_load": 283, "init_module": 128,
		"finit_module": 350, "delete_module": 129, "open_by_handle_at": 342,
		"name_to_handle_at": 341, "acct": 51, "swapon": 87, "swapoff": 115,
		"reboot": 88, "settimeofday": 79, "stime": 25, "clock_settime": 264,
		"clock_settime64": 404, "adjtimex": 124, "clock_adjtime": 343,
		"clock_adjtime64": 405, "iopl": 110, "ioperm": 101, "ptrace": 26,
		"process_vm_readv": 347, "process_vm_writev": 348, "quotactl": 131,
		"quotactl_fd": 443, "lookup_dcookie": 253, "vhangup": 111, "syslog": 103,
	},
}

// archFor returns the filter description for a GOARCH-style arch name.
func archFor(goarch string) (seccompArch, error) {
	tab, ok := syscallTable[goarch]
	if !ok {
		return seccompArch{}, fmt.Errorf("seccomp: unsupported arch %q", goarch)
	}
	a := seccompArch{Name: goarch, Clone: tab["clone"], Clone3: tab["clone3"]}
	switch goarch {
	case "amd64":
		a.Audit, a.X32Check = auditArchX86_64, true
	case "386":
		a.Audit = auditArchI386
	}
	for _, n := range deniedNames {
		if nr, ok := tab[n]; ok {
			a.Denied = append(a.Denied, nr)
		}
	}
	return a, nil
}

// filterArches picks the ABIs the filter accepts. The shim's own ABI must
// be allowed (its execve passes through the filter). A 386 shim on an
// x86_64 kernel also allows native 64-bit syscalls, because the task
// binaries on such a system are usually 64-bit. Everything else, including
// i386-compat syscalls under an amd64 shim, is killed.
func filterArches(shimArch, kernelMachine string) []string {
	if shimArch == "386" && kernelMachine == "x86_64" {
		return []string{"386", "amd64"}
	}
	return []string{shimArch}
}

// buildSeccompProgram assembles the task filter for the given arches.
//
// Layout per arch: dispatch on seccomp_data.arch; on x86_64 kill x32
// syscalls; clone3 -> ENOSYS (so libcs fall back to clone); clone with any
// CLONE_NEW* flag -> EPERM; each denied syscall -> EPERM; else allow.
// Unknown arches are killed.
func buildSeccompProgram(arches []string) ([]bpfInsn, error) {
	if len(arches) == 0 {
		return nil, errors.New("seccomp: no arches")
	}
	var specs []seccompArch
	for _, name := range arches {
		a, err := archFor(name)
		if err != nil {
			return nil, err
		}
		specs = append(specs, a)
	}
	var asm assembler
	asm.load(offArch)
	for i, a := range specs {
		asm.jeq(a.Audit, fmt.Sprintf("arch%d", i), "")
	}
	asm.ret(seccompRetKillProcess)
	for i, a := range specs {
		p := fmt.Sprintf("a%d.", i)
		asm.label(fmt.Sprintf("arch%d", i))
		asm.load(offNr)
		if a.X32Check {
			asm.jset(x32SyscallBit, p+"kill", "")
		}
		asm.jeq(a.Clone3, p+"enosys", "")
		asm.jeq(a.Clone, p+"clone", "")
		for _, nr := range a.Denied {
			asm.jeq(nr, p+"eperm", "")
		}
		asm.ret(seccompRetAllow)
		asm.label(p + "clone")
		asm.load(offArgs0Lo)
		asm.jset(cloneNewMask, p+"eperm", "")
		asm.ret(seccompRetAllow)
		asm.label(p + "enosys")
		asm.ret(seccompRetErrno | errnoENOSYS)
		asm.label(p + "eperm")
		asm.ret(seccompRetErrno | errnoEPERM)
		asm.label(p + "kill")
		asm.ret(seccompRetKillProcess)
	}
	return asm.assemble()
}

// assembler builds a BPF program with symbolic forward jump targets.
type assembler struct {
	insns  []bpfInsn
	jumps  []pendingJump
	labels map[string]int
}

type pendingJump struct {
	at     int
	jt, jf string
}

func (a *assembler) label(name string) {
	if a.labels == nil {
		a.labels = map[string]int{}
	}
	a.labels[name] = len(a.insns)
}

func (a *assembler) load(off uint32) {
	a.insns = append(a.insns, bpfInsn{Code: bpfLD | bpfW | bpfABS, K: off})
}

func (a *assembler) ret(v uint32) {
	a.insns = append(a.insns, bpfInsn{Code: bpfRET | bpfK, K: v})
}

// jeq and jset jump to label jt when the condition holds, else to jf
// ("" = fall through to the next instruction).
func (a *assembler) jeq(k uint32, jt, jf string)  { a.jump(bpfJMP|bpfJEQ|bpfK, k, jt, jf) }
func (a *assembler) jset(k uint32, jt, jf string) { a.jump(bpfJMP|bpfJSET|bpfK, k, jt, jf) }

func (a *assembler) jump(code uint16, k uint32, jt, jf string) {
	a.jumps = append(a.jumps, pendingJump{at: len(a.insns), jt: jt, jf: jf})
	a.insns = append(a.insns, bpfInsn{Code: code, K: k})
}

func (a *assembler) assemble() ([]bpfInsn, error) {
	resolve := func(from int, name string) (uint8, error) {
		if name == "" {
			return 0, nil
		}
		to, ok := a.labels[name]
		if !ok {
			return 0, fmt.Errorf("seccomp: undefined label %q", name)
		}
		off := to - from - 1
		if off < 0 || off > 255 {
			return 0, fmt.Errorf("seccomp: jump to %q out of range (%d)", name, off)
		}
		return uint8(off), nil
	}
	for _, j := range a.jumps {
		var err error
		if a.insns[j.at].Jt, err = resolve(j.at, j.jt); err != nil {
			return nil, err
		}
		if a.insns[j.at].Jf, err = resolve(j.at, j.jf); err != nil {
			return nil, err
		}
	}
	if len(a.insns) > 4096 {
		return nil, errors.New("seccomp: program too long")
	}
	return a.insns, nil
}
