package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
)

// stickMBR returns the MBR of a SaviorOS stick with the FAT
// partition at 1 MiB spanning fatMiB MiB.
func stickMBR(fatMiB uint32) []byte {
	return makeMBR(savior(fatMiB * mib512))
}

type initEnv struct {
	s        *fakeSys
	diskPath string
	mounts   []string
	rereads  int
	addParts int
	mkfsDev  string
	opts     initDataOpts
	ops      sysOps
}

// newInitEnv fakes a 4 GiB USB stick (sdb) with the booted FAT partition
// sdb1 mounted on /media/savior.
func newInitEnv(t *testing.T, removable bool, mbr []byte) *initEnv {
	s := newFakeSys(t)
	e := &initEnv{s: s}
	const diskSectors = 8 * 1024 * 1024 // 4 GiB
	path := usbPath
	if !removable {
		path = ataPath
	}
	s.disk("sdb", path, false, diskSectors, nil)
	s.part("sdb", path, 1, 2048, 200*mib512, fat16Image(t, "SAVIOR", "", 0xAABBCCDD))
	s.write("sys/class/block/sdb/queue/logical_block_size", "512\n")
	os.Symlink("../../class/block/sdb1", filepath.Join(s.root, "sys/dev/block/8:17"))
	// The disk "device" is a regular file holding the MBR (sparse beyond).
	e.diskPath = filepath.Join(s.root, "dev", "sdb")
	f, _ := os.Create(e.diskPath)
	f.Write(mbr)
	f.Truncate(2 << 20)
	f.Close()
	s.write("proc/self/mountinfo", "36 25 8:17 / /media/savior ro,nosuid,nodev,noexec - vfat /dev/sdb1 ro\n")
	e.ops = sysOps{
		mount: func(src, dst, fstype string, o mountOpts) error {
			if !o.NoSuid || !o.NoDev || o.ReadOnly {
				t.Errorf("data mount flags %+v", o)
			}
			e.mounts = append(e.mounts, fstype+" "+src)
			return nil
		},
		rereadPT: func(disk string) error {
			// The boot partition is mounted: the kernel refuses to re-read.
			e.rereads++
			return errors.New("BLKRRPART: device or resource busy")
		},
		addPart: func(disk string, p DataPlan, sectorSize int) error {
			// The kernel picks up the new partition: sysfs entry + node,
			// which shows what the disk holds at the partition start.
			e.addParts++
			s.part("sdb", path, p.PartNum, int64(p.StartLBA)*int64(sectorSize)/512, int64(p.Sectors)*int64(sectorSize)/512,
				diskAt(t, e.diskPath, int64(p.StartLBA)*int64(sectorSize), 64<<10))
			return nil
		},
		mknod: func(string, string) error { t.Error("unexpected mknod"); return nil },
	}
	e.opts = initDataOpts{sysRoot: s.root, media: "/media/savior", mountpoint: filepath.Join(s.root, "data"),
		minFree: 256 << 20, partWait: time.Second,
		mkfs: func(dev string, _ io.Writer) error {
			e.mkfsDev = dev
			return os.WriteFile(dev, extImage(DataLabel, extCompatHasJournal, extIncompatExtents, 0), 0o600)
		}}
	return e
}

// diskAt returns n bytes of the fake disk at off (zeros beyond its end).
func diskAt(t *testing.T, disk string, off int64, n int) []byte {
	f, err := os.Open(disk)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return b
}

