package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DataLabel is the ext4 label of the hive data partition.
const DataLabel = "SAVIOR-DATA"

type initDataOpts struct {
	sysRoot    string
	dev        string // disk (or partition on it) to use instead of the boot medium's disk
	media      string // mountpoint of the boot medium
	mountpoint string
	minFree    uint64 // bytes
	dryRun     bool
	force      bool
	partWait   time.Duration
	mkfs       func(dev string, log io.Writer) error
}

func initData(o initDataOpts, ops sysOps, stdout, stderr io.Writer) int {
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, "savior storage: "+format+"\n", args...) }
	refuse := func(format string, args ...any) int {
		logf("init-data: "+format+"; hive data stays in RAM", args...)
		return 1
	}
	mountinfo := filepath.Join(o.sysRoot, "proc", "self", "mountinfo")
	mi, err := ReadMountInfo(mountinfo)
	if err != nil {
		return refuse("read %s: %v", mountinfo, err)
	}
	if e, ok := findMount(mi, o.mountpoint); ok {
		logf("%s already mounted on %s", e.Source, o.mountpoint)
		fmt.Fprintf(stdout, "%s %s\n", e.Source, o.mountpoint)
		return 0
	}

	// 1. The disk holding the boot medium.
	var name string
	if o.dev != "" {
		name = filepath.Base(o.dev)
	} else {
		e, ok := findMount(mi, o.media)
		if !ok {
			return refuse("no boot medium is mounted on %s", o.media)
		}
		if name, err = blockDevByNumber(o.sysRoot, e.MajMin); err != nil {
			return refuse("find the device of %s (%s): %v", o.media, e.MajMin, err)
		}
	}
	bd, err := readBlockDev(o.sysRoot, name)
	if err != nil {
		return refuse("device %s: %v", name, err)
	}
	var bootStart512 uint64
	if bd.Partition != 0 {
		bootStart512 = bd.Start
	} else if o.dev == "" {
		return refuse("the boot medium %s has no partition table (CD, ISO or superfloppy)", bd.Name)
	}
	disk, err := readBlockDev(o.sysRoot, bd.Disk)
	if err != nil {
		return refuse("disk %s: %v", bd.Disk, err)
	}

	// 2. An existing SAVIOR-DATA partition on that disk is simply mounted.
	parts, _ := diskPartitions(o.sysRoot, disk.Name)
	for _, p := range parts {
		if fs, err := ProbeFile(p.Path); err == nil && fs.IsExt() && fs.Label == DataLabel {
			logf("found %s on %s", DataLabel, p.Path)
			if o.dryRun {
				fmt.Fprintf(stdout, "would mount %s on %s\n", p.Path, o.mountpoint)
				return 0
			}
			return mountData(ops, p.Path, o.mountpoint, logf, stdout)
		}
	}

	// 3. Safety checks.
	if !disk.Portable() && !o.force {
		return refuse("%s is not a removable/USB disk (use --force to allow)", disk.Name)
	}
	if disk.ReadOnly {
		return refuse("%s is read-only", disk.Name)
	}
	sectorSize := 512
	if n, ok := readUint(filepath.Join(o.sysRoot, "sys", "class", "block", disk.Name, "queue", "logical_block_size")); ok && n >= 512 && n <= 65536 {
		sectorSize = int(n)
	}
	g := Geometry{Sectors: uint64(disk.Size) / uint64(sectorSize), SectorSize: sectorSize}

	// 4. Read and plan.
	head, err := readHead(disk.Path, 2*sectorSize)
	if err != nil {
		return refuse("read %s: %v", disk.Path, err)
	}
	mbr := head[:mbrSize]
	if HasGPTHeader(head[sectorSize : 2*sectorSize]) {
		return refuse("%s: %v", disk.Name, ErrGPT)
	}
	bootLBA := bootStart512 * 512 / uint64(sectorSize)
	plan, err := PlanDataPartition(mbr, g, bootLBA, o.minFree)
	if err != nil {
		return refuse("%s: %v", disk.Name, err)
	}
	partName := partitionName(disk.Name, plan.PartNum)
	partPath := filepath.Join(o.sysRoot, "dev", partName)
	if plan.Existing {
		logf("%s: %s already in the partition table", disk.Name, plan)
	} else {
		logf("%s: adding %s (%d MiB)", disk.Name, plan, plan.Sectors*uint64(sectorSize)>>20)
	}
	if o.dryRun {
		fmt.Fprintf(stdout, "would create %s: %s, format ext4 %s, mount on %s\n", partPath, plan, DataLabel, o.mountpoint)
		return 0
	}

	// 5. Mark the space as ours, then write the 16-byte entry (and nothing
	// else in the table). The marker goes first so that an entry for this
	// layout without the marker behind it is never taken for ours (step 7).
	if !plan.Existing {
		if err := writeDataMarker(disk.Path, mbr, plan, sectorSize); err != nil {
			return refuse("mark the new partition on %s: %v", disk.Name, err)
		}
		if err := writeEntry(disk.Path, mbr, plan); err != nil {
			return refuse("write partition table of %s: %v", disk.Name, err)
		}
	}

	// 6. Make the kernel see it and wait for the node. BLKRRPART fails with
	// EBUSY while the boot partition is mounted; then (or when a re-read did
	// not produce the partition) it is added with BLKPG.
	present := func() bool { return partitionPresent(o.sysRoot, partName, plan, sectorSize) }
	if !present() {
		if err := ops.rereadPT(disk.Path); err != nil {
			logf("%v; adding the partition with BLKPG", err)
		} else {
			waitFor(present, 2*time.Second)
		}
		if !present() {
			if err := ops.addPart(disk.Path, plan, sectorSize); err != nil {
				logf("%v", err)
			}
		}
	}
	if !waitFor(present, o.partWait) {
		return refuse("the kernel did not pick up %s", partName)
	}
	nodeExists := func() bool { _, err := os.Stat(partPath); return err == nil }
	if !waitFor(nodeExists, 5*time.Second) {
		b, _ := os.ReadFile(filepath.Join(o.sysRoot, "sys", "class", "block", partName, "dev"))
		if err := ops.mknod(partPath, string(b)); err != nil {
			return refuse("no device node %s: %v", partPath, err)
		}
	}

	// 7. Format unless it already carries our filesystem, and only when it
	// provably is the partition SaviorOS added: it starts with the marker
	// written in step 5 (now or on an earlier, interrupted boot). Probe knows
	// only a few filesystems, so "nothing recognized" is not proof: a LUKS,
	// btrfs, xfs or NTFS partition someone made in the same place is refused.
	fs, err := ProbeFile(partPath)
	if err != nil {
		return refuse("read %s: %v", partPath, err)
	}
	if !(fs.IsExt() && fs.Label == DataLabel) {
		if fs.Type != "" {
			return refuse("%s holds a %s filesystem labelled %q that SaviorOS did not create", partPath, fs.Type, fs.Label)
		}
		owned, err := hasDataMarker(partPath, plan)
		if err != nil {
			return refuse("read %s: %v", partPath, err)
		}
		if !owned {
			return refuse("%s already holds data SaviorOS did not create (unknown content, no SaviorOS marker)", partPath)
		}
		logf("formatting %s (ext4, label %s)", partPath, DataLabel)
		mkfs := o.mkfs
		if mkfs == nil {
			mkfs = mkfsData
		}
		if err := mkfs(partPath, stderr); err != nil {
			return refuse("format %s: %v", partPath, err)
		}
		// e2fsprogs wipes sector 0, BusyBox mke2fs leaves it alone.
		if err := clearDataMarker(partPath, plan); err != nil {
			logf("clear the marker on %s: %v", partPath, err)
		}
	}
	return mountData(ops, partPath, o.mountpoint, logf, stdout)
}

