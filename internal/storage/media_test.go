package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSys builds a fake /sys + /dev tree for block devices.
type fakeSys struct {
	t    *testing.T
	root string
}

func newFakeSys(t *testing.T) *fakeSys {
	root := t.TempDir()
	for _, d := range []string{"sys/class/block", "sys/dev/block", "dev", "proc/self", "run"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "proc/self/mountinfo"), nil, 0o644)
	return &fakeSys{t: t, root: root}
}

const (
	usbPath   = "devices/pci0000:00/0000:00:1d.7/usb1/1-1/1-1:1.0/host6/target6:0:0/6:0:0:0/block"
	ataPath   = "devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block"
	cdPath    = "devices/pci0000:00/0000:00:1f.1/ata2/host1/target1:0:0/1:0:0:0/block"
	mmcPath   = "devices/pci0000:00/0000:00:1e.0/mmc_host/mmc0/mmc0:0001/block"
	loopPath  = "devices/virtual/block"
	majorBase = 8
)

func (s *fakeSys) write(rel, content string) {
	s.t.Helper()
	p := filepath.Join(s.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *fakeSys) link(name, target string) {
	s.t.Helper()
	if err := os.Symlink(target, filepath.Join(s.root, "sys/class/block", name)); err != nil {
		s.t.Fatal(err)
	}
}

// disk adds a whole disk; image (may be nil) becomes /dev/<name>.
func (s *fakeSys) disk(name, devPath string, removable bool, sectors int64, image []byte) {
	dir := filepath.Join("sys", devPath, name)
	s.write(filepath.Join(dir, "size"), strconv.FormatInt(sectors, 10)+"\n")
	s.write(filepath.Join(dir, "removable"), map[bool]string{true: "1\n", false: "0\n"}[removable])
	s.write(filepath.Join(dir, "ro"), "0\n")
	s.write(filepath.Join(dir, "dev"), "8:0\n")
	s.link(name, filepath.Join("../..", devPath, name))
	if image != nil {
		s.write(filepath.Join("dev", name), string(image))
	}
}

// part adds partition n of disk.
func (s *fakeSys) part(disk, devPath string, n int, start, sectors int64, image []byte) string {
	name := partitionName(disk, n)
	dir := filepath.Join("sys", devPath, disk, name)
	s.write(filepath.Join(dir, "partition"), strconv.Itoa(n)+"\n")
	s.write(filepath.Join(dir, "start"), strconv.FormatInt(start, 10)+"\n")
	s.write(filepath.Join(dir, "size"), strconv.FormatInt(sectors, 10)+"\n")
	s.write(filepath.Join(dir, "ro"), "0\n")
	s.write(filepath.Join(dir, "dev"), "8:"+strconv.Itoa(n)+"\n")
	s.link(name, filepath.Join("../..", devPath, disk, name))
	if image != nil {
		s.write(filepath.Join("dev", name), string(image))
	}
	return name
}

func names(devs []BlockDev) []string {
	var out []string
	for _, d := range devs {
		out = append(out, d.Name)
	}
	return out
}

func TestListBlockDevicesOrder(t *testing.T) {
	s := newFakeSys(t)
	blank := make([]byte, 4096)
	s.disk("sda", ataPath, false, 1<<20, blank)
	s.part("sda", ataPath, 1, 2048, 1000, blank)
	s.part("sda", ataPath, 2, 4096, 1000, blank)
	s.disk("sdb", usbPath, false, 1<<20, blank) // USB sticks often report removable=0
	s.part("sdb", usbPath, 1, 2048, 1000, blank)
	s.disk("sr0", cdPath, true, 1<<20, blank)
	s.disk("sr1", cdPath, true, 0, nil) // empty drive
	s.disk("mmcblk0", mmcPath, false, 1<<20, blank)
	s.part("mmcblk0", mmcPath, 1, 2048, 1000, blank)
	s.disk("loop0", loopPath, false, 1<<20, blank)
	s.disk("zram0", loopPath, false, 1<<20, blank)
	s.disk("ram0", loopPath, false, 1<<20, blank)
	s.disk("fd0", loopPath, true, 2880, blank)

	devs, err := ListBlockDevices(s.root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mmcblk0", "mmcblk0p1", "sdb", "sdb1", "sr0", "sda", "sda1", "sda2"}
	if got := names(devs); !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	byName := map[string]BlockDev{}
	for _, d := range devs {
		byName[d.Name] = d
	}
	if d := byName["sdb1"]; !d.USB || d.Disk != "sdb" || d.Partition != 1 || d.Start != 2048 || d.Size != 1000*512 {
		t.Errorf("sdb1 = %+v", d)
	}
	if d := byName["sda2"]; d.Portable() || d.Disk != "sda" {
		t.Errorf("sda2 = %+v", d)
	}
	if d := byName["sr0"]; !d.CDROM || !d.Portable() {
		t.Errorf("sr0 = %+v", d)
	}
	if d := byName["mmcblk0p1"]; !d.Removable || d.Disk != "mmcblk0" {
		t.Errorf("mmcblk0p1 = %+v", d)
	}
	if want := filepath.Join(s.root, "dev", "sdb1"); byName["sdb1"].Path != want {
		t.Errorf("path %q, want %q", byName["sdb1"].Path, want)
	}
	parts, err := diskPartitions(s.root, "sda")
	if err != nil || !reflect.DeepEqual(names(parts), []string{"sda1", "sda2"}) {
		t.Errorf("diskPartitions = %v, %v", names(parts), err)
	}
}

func TestParseMediaSpec(t *testing.T) {
	cases := map[string]mediaSpec{
		"":                                   {},
		"quiet savior.media=none":            {Raw: "none", None: true},
		"savior.media=UUID=abcd-1234 quiet":  {Raw: "UUID=abcd-1234", UUID: "ABCD1234"},
		"savior.media=uuid=ABCD-1234":        {Raw: "uuid=ABCD-1234", UUID: "ABCD1234"},
		"savior.media=ABCD-1234":             {Raw: "ABCD-1234", UUID: "ABCD1234"},
		`savior.media="LABEL=MY STICK"`:      {Raw: "LABEL=MY STICK", Label: "MY STICK"},
		"savior.media=LABEL=MY%20STICK":      {Raw: "LABEL=MY STICK", Label: "MY STICK"},
		"savior.media=none savior.media=1-2": {Raw: "1-2", UUID: "12"},
		"xsavior.media=none":                 {},
	}
	for in, want := range cases {
		if got := parseMediaSpec(in); got != want {
			t.Errorf("parseMediaSpec(%q) = %+v, want %+v", in, got, want)
		}
	}
	if p := parseMediaSpec("savior.media=UUID=ABCD-12EF").markerPrefix(); p != "savior-abcd12ef" {
		t.Errorf("markerPrefix %q", p)
	}
	if p := parseMediaSpec("savior.media=UUID=0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0").markerPrefix(); p != "" {
		t.Errorf("markerPrefix for ext UUID %q", p)
	}
}

// fakeClock drives finder.find without real sleeping.
type fakeClock struct {
	now    time.Time
	sleeps int
	onTick func(n int)
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(d time.Duration) {
	c.now = c.now.Add(d)
	c.sleeps++
	if c.onTick != nil {
		c.onTick(c.sleeps)
	}
}

type inspectResult struct{ conf, marker bool }

func testFinder(s *fakeSys, cmdline string, contents map[string]inspectResult) (*finder, *fakeClock, *[]string) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	var inspected []string
	f := newFinder(s.root, parseMediaSpec(cmdline), sysOps{}, func(string, ...any) {})
	f.now, f.sleep = clk.Now, clk.Sleep
	f.inspect = func(c *candidate) (bool, bool, error) {
		inspected = append(inspected, c.Dev.Name)
		r := contents[c.Dev.Name]
		return r.conf, r.marker, nil
	}
	return f, clk, &inspected
}

func TestFindExactImmediately(t *testing.T) {
	s := newFakeSys(t)
	s.disk("sda", ataPath, false, 1<<20, nil)
	s.part("sda", ataPath, 1, 2048, 1<<19, fat16Image(t, "SAVIOR", "", 0x11112222)) // old stick dd'd to disk
	s.disk("sdb", usbPath, true, 1<<20, make([]byte, 4096))
	s.part("sdb", usbPath, 1, 2048, 1<<19, fat16Image(t, "SAVIOR", "", 0xAABBCCDD))
	f, clk, _ := testFinder(s, "quiet savior.media=UUID=AABB-CCDD", nil)
	c := f.find(20 * time.Second)
	if c == nil || c.Dev.Name != "sdb1" || c.Rank != rankExact {
		t.Fatalf("got %v", c)
	}
	if clk.sleeps != 0 {
		t.Errorf("slept %d times", clk.sleeps)
	}
}

func TestFindWaitsForExactThenFallsBack(t *testing.T) {
	s := newFakeSys(t)
	s.disk("sda", ataPath, false, 1<<20, nil)
	s.part("sda", ataPath, 1, 2048, 1<<19, fat16Image(t, "SAVIOR", "", 0x11112222))
	f, clk, _ := testFinder(s, "savior.media=UUID=AABB-CCDD", nil)
	c := f.find(20 * time.Second)
	if c == nil || c.Dev.Name != "sda1" || c.Rank != rankLabel {
		t.Fatalf("got %v", c)
	}
	if clk.sleeps != 40 {
		t.Errorf("slept %d times, want the full 20 s (40 polls)", clk.sleeps)
	}
}

func TestFindDeviceAppearsLater(t *testing.T) {
	s := newFakeSys(t)
	s.disk("sda", ataPath, false, 1<<20, nil)
	s.part("sda", ataPath, 1, 2048, 1<<19, fat16Image(t, "SAVIOR", "", 0x11112222))
	f, clk, _ := testFinder(s, "savior.media=UUID=AABB-CCDD", nil)
	clk.onTick = func(n int) {
		if n == 7 { // the USB stick shows up after 3.5 s
			s.disk("sdb", usbPath, true, 1<<20, make([]byte, 4096))
			s.part("sdb", usbPath, 1, 2048, 1<<19, fat16Image(t, "SAVIOR", "", 0xAABBCCDD))
		}
	}
	c := f.find(20 * time.Second)
	if c == nil || c.Dev.Name != "sdb1" {
		t.Fatalf("got %v", c)
	}
	if clk.sleeps != 7 {
		t.Errorf("slept %d times, want 7", clk.sleeps)
	}
}

func TestFindNoSpecLabelImmediately(t *testing.T) {
	s := newFakeSys(t)
	s.disk("sdb", usbPath, true, 1<<20, fat32Image(t, "NO NAME", "SAVIOR", 1)) // superfloppy
	f, clk, _ := testFinder(s, "", nil)
	if c := f.find(20 * time.Second); c == nil || c.Dev.Name != "sdb" || c.Rank != rankLabel {
		t.Fatalf("got %v", c)
	}
	if clk.sleeps != 0 {
		t.Errorf("slept %d", clk.sleeps)
	}
}

func TestFindCDBootPrefersPlainStick(t *testing.T) {
	s := newFakeSys(t)
	s.disk("sr0", cdPath, true, 1<<20, isoImage("SAVIOR", "2026092312345600"))
	s.disk("sdc", usbPath, true, 1<<20, make([]byte, 4096))
	s.part("sdc", usbPath, 1, 2048, 1<<19, fat16Image(t, "NO NAME", "KINGSTON", 5))
	// A fixed disk's FAT partition with savior.conf is never considered.
	s.disk("sda", ataPath, false, 1<<20, nil)
	s.part("sda", ataPath, 1, 2048, 1<<19, fat16Image(t, "NO NAME", "", 6))
	contents := map[string]inspectResult{"sr0": {conf: true, marker: true}, "sdc1": {conf: true}, "sda1": {conf: true}}
	f, clk, inspected := testFinder(s, "savior.media=UUID=AABB-CCDD", contents)
	c := f.find(20 * time.Second)
	if c == nil || c.Dev.Name != "sdc1" || c.Rank != rankPlain {
		t.Fatalf("got %v", c)
	}
	// The booted CD was recognized by its marker: only the settle time passed.
	if clk.sleeps != 4 {
		t.Errorf("slept %d times, want 4 (2 s settle)", clk.sleeps)
	}
	for _, n := range *inspected {
		if n == "sda1" {
			t.Error("trial-mounted a fixed disk")
		}
	}
	if len(*inspected) != 2 {
		t.Errorf("inspected %v, want each portable candidate once", *inspected)
	}
}

func TestFindCDOnly(t *testing.T) {
	s := newFakeSys(t)
	s.disk("sr0", cdPath, true, 1<<20, isoImage("SAVIOR", "2026092312345600"))
	f, _, _ := testFinder(s, "savior.media=UUID=AABB-CCDD", map[string]inspectResult{"sr0": {conf: true, marker: true}})
	if c := f.find(20 * time.Second); c == nil || c.Dev.Name != "sr0" || c.Rank != rankOptical || !c.Marker {
		t.Fatalf("got %v", c)
	}
	// Without a spec, the CD is taken after settling as well.
	f, clk, _ := testFinder(s, "", map[string]inspectResult{"sr0": {conf: true}})
	if c := f.find(20 * time.Second); c == nil || c.Dev.Name != "sr0" || clk.sleeps != 4 {
		t.Fatalf("got %v after %d sleeps", c, clk.sleeps)
	}
}

func TestFindISOHybridStick(t *testing.T) {
	// An ISO written to a USB stick: iso9660 on the whole disk plus an EFI
	// FAT partition without savior.conf.
	s := newFakeSys(t)
	s.disk("sdb", usbPath, true, 1<<20, isoImage("SAVIOR", "2026092312345600"))
	s.part("sdb", usbPath, 2, 100, 5000, fat16Image(t, "NO NAME", "", 7))
	f, _, _ := testFinder(s, "savior.media=UUID=AABB-CCDD", map[string]inspectResult{"sdb": {conf: true, marker: true}})
	if c := f.find(20 * time.Second); c == nil || c.Dev.Name != "sdb" {
		t.Fatalf("got %v", c)
	}
}

func TestFindLabelSpecAndNothing(t *testing.T) {
	s := newFakeSys(t)
	s.disk("sdb", usbPath, true, 1<<20, fat16Image(t, "CONFIGS", "", 7))
	f, _, _ := testFinder(s, "savior.media=LABEL=configs", nil)
	if c := f.find(5 * time.Second); c == nil || c.Dev.Name != "sdb" || c.Rank != rankExact {
		t.Fatalf("got %v", c)
	}
	s2 := newFakeSys(t)
	s2.disk("sda", ataPath, false, 1<<20, extImage("root", 0, 0x2, 0))
	f, clk, _ := testFinder(s2, "", nil)
	if c := f.find(3 * time.Second); c != nil {
		t.Fatalf("got %v, want nothing", c)
	}
	if clk.sleeps != 6 {
		t.Errorf("slept %d", clk.sleeps)
	}
}

func TestFindMediaMountOptions(t *testing.T) {
	s := newFakeSys(t)
	s.disk("sdb", usbPath, true, 1<<20, make([]byte, 4096))
	s.part("sdb", usbPath, 1, 2048, 1<<19, fat16Image(t, "SAVIOR", "", 0xAABBCCDD))
	os.WriteFile(filepath.Join(s.root, "cmdline"), []byte("savior.media=UUID=AABB-CCDD\n"), 0o644)
	type call struct {
		src, dst, fstype string
		o                mountOpts
	}
	var calls []call
	ops := sysOps{mount: func(src, dst, fstype string, o mountOpts) error {
		calls = append(calls, call{src, dst, fstype, o})
		if strings.Contains(o.Data, "iocharset") {
			return errors.New("invalid argument") // kernel without NLS
		}
		return nil
	}}
	mp := filepath.Join(s.root, "media", "savior")
	var out, errb bytes.Buffer
	rc := findMedia(findMediaOpts{cmdlineFile: filepath.Join(s.root, "cmdline"), wait: time.Second,
		mountpoint: mp, sysRoot: s.root}, ops, &out, &errb)
	if rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	dev := filepath.Join(s.root, "dev", "sdb1")
	if got := out.String(); got != dev+" vfat\n" {
		t.Errorf("stdout %q", got)
	}
	want := []call{
		{dev, mp, "vfat", mountOpts{ReadOnly: true, NoSuid: true, NoDev: true, NoExec: true,
			Data: "uid=0,gid=0,fmask=0177,dmask=0077,iocharset=utf8,codepage=437"}},
		{dev, mp, "vfat", mountOpts{ReadOnly: true, NoSuid: true, NoDev: true, NoExec: true,
			Data: "uid=0,gid=0,fmask=0177,dmask=0077"}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("mount calls\n%+v\nwant\n%+v", calls, want)
	}
	if st, err := os.Stat(mp); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("mountpoint %v %v", st, err)
	}

	// Already mounted: reported, nothing mounted again.
	os.WriteFile(filepath.Join(s.root, "proc/self/mountinfo"),
		[]byte("36 25 8:17 / "+mp+" ro,nosuid,nodev,noexec - vfat /dev/sdb1 ro,fmask=0177\n"), 0o644)
	calls = nil
	out.Reset()
	if rc := findMedia(findMediaOpts{wait: time.Second, mountpoint: mp, sysRoot: s.root}, ops, &out, &errb); rc != 0 || len(calls) != 0 || out.String() != "/dev/sdb1 vfat\n" {
		t.Errorf("already mounted: rc %d calls %v out %q", rc, calls, out.String())
	}

	// savior.media=none: not found, nothing probed.
	os.WriteFile(filepath.Join(s.root, "cmdline"), []byte("savior.media=none"), 0o644)
	if rc := findMedia(findMediaOpts{cmdlineFile: filepath.Join(s.root, "cmdline"), wait: time.Second,
		mountpoint: "", sysRoot: s.root}, ops, &out, &errb); rc != 1 {
		t.Errorf("media=none: rc %d", rc)
	}
}

func TestMediaMountAttempts(t *testing.T) {
	vfat := mediaMountAttempts(FSInfo{Type: "vfat"})
	if len(vfat) != 3 || vfat[0].data != "uid=0,gid=0,fmask=0177,dmask=0077,iocharset=utf8,codepage=437" ||
		!strings.HasSuffix(vfat[2].data, "iocharset=cp437") {
		t.Errorf("vfat attempts %+v", vfat)
	}
	iso := mediaMountAttempts(FSInfo{Type: "iso9660"})
	if len(iso) != 3 || !strings.Contains(iso[0].data, "dmode=0700") {
		t.Errorf("iso attempts %+v", iso)
	}
	ext := mediaMountAttempts(FSInfo{Type: "ext2"})
	if len(ext) != 2 || ext[0].fstype != "ext4" || ext[1].fstype != "ext2" {
		t.Errorf("ext attempts %+v", ext)
	}
	if mediaMountAttempts(FSInfo{Type: "ntfs"}) != nil {
		t.Error("ntfs")
	}
}

func TestInspectTree(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "boot"), 0o755)
	os.WriteFile(filepath.Join(root, "boot", "savior-aabbccdd0011aa22.id"), []byte("v1\n"), 0o644)
	conf, marker, _ := inspectTree(root, "savior-aabbccdd")
	if conf || !marker {
		t.Errorf("conf %v marker %v", conf, marker)
	}
	os.WriteFile(filepath.Join(root, "savior.conf"), nil, 0o644)
	conf, marker, _ = inspectTree(root, "savior-11111111")
	if !conf || marker {
		t.Errorf("conf %v marker %v", conf, marker)
	}
}

func TestMountInfo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mountinfo")
	os.WriteFile(p, []byte(`22 1 0:21 / /proc rw,nosuid - proc proc rw
36 25 8:17 / /media/savior ro,nosuid,nodev,noexec - vfat /dev/sdb1 ro,fmask=0177
40 25 8:18 / /var/lib/savior/data rw,nosuid shared:5 - ext4 /dev/sdb2 rw
41 25 8:33 / /mnt/with\040space rw - ext4 /dev/sdc1 rw
garbage line
`), 0o644)
	mi, err := ReadMountInfo(p)
	if err != nil || len(mi) != 4 {
		t.Fatalf("%v %v", mi, err)
	}
	if e, ok := findMount(mi, "/media/savior/"); !ok || e.MajMin != "8:17" || e.FSType != "vfat" || e.Source != "/dev/sdb1" {
		t.Errorf("media %+v %v", e, ok)
	}
	if e, ok := findMount(mi, "/var/lib/savior/data"); !ok || e.FSType != "ext4" {
		t.Errorf("data %+v", e)
	}
	if _, ok := findMount(mi, "/mnt/with space"); !ok {
		t.Error("escaped mount point")
	}
	if _, ok := findMount(mi, "/media"); ok {
		t.Error("prefix matched")
	}
}