func TestInitDataCreates(t *testing.T) {
	mbr := stickMBR(200)
	e := newInitEnv(t, true, mbr)
	var out, errb bytes.Buffer
	if rc := initData(e.opts, e.ops, &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	got, _ := os.ReadFile(e.diskPath)
	// Only the 16 bytes of entry 2 changed.
	for i := range mbr {
		if (i < 462 || i >= 478) && got[i] != mbr[i] {
			t.Fatalf("byte %d changed", i)
		}
	}
	if got[462+4] != 0x83 || binary.LittleEndian.Uint32(got[462+8:]) != 201*mib512 {
		t.Errorf("entry % x", got[462:478])
	}
	part := filepath.Join(e.s.root, "dev", "sdb2")
	if e.rereads != 1 || e.addParts != 1 || e.mkfsDev != part {
		t.Errorf("rereads %d addParts %d mkfs %q", e.rereads, e.addParts, e.mkfsDev)
	}
	if len(e.mounts) != 1 || e.mounts[0] != "ext4 "+part {
		t.Errorf("mounts %v", e.mounts)
	}
	if st, err := os.Stat(e.opts.mountpoint); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("mountpoint %v %v", st, err)
	}

	// Second run (next boot): the partition exists and is simply mounted.
	e.mounts, e.mkfsDev, e.rereads, e.addParts = nil, "", 0, 0
	before, _ := os.ReadFile(e.diskPath)
	if rc := initData(e.opts, e.ops, &out, &errb); rc != 0 {
		t.Fatalf("second run rc %d: %s", rc, errb.String())
	}
	after, _ := os.ReadFile(e.diskPath)
	if !bytes.Equal(before, after) || e.mkfsDev != "" || e.rereads+e.addParts != 0 || len(e.mounts) != 1 {
		t.Errorf("second run changed things: mkfs %q rereads %d mounts %v", e.mkfsDev, e.rereads, e.mounts)
	}
}

func TestInitDataDryRunWritesNothing(t *testing.T) {
	mbr := stickMBR(200)
	e := newInitEnv(t, true, mbr)
	e.opts.dryRun = true
	var out, errb bytes.Buffer
	if rc := initData(e.opts, e.ops, &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	got, _ := os.ReadFile(e.diskPath)
	if !bytes.Equal(got[:512], mbr) || e.rereads+e.addParts != 0 || e.mkfsDev != "" || len(e.mounts) != 0 {
		t.Fatal("dry run wrote something")
	}
	if !strings.Contains(out.String(), "would create") {
		t.Errorf("stdout %q", out.String())
	}
}

func TestInitDataRefusals(t *testing.T) {
	cases := []struct {
		name      string
		removable bool
		mbr       []byte
		prep      func(e *initEnv)
		force     bool
		want      string
	}{
		{"fixed disk", false, stickMBR(200), nil, false, "not a removable"},
		{"gpt", true, stickMBR(200), func(e *initEnv) {
			f, _ := os.OpenFile(e.diskPath, os.O_RDWR, 0)
			f.WriteAt([]byte("EFI PART"), 512)
			f.Close()
		}, false, "GPT"},
		{"two partitions", true, makeMBR(savior(200*mib512), testEntry{slot: 1, typ: 0x07, start: 300 * mib512, length: mib512}), nil, false, "more than one"},
		{"no space", true, stickMBR(3900), nil, false, "not enough"},
		{"no medium", true, stickMBR(200), func(e *initEnv) { e.s.write("proc/self/mountinfo", "") }, false, "no boot medium"},
		{"mbr mismatch", true, makeMBR(testEntry{slot: 0, status: 0x80, typ: 0x0C, start: 4096, length: 1000}), nil, false, "does not match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newInitEnv(t, tc.removable, tc.mbr)
			if tc.prep != nil {
				tc.prep(e)
			}
			before, _ := os.ReadFile(e.diskPath)
			e.opts.force = tc.force
			var out, errb bytes.Buffer
			if rc := initData(e.opts, e.ops, &out, &errb); rc != 1 {
				t.Fatalf("rc %d, want 1: %s", rc, errb.String())
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("stderr %q, want %q", errb.String(), tc.want)
			}
			after, _ := os.ReadFile(e.diskPath)
			if !bytes.Equal(before, after) || len(e.mounts) != 0 || e.mkfsDev != "" {
				t.Error("refusal wrote or mounted something")
			}
		})
	}
	// --force allows a fixed disk.
	e := newInitEnv(t, false, stickMBR(200))
	e.opts.force = true
	var out, errb bytes.Buffer
	if rc := initData(e.opts, e.ops, &out, &errb); rc != 0 {
		t.Fatalf("force: rc %d %s", rc, errb.String())
	}
}

