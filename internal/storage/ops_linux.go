//go:build linux

package storage

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func platformOps() sysOps {
	return sysOps{
		mount:    linuxMount,
		unmount:  func(target string) error { return unix.Unmount(target, 0) },
		rereadPT: linuxRereadPT,
		addPart:  linuxAddPart,
		mknod:    linuxMknod,
		loadModules: func(names []string) {
			if p := findTool("modprobe"); p != "" {
				_ = exec.Command(p, append([]string{"-q", "-a"}, names...)...).Run()
			}
		},
	}
}

func linuxMount(source, target, fstype string, o mountOpts) error {
	var flags uintptr
	if o.ReadOnly {
		flags |= unix.MS_RDONLY
	}
	if o.NoSuid {
		flags |= unix.MS_NOSUID
	}
	if o.NoDev {
		flags |= unix.MS_NODEV
	}
	if o.NoExec {
		flags |= unix.MS_NOEXEC
	}
	if o.NoAtime {
		flags |= unix.MS_NOATIME
	}
	err := unix.Mount(source, target, fstype, flags, o.Data)
	if err != nil {
		return &os.PathError{Op: "mount " + fstype + " " + source, Path: target, Err: err}
	}
	return nil
}

// linuxRereadPT asks the kernel to re-read the partition table (BLKRRPART),
// retrying briefly on EBUSY. With a mounted partition on the disk (the stick
// itself) it keeps failing with EBUSY; the caller then adds the partition
// with linuxAddPart.
func linuxRereadPT(disk string) error {
	f, err := os.OpenFile(disk, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	for i := 0; ; i++ {
		err = unix.IoctlSetInt(int(f.Fd()), unix.BLKRRPART, 0)
		if err == nil || !errors.Is(err, unix.EBUSY) || i == 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("BLKRRPART: %w", err)
	}
	return nil
}

// linuxAddPart tells the kernel about one partition (BLKPG_ADD_PARTITION).
// EBUSY means the kernel already has a partition with that number.
func linuxAddPart(disk string, p DataPlan, sectorSize int) error {
	f, err := os.OpenFile(disk, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	part := unix.BlkpgPartition{
		Start:  int64(p.StartLBA) * int64(sectorSize),
		Length: int64(p.Sectors) * int64(sectorSize),
		Pno:    int32(p.PartNum),
	}
	arg := unix.BlkpgIoctlArg{
		Op:      unix.BLKPG_ADD_PARTITION,
		Datalen: int32(unsafe.Sizeof(part)),
		Data:    (*byte)(unsafe.Pointer(&part)),
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKPG, uintptr(unsafe.Pointer(&arg)))
	if errno != 0 && errno != unix.EBUSY {
		return fmt.Errorf("BLKPG add partition %d: %w", p.PartNum, errno)
	}
	return nil
}

// linuxMknod creates a block device node from a "major:minor" string.
func linuxMknod(path, majMin string) error {
	maj, min, ok := strings.Cut(strings.TrimSpace(majMin), ":")
	if !ok {
		return fmt.Errorf("bad device number %q", majMin)
	}
	a, err1 := strconv.ParseUint(maj, 10, 32)
	b, err2 := strconv.ParseUint(min, 10, 32)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("bad device number %q", majMin)
	}
	return unix.Mknod(path, unix.S_IFBLK|0o600, int(unix.Mkdev(uint32(a), uint32(b))))
}
