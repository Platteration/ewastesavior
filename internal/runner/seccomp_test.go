package runner

import (
	"reflect"
	"testing"
)

func mustProgram(t *testing.T, arches ...string) []bpfInsn {
	t.Helper()
	prog, err := buildSeccompProgram(arches)
	if err != nil {
		t.Fatalf("buildSeccompProgram(%v): %v", arches, err)
	}
	return prog
}

func eval(t *testing.T, prog []bpfInsn, d seccompData) uint32 {
	t.Helper()
	v, err := runBPF(prog, d)
	if err != nil {
		t.Fatalf("runBPF(%+v): %v", d, err)
	}
	return v
}

var archAudit = map[string]uint32{"amd64": auditArchX86_64, "386": auditArchI386}

func TestSeccompDeniedSyscallsGetEPERM(t *testing.T) {
	for _, arch := range []string{"amd64", "386"} {
		t.Run(arch, func(t *testing.T) {
			prog := mustProgram(t, arch)
			tab := syscallTable[arch]
			n := 0
			for _, name := range deniedNames {
				nr, ok := tab[name]
				if !ok {
					continue
				}
				n++
				got := eval(t, prog, seccompData{Nr: int32(nr), Arch: archAudit[arch]})
				if got != seccompRetErrno|errnoEPERM {
					t.Errorf("%s (%d): got %#x, want ERRNO(EPERM)", name, nr, got)
				}
			}
			if n < 45 {
				t.Errorf("only %d denied syscalls resolved for %s", n, arch)
			}
		})
	}
}

func TestSeccompAllowedSyscallsPass(t *testing.T) {
	allowed := map[string][]int32{
		// read write open close getpid execve socket wait4 exit_group
		"amd64": {0, 1, 2, 3, 39, 59, 41, 61, 231},
		// exit read write open close execve getpid socketcall exit_group
		"386": {1, 3, 4, 5, 6, 11, 20, 102, 252},
	}
	for arch, nrs := range allowed {
		t.Run(arch, func(t *testing.T) {
			prog := mustProgram(t, arch)
			for _, nr := range nrs {
				if got := eval(t, prog, seccompData{Nr: nr, Arch: archAudit[arch]}); got != seccompRetAllow {
					t.Errorf("syscall %d: got %#x, want ALLOW", nr, got)
				}
			}
		})
	}
}

func TestSeccompClone(t *testing.T) {
	for _, arch := range []string{"amd64", "386"} {
		t.Run(arch, func(t *testing.T) {
			prog := mustProgram(t, arch)
			tab := syscallTable[arch]
			cases := []struct {
				name  string
				flags uint64
				want  uint32
			}{
				{"thread", 0x003d0f00, seccompRetAllow}, // CLONE_VM|FS|FILES|SIGHAND|THREAD|SYSVSEM|SETTLS|PARENT_SETTID|CHILD_CLEARTID
				{"fork", 0x01200011, seccompRetAllow},   // CLONE_CHILD_SETTID|CLONE_CHILD_CLEARTID|SIGCHLD
				{"newns", 0x00020000 | 17, seccompRetErrno | errnoEPERM},
				{"newcgroup", 0x02000000, seccompRetErrno | errnoEPERM},
				{"newuts", 0x04000000, seccompRetErrno | errnoEPERM},
				{"newipc", 0x08000000, seccompRetErrno | errnoEPERM},
				{"newuser", 0x10000000, seccompRetErrno | errnoEPERM},
				{"newpid", 0x20000000, seccompRetErrno | errnoEPERM},
				{"newnet", 0x40000000, seccompRetErrno | errnoEPERM},
				{"newuser+newns", 0x10020000, seccompRetErrno | errnoEPERM},
				// High 32 bits of args[0] are not flags; they must not matter.
				{"high bits only", 0xffffffff00000011, seccompRetAllow},
			}
			for _, c := range cases {
				d := seccompData{Nr: int32(tab["clone"]), Arch: archAudit[arch]}
				d.Args[0] = c.flags
				if got := eval(t, prog, d); got != c.want {
					t.Errorf("clone %s (%#x): got %#x, want %#x", c.name, c.flags, got, c.want)
				}
			}
			if got := eval(t, prog, seccompData{Nr: int32(tab["clone3"]), Arch: archAudit[arch]}); got != seccompRetErrno|errnoENOSYS {
				t.Errorf("clone3: got %#x, want ERRNO(ENOSYS)", got)
			}
		})
	}
}