// dataDiskSectors is the size of newInitEnv's stick; ownEntry and ownPlan
// are the data partition init-data plans on it (201 MiB to the end).
const dataDiskSectors = 8 * 1024 * 1024

var (
	ownEntry = testEntry{slot: 1, typ: 0x83, start: 201 * mib512, length: dataDiskSectors - 201*mib512}
	ownPlan  = DataPlan{Slot: 1, PartNum: 2, StartLBA: 201 * mib512, Sectors: dataDiskSectors - 201*mib512}
)

// at returns a size-byte image holding each chunk at its offset.
func at(size int, chunks map[int]string) []byte {
	b := make([]byte, size)
	for off, c := range chunks {
		copy(b[off:], c)
	}
	return b
}

func TestInitDataRecoversInterruptedRun(t *testing.T) {
	// The marker and the entry were written on an earlier boot but mkfs
	// never ran; the kernel knows the partition and it holds no filesystem.
	e := newInitEnv(t, true, makeMBR(savior(200*mib512), ownEntry))
	e.s.part("sdb", usbPath, 2, 201*mib512, dataDiskSectors-201*mib512, append(dataMarker(ownPlan), make([]byte, 8192)...))
	var out, errb bytes.Buffer
	if rc := initData(e.opts, e.ops, &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	if e.rereads+e.addParts != 0 || e.mkfsDev == "" || len(e.mounts) != 1 {
		t.Errorf("rereads %d mkfs %q mounts %v", e.rereads, e.mkfsDev, e.mounts)
	}
}

// INIT-1: a partition in exactly the place init-data would put its own is
// formatted only when it carries SaviorOS's marker. Filesystems Probe does
// not recognize (LUKS, btrfs, xfs, NTFS, exFAT ...) used to be formatted.
func TestInitDataRefusesForeignPartition(t *testing.T) {
	marker := string(dataMarker(ownPlan))
	other := ownPlan
	other.Sectors--
	cases := []struct {
		name  string
		image []byte
	}{
		{"ext", extImage("photos", 0, 0x2, 0)},
		{"luks", at(64<<10, map[int]string{0: "LUKS\xba\xbe\x00\x01"})},
		{"luks2", at(64<<10, map[int]string{0: "LUKS\xba\xbe\x00\x02", 0x4000: "SKUL\xba\xbe\x00\x02"})},
		{"btrfs", at(128<<10, map[int]string{64<<10 + 0x40: "_BHRfS_M"})},
		{"xfs", at(64<<10, map[int]string{0: "XFSB"})},
		{"f2fs", at(64<<10, map[int]string{1024: "\x10\x20\xf5\xf2"})},
		{"ntfs", at(64<<10, map[int]string{0: "\xeb\x52\x90NTFS    ", 510: "\x55\xaa"})},
		{"exfat", at(64<<10, map[int]string{0: "\xeb\x76\x90EXFAT   ", 510: "\x55\xaa"})},
		{"lvm", at(64<<10, map[int]string{512: "LABELONE", 536: "LVM2 001"})},
		{"blank", make([]byte, 64<<10)},
		{"marker for other geometry", at(64<<10, map[int]string{0: string(dataMarker(other))})},
		{"marker under a filesystem", func() []byte {
			img := extImage("photos", 0, 0x2, 0)
			copy(img, marker)
			return img
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newInitEnv(t, true, makeMBR(savior(200*mib512), ownEntry))
			e.s.part("sdb", usbPath, 2, 201*mib512, dataDiskSectors-201*mib512, tc.image)
			part := filepath.Join(e.s.root, "dev", "sdb2")
			diskBefore, _ := os.ReadFile(e.diskPath)
			var out, errb bytes.Buffer
			if rc := initData(e.opts, e.ops, &out, &errb); rc != 1 {
				t.Fatalf("rc %d, want 1: %s", rc, errb.String())
			}
			if e.mkfsDev != "" || len(e.mounts) != 0 {
				t.Fatalf("DATA LOSS: mkfs %q mounts %v", e.mkfsDev, e.mounts)
			}
			if !strings.Contains(errb.String(), "SaviorOS did not create") {
				t.Errorf("stderr %q", errb.String())
			}
			if got, _ := os.ReadFile(part); !bytes.Equal(got, tc.image) {
				t.Error("partition changed")
			}
			if got, _ := os.ReadFile(e.diskPath); !bytes.Equal(got, diskBefore) {
				t.Error("disk changed")
			}
		})
	}
}

func TestInitDataMarksBeforeTheEntry(t *testing.T) {
	// A reused stick: the space after the FAT partition holds stale LUKS
	// and btrfs signatures from its previous life.
	e := newInitEnv(t, true, stickMBR(200))
	const off = 201 << 20
	f, _ := os.OpenFile(e.diskPath, os.O_RDWR, 0)
	f.WriteAt([]byte("last FAT sector"), off-512)
	f.WriteAt([]byte("LUKS\xba\xbe\x00\x01"), off)
	f.WriteAt([]byte("_BHRfS_M"), off+64<<10+0x40)
	f.WriteAt([]byte("beyond the wiped MiB"), off+dataMarkerWipe)
	f.Close()
	var out, errb bytes.Buffer
	if rc := initData(e.opts, e.ops, &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	if e.mkfsDev == "" || len(e.mounts) != 1 {
		t.Fatalf("mkfs %q mounts %v", e.mkfsDev, e.mounts)
	}
	// The marker (written before the entry, so it survives an interruption
	// right after the entry) replaced the old signatures in the first MiB
	// and nothing outside it changed.
	if got := diskAt(t, e.diskPath, off, dataMarkerWipe); !bytes.Equal(got[:dataMarkerSize], dataMarker(ownPlan)) ||
		!bytes.Equal(got[dataMarkerSize:], make([]byte, dataMarkerWipe-dataMarkerSize)) {
		t.Errorf("start of the partition % x", got[:80])
	}
	if got := diskAt(t, e.diskPath, off-512, 15); string(got) != "last FAT sector" {
		t.Errorf("boot partition changed: %q", got)
	}
	if got := diskAt(t, e.diskPath, off+dataMarkerWipe, 20); string(got) != "beyond the wiped MiB" {
		t.Errorf("beyond the first MiB: %q", got)
	}
}

func TestInitDataClearsMarkerAfterMkfs(t *testing.T) {
	// BusyBox mke2fs leaves the 1 KiB boot block alone, so the marker would
	// outlive the format.
	e := newInitEnv(t, true, stickMBR(200))
	e.opts.mkfs = func(dev string, _ io.Writer) error {
		e.mkfsDev = dev
		f, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteAt(extImage(DataLabel, 0, 0, 0)[1024:], 1024)
		return err
	}
	var out, errb bytes.Buffer
	if rc := initData(e.opts, e.ops, &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	part := filepath.Join(e.s.root, "dev", "sdb2")
	got, _ := os.ReadFile(part)
	if !bytes.Equal(got[:dataMarkerSize], make([]byte, dataMarkerSize)) {
		t.Errorf("marker still there: % x", got[:dataMarkerSize])
	}
	if fs, err := ProbeFile(part); err != nil || !fs.IsExt() || fs.Label != DataLabel {
		t.Errorf("filesystem after clearing: %+v %v", fs, err)
	}
	// Clearing only ever touches the marker itself.
	if err := clearDataMarker(part, ownPlan); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(part); !bytes.Equal(again, got) {
		t.Error("second clear changed the partition")
	}
}

func TestWriteDataMarker(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk")
	mbr := stickMBR(200)
	os.WriteFile(disk, mbr, 0o600)
	plan, err := PlanDataPartition(mbr, Geometry{Sectors: dataDiskSectors, SectorSize: 512}, 2048, 0)
	if err != nil || plan.StartLBA != ownPlan.StartLBA || plan.Sectors != ownPlan.Sectors {
		t.Fatalf("plan %+v %v", plan, err)
	}
	// The MBR changed since it was planned from: nothing is written.
	changed := append([]byte(nil), mbr...)
	changed[440]++
	if err := writeDataMarker(disk, changed, plan, 512); err == nil {
		t.Error("stale MBR accepted")
	}
	if st, _ := os.Stat(disk); st.Size() != 512 {
		t.Errorf("wrote after a refusal: size %d", st.Size())
	}
	if err := writeDataMarker(disk, mbr, plan, 512); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(disk); st.Size() != 202<<20 {
		t.Errorf("size %d, want the first MiB of the partition", st.Size())
	}
	part := filepath.Join(dir, "part")
	os.WriteFile(part, diskAt(t, disk, 201<<20, 4096), 0o600)
	if ok, err := hasDataMarker(part, plan); !ok || err != nil {
		t.Errorf("marker not found: %v", err)
	}
	other := plan
	other.StartLBA += 2048
	if ok, _ := hasDataMarker(part, other); ok {
		t.Error("marker matches another geometry")
	}
	// 4 KiB sectors: the marker records sectors, the offset is in bytes.
	os.WriteFile(disk, mbr, 0o600)
	p4k := DataPlan{StartLBA: 256, Sectors: 1000}
	if err := writeDataMarker(disk, mbr, p4k, 4096); err != nil {
		t.Fatal(err)
	}
	if got := diskAt(t, disk, 256*4096, dataMarkerSize); !bytes.Equal(got, dataMarker(p4k)) {
		t.Errorf("4K marker % x", got)
	}
	// A partition too small for the marker is refused.
	if err := writeDataMarker(disk, mbr, DataPlan{StartLBA: 4096, Sectors: 0}, 512); err == nil {
		t.Error("empty partition accepted")
	}
}

func TestPickBinary(t *testing.T) {
	p3 := "processor\t: 0\nflags\t\t: fpu vme de pse tsc msr cx8 sep mtrr pge cmov pat mmx fxsr sse\n"
	p4 := "processor\t: 0\nflags\t\t: fpu vme de pse tsc msr pae cx8 sep mmx fxsr sse sse2 ss ht\n" +
		"processor\t: 1\nflags\t\t: fpu vme de pse tsc msr pae cx8 sep mmx fxsr sse sse2 ss ht\n"
	cases := []struct {
		name, cpuinfo string
		files         []string
		want          string // content of savior afterwards
		gone          []string
	}{
		{"p4 keeps sse2", p4, []string{"savior-sse2", "savior-softfloat"}, "savior-sse2", []string{"savior-softfloat", "savior-sse2"}},
		{"p3 keeps softfloat", p3, []string{"savior-sse2", "savior-softfloat"}, "savior-softfloat", []string{"savior-softfloat", "savior-sse2"}},
		{"only softfloat", p4, []string{"savior-softfloat"}, "savior-softfloat", []string{"savior-softfloat"}},
		{"amd64 build", p4, []string{"savior"}, "savior", nil},
		{"replaces placeholder", p3, []string{"savior", "savior-sse2", "savior-softfloat"}, "savior-softfloat", []string{"savior-sse2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ci := filepath.Join(dir, "cpuinfo")
			os.WriteFile(ci, []byte(tc.cpuinfo), 0o644)
			bin := filepath.Join(dir, "bin")
			os.Mkdir(bin, 0o755)
			for _, f := range tc.files {
				os.WriteFile(filepath.Join(bin, f), []byte(f), 0o755)
			}
			var out, errb bytes.Buffer
			if rc := pickBinary(ci, bin, &out, &errb); rc != 0 {
				t.Fatalf("rc %d: %s", rc, errb.String())
			}
			got, err := os.ReadFile(filepath.Join(bin, "savior"))
			if err != nil || string(got) != tc.want {
				t.Errorf("savior = %q (%v), want %q", got, err, tc.want)
			}
			for _, g := range tc.gone {
				if _, err := os.Stat(filepath.Join(bin, g)); err == nil {
					t.Errorf("%s still exists", g)
				}
			}
			// Idempotent.
			if rc := pickBinary(ci, bin, &out, &errb); rc != 0 {
				t.Errorf("second run rc %d", rc)
			}
		})
	}
	if rc := pickBinary("/nonexistent", t.TempDir(), io.Discard, io.Discard); rc != 1 {
		t.Errorf("empty dir: rc %d", rc)
	}
}

func TestCPUHasSSE2(t *testing.T) {
	if cpuHasSSE2(strings.NewReader("")) {
		t.Error("empty cpuinfo")
	}
	if cpuHasSSE2(strings.NewReader("flags : sse sse2x\n")) {
		t.Error("substring match")
	}
	if !cpuHasSSE2(strings.NewReader("flags\t: fpu sse2\n")) {
		t.Error("sse2 not found")
	}
}

func TestFingerprint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cert.pem")
	// The hive's own view (auth.LoadOrCreateCert) of the same certificate.
	keyDir := t.TempDir()
	c, fp, err := auth.LoadOrCreateCert(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, _ := os.ReadFile(filepath.Join(keyDir, "cert.pem"))
	os.WriteFile(p, pemBytes, 0o644)
	got, err := certFingerprint(p)
	if err != nil || got != fp || got != auth.Fingerprint(c.Certificate[0]) {
		t.Errorf("fingerprint %q (%v), want %q", got, err, fp)
	}
	if !strings.HasPrefix(got, "sha256:") || len(got) != 7+64 {
		t.Errorf("format %q", got)
	}
	var out, errb bytes.Buffer
	if rc := run([]string{"fingerprint", p}, &out, &errb, sysOps{}); rc != 0 || strings.TrimSpace(out.String()) != fp {
		t.Errorf("cli rc %d out %q err %q", rc, out.String(), errb.String())
	}
	os.WriteFile(p, []byte("not a cert"), 0o644)
	if _, err := certFingerprint(p); err == nil {
		t.Error("garbage accepted")
	}
}

func TestCLIUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := run(nil, &out, &errb, sysOps{}); rc != 2 {
		t.Errorf("no args rc %d", rc)
	}
	if rc := run([]string{"bogus"}, &out, &errb, sysOps{}); rc != 2 {
		t.Errorf("bogus rc %d", rc)
	}
	if rc := run([]string{"find-media", "--wait", "x"}, &out, &errb, sysOps{}); rc != 2 {
		t.Errorf("bad wait rc %d", rc)
	}
	var s seconds
	if s.Set("20") != nil || time.Duration(s) != 20*time.Second || s.Set("1m") != nil || time.Duration(s) != time.Minute || s.Set("-1") == nil {
		t.Error("seconds flag")
	}
	// probe on a synthetic image.
	p := filepath.Join(t.TempDir(), "img")
	os.WriteFile(p, fat16Image(t, "SAVIOR", "", 0xAABBCCDD), 0o644)
	out.Reset()
	if rc := run([]string{"probe", "--field", "uuid", p}, &out, &errb, sysOps{}); rc != 0 || out.String() != "AABB-CCDD\n" {
		t.Errorf("probe rc %d out %q", rc, out.String())
	}
}
