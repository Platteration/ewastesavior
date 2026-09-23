//go:build linux && 386

package runner

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestSyscallTableMatchesUnix386(t *testing.T) {
	want := map[string]uint32{
		"clone": unix.SYS_CLONE, "clone3": unix.SYS_CLONE3,
		"unshare": unix.SYS_UNSHARE, "setns": unix.SYS_SETNS, "mount": unix.SYS_MOUNT,
		"umount": unix.SYS_UMOUNT, "umount2": unix.SYS_UMOUNT2, "pivot_root": unix.SYS_PIVOT_ROOT,
		"chroot": unix.SYS_CHROOT, "fsopen": unix.SYS_FSOPEN, "fsconfig": unix.SYS_FSCONFIG,
		"fsmount": unix.SYS_FSMOUNT, "fspick": unix.SYS_FSPICK, "open_tree": unix.SYS_OPEN_TREE,
		"open_tree_attr": unix.SYS_OPEN_TREE_ATTR, "move_mount": unix.SYS_MOVE_MOUNT,
		"mount_setattr": unix.SYS_MOUNT_SETATTR, "keyctl": unix.SYS_KEYCTL, "add_key": unix.SYS_ADD_KEY,
		"request_key": unix.SYS_REQUEST_KEY, "bpf": unix.SYS_BPF, "perf_event_open": unix.SYS_PERF_EVENT_OPEN,
		"userfaultfd": unix.SYS_USERFAULTFD, "io_uring_setup": unix.SYS_IO_URING_SETUP,
		"io_uring_enter": unix.SYS_IO_URING_ENTER, "io_uring_register": unix.SYS_IO_URING_REGISTER,
		"kexec_load": unix.SYS_KEXEC_LOAD, "init_module": unix.SYS_INIT_MODULE,
		"finit_module": unix.SYS_FINIT_MODULE, "delete_module": unix.SYS_DELETE_MODULE,
		"open_by_handle_at": unix.SYS_OPEN_BY_HANDLE_AT, "name_to_handle_at": unix.SYS_NAME_TO_HANDLE_AT,
		"acct": unix.SYS_ACCT, "swapon": unix.SYS_SWAPON, "swapoff": unix.SYS_SWAPOFF, "reboot": unix.SYS_REBOOT,
		"settimeofday": unix.SYS_SETTIMEOFDAY, "stime": unix.SYS_STIME, "clock_settime": unix.SYS_CLOCK_SETTIME,
		"clock_settime64": unix.SYS_CLOCK_SETTIME64, "adjtimex": unix.SYS_ADJTIMEX,
		"clock_adjtime": unix.SYS_CLOCK_ADJTIME, "clock_adjtime64": unix.SYS_CLOCK_ADJTIME64,
		"iopl": unix.SYS_IOPL, "ioperm": unix.SYS_IOPERM, "ptrace": unix.SYS_PTRACE,
		"process_vm_readv": unix.SYS_PROCESS_VM_READV, "process_vm_writev": unix.SYS_PROCESS_VM_WRITEV,
		"quotactl": unix.SYS_QUOTACTL, "quotactl_fd": unix.SYS_QUOTACTL_FD,
		"lookup_dcookie": unix.SYS_LOOKUP_DCOOKIE, "vhangup": unix.SYS_VHANGUP, "syslog": unix.SYS_SYSLOG,
	}
	checkTable(t, "386", want)
}