// The ownership marker of a data partition SaviorOS is adding. Before the
// entry is written, the first MiB of the space the partition will cover is
// zeroed (which also removes stale filesystem signatures) and the marker is
// put at its start; mkfs and clearDataMarker remove it again. Its geometry
// fields tie it to exactly the planned partition.
const (
	dataMarkerMagic = "SAVIOR-DATA-PENDING\x00"
	dataMarkerSize  = 64
	dataMarkerWipe  = 1 << 20
)

// dataMarker returns the marker for p: the magic, then StartLBA and Sectors
// (little endian uint64), then zeros.
func dataMarker(p DataPlan) []byte {
	b := make([]byte, dataMarkerSize)
	n := copy(b, dataMarkerMagic)
	binary.LittleEndian.PutUint64(b[n:], p.StartLBA)
	binary.LittleEndian.PutUint64(b[n+8:], p.Sectors)
	return b
}

// writeDataMarker zeroes the start of the planned partition on the whole
// disk and writes the marker there, after checking that the MBR is still
// exactly what was planned from (so the space is still unallocated). It syncs
// and verifies.
func writeDataMarker(disk string, mbr []byte, p DataPlan, sectorSize int) error {
	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	cur := make([]byte, mbrSize)
	if _, err := f.ReadAt(cur, 0); err != nil {
		return err
	}
	if !bytes.Equal(cur, mbr[:mbrSize]) {
		return errors.New("the MBR changed while planning")
	}
	n := uint64(dataMarkerWipe)
	if size := p.Sectors * uint64(sectorSize); size < n {
		n = size
	}
	if n < dataMarkerSize {
		return fmt.Errorf("partition of %d bytes is too small", n)
	}
	buf := make([]byte, n)
	marker := dataMarker(p)
	copy(buf, marker)
	off := int64(p.StartLBA) * int64(sectorSize)
	if _, err := f.WriteAt(buf, off); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	got := make([]byte, dataMarkerSize)
	if _, err := f.ReadAt(got, off); err != nil {
		return err
	}
	if !bytes.Equal(got, marker) {
		return errors.New("verification after write failed")
	}
	return nil
}