func TestSeccompWrongArchKills(t *testing.T) {
	cases := []struct {
		arches []string
		arch   uint32
	}{
		{[]string{"amd64"}, auditArchI386},     // i386 compat on x86_64
		{[]string{"amd64"}, 0xc00000b7},        // aarch64
		{[]string{"386"}, auditArchX86_64},     // 64-bit on a 32-bit filter
		{[]string{"386", "amd64"}, 0x40000028}, // arm
	}
	for _, c := range cases {
		prog := mustProgram(t, c.arches...)
		for _, nr := range []int32{0, 1, 39, 59} {
			if got := eval(t, prog, seccompData{Nr: nr, Arch: c.arch}); got != seccompRetKillProcess {
				t.Errorf("arches %v, arch %#x, nr %d: got %#x, want KILL_PROCESS", c.arches, c.arch, nr, got)
			}
		}
	}
}

func TestSeccompX32Kills(t *testing.T) {
	prog := mustProgram(t, "amd64")
	for _, nr := range []int32{x32SyscallBit | 0, x32SyscallBit | 59, x32SyscallBit | 272, x32SyscallBit | 512} {
		if got := eval(t, prog, seccompData{Nr: nr, Arch: auditArchX86_64}); got != seccompRetKillProcess {
			t.Errorf("x32 nr %#x: got %#x, want KILL_PROCESS", nr, got)
		}
	}
	// On i386 the same bit is not special (no x32 ABI); a huge number is
	// simply unknown and allowed through to the kernel, which returns ENOSYS.
	prog386 := mustProgram(t, "386")
	if got := eval(t, prog386, seccompData{Nr: x32SyscallBit | 1, Arch: auditArchI386}); got != seccompRetAllow {
		t.Errorf("i386 high nr: got %#x, want ALLOW", got)
	}
}

func TestSeccompMultiArchUsesEachTable(t *testing.T) {
	prog := mustProgram(t, "386", "amd64")
	// unshare is 310 on i386 but process_vm_readv on x86_64: each arch must
	// be judged by its own table.
	if got := eval(t, prog, seccompData{Nr: 310, Arch: auditArchI386}); got != seccompRetErrno|errnoEPERM {
		t.Errorf("i386 unshare: got %#x", got)
	}
	if got := eval(t, prog, seccompData{Nr: 272, Arch: auditArchX86_64}); got != seccompRetErrno|errnoEPERM {
		t.Errorf("x86_64 unshare: got %#x", got)
	}
	// 21 is mount on i386 and access on x86_64.
	if got := eval(t, prog, seccompData{Nr: 21, Arch: auditArchX86_64}); got != seccompRetAllow {
		t.Errorf("x86_64 access: got %#x, want ALLOW", got)
	}
	if got := eval(t, prog, seccompData{Nr: 21, Arch: auditArchI386}); got != seccompRetErrno|errnoEPERM {
		t.Errorf("i386 mount: got %#x", got)
	}
	if got := eval(t, prog, seccompData{Nr: x32SyscallBit | 1, Arch: auditArchX86_64}); got != seccompRetKillProcess {
		t.Errorf("x32 in multi-arch: got %#x", got)
	}
}

func TestFilterArches(t *testing.T) {
	cases := []struct {
		shim, machine string
		want          []string
	}{
		{"amd64", "x86_64", []string{"amd64"}},
		{"386", "i686", []string{"386"}},
		{"386", "i586", []string{"386"}},
		{"386", "x86_64", []string{"386", "amd64"}},
	}
	for _, c := range cases {
		if got := filterArches(c.shim, c.machine); !reflect.DeepEqual(got, c.want) {
			t.Errorf("filterArches(%s, %s) = %v, want %v", c.shim, c.machine, got, c.want)
		}
	}
}

func TestSeccompUnsupportedArch(t *testing.T) {
	if _, err := buildSeccompProgram([]string{"arm64"}); err == nil {
		t.Fatal("expected an error for arm64")
	}
	if _, err := buildSeccompProgram(nil); err == nil {
		t.Fatal("expected an error for no arches")
	}
}

func TestSeccompProgramShape(t *testing.T) {
	for _, arches := range [][]string{{"amd64"}, {"386"}, {"386", "amd64"}} {
		prog := mustProgram(t, arches...)
		if last := prog[len(prog)-1]; last.Code != bpfRET|bpfK {
			t.Errorf("%v: program does not end in RET", arches)
		}
		if len(prog) > 200 {
			t.Errorf("%v: program unexpectedly long (%d insns)", arches, len(prog))
		}
	}
}