// hasDataMarker reports whether the partition at path starts with p's marker.
func hasDataMarker(path string, p DataPlan) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	got := make([]byte, dataMarkerSize)
	if _, err := f.ReadAt(got, 0); err != nil {
		return false, err
	}
	return bytes.Equal(got, dataMarker(p)), nil
}

// clearDataMarker zeroes p's marker at the start of the freshly formatted
// partition at path, if it is still there. Those bytes are the ext boot
// block, which the filesystem does not use.
func clearDataMarker(path string, p DataPlan) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	got := make([]byte, dataMarkerSize)
	if _, err := f.ReadAt(got, 0); err != nil {
		return err
	}
	if !bytes.Equal(got, dataMarker(p)) {
		return nil
	}
	if _, err := f.WriteAt(make([]byte, dataMarkerSize), 0); err != nil {
		return err
	}
	return f.Sync()
}

// waitFor polls cond every 100 ms until it is true or d has passed.
func waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// readHead reads the first n bytes of a device.
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// writeEntry writes the plan's 16-byte entry after checking that the MBR is
// still exactly what was planned from, then syncs and verifies.
func writeEntry(disk string, mbr []byte, p DataPlan) error {
	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	cur := make([]byte, mbrSize)
	if _, err := f.ReadAt(cur, 0); err != nil {
		return err
	}
	if !bytes.Equal(cur, mbr[:mbrSize]) {
		return errors.New("the MBR changed while planning")
	}
	if _, err := f.WriteAt(p.Entry[:], p.Offset()); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if _, err := f.ReadAt(cur, 0); err != nil {
		return err
	}
	if !bytes.Equal(cur, ApplyPlan(mbr[:mbrSize], p)) {
		return errors.New("verification after write failed")
	}
	return nil
}

// partitionPresent reports whether sysfs shows the planned partition.
func partitionPresent(sysRoot, name string, p DataPlan, sectorSize int) bool {
	start, ok := readUint(filepath.Join(sysRoot, "sys", "class", "block", name, "start"))
	return ok && start*512 == p.StartLBA*uint64(sectorSize)
}

// mountData mounts the data partition read-write and makes its root 0700.
func mountData(ops sysOps, dev, dir string, logf func(string, ...any), stdout io.Writer) int {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logf("init-data: create %s: %v", dir, err)
		return 1
	}
	var errs []error
	for _, t := range []string{"ext4", "ext3", "ext2"} {
		err := ops.mount(dev, dir, t, mountOpts{NoSuid: true, NoDev: true, NoExec: true, NoAtime: true})
		if err == nil {
			if err := os.Chmod(dir, 0o700); err != nil {
				logf("init-data: chmod %s: %v", dir, err)
			}
			logf("mounted %s on %s", dev, dir)
			fmt.Fprintf(stdout, "%s %s\n", dev, dir)
			return 0
		}
		errs = append(errs, err)
	}
	logf("init-data: mount %s: %v; hive data stays in RAM", dev, errors.Join(errs...))
	return 1
}

// mkfsData formats dev as ext4 with e2fsprogs, or as ext2 with BusyBox
// mke2fs when that is all the image has.
func mkfsData(dev string, log io.Writer) error {
	ext4, mke2fs, bb := findTool("mkfs.ext4"), findTool("mke2fs"), findTool("busybox")
	var argv []string
	switch {
	case isE2fsprogs(ext4):
		argv = []string{ext4, "-q", "-F", "-m", "1", "-L", DataLabel, dev}
	case isE2fsprogs(mke2fs):
		argv = []string{mke2fs, "-q", "-F", "-t", "ext4", "-m", "1", "-L", DataLabel, dev}
	case mke2fs != "": // BusyBox mke2fs: ext2, no -t
		argv = []string{mke2fs, "-F", "-m", "1", "-L", DataLabel, dev}
	case bb != "":
		argv = []string{bb, "mke2fs", "-F", "-m", "1", "-L", DataLabel, dev}
	default:
		return errors.New("no mke2fs found")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// isE2fsprogs reports whether path is an e2fsprogs mke2fs/mkfs.ext4: only
// that one accepts -V (BusyBox mke2fs prints its usage and fails).
func isE2fsprogs(path string) bool {
	return path != "" && exec.Command(path, "-V").Run() == nil
}

func findTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/sbin", "/usr/sbin", "/bin", "/usr/bin"} {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}
