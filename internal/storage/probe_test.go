package storage

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fat16Image builds a minimal FAT16 volume: boot sector plus an empty root
// directory, optionally with a volume label entry.
func fat16Image(t *testing.T, bpbLabel, rootLabel string, serial uint32) []byte {
	t.Helper()
	img := make([]byte, 128<<10)
	b := img
	copy(b[0:], []byte{0xEB, 0x3C, 0x90})
	copy(b[3:], "MSWIN4.1")
	binary.LittleEndian.PutUint16(b[11:], 512) // bytes per sector
	b[13] = 4                                  // sectors per cluster
	binary.LittleEndian.PutUint16(b[14:], 1)   // reserved
	b[16] = 2                                  // FATs
	binary.LittleEndian.PutUint16(b[17:], 512) // root entries
	binary.LittleEndian.PutUint16(b[19:], 256) // total sectors
	b[21] = 0xF8
	binary.LittleEndian.PutUint16(b[22:], 1) // sectors per FAT
	b[38] = 0x29
	binary.LittleEndian.PutUint32(b[39:], serial)
	copy(b[43:54], padLabel(bpbLabel))
	copy(b[54:62], "FAT16   ")
	b[510], b[511] = 0x55, 0xAA
	if rootLabel != "" {
		root := (1 + 2*1) * 512
		copy(img[root:root+11], padLabel(rootLabel))
		img[root+11] = 0x08
	}
	return img
}

// fat32Image builds a minimal FAT32 volume whose root directory (cluster 2)
// holds an LFN entry, a deleted entry and then the volume label.
func fat32Image(t *testing.T, bpbLabel, rootLabel string, serial uint32) []byte {
	t.Helper()
	const bps, spc, reserved, fatSize = 512, 1, 32, 8
	img := make([]byte, 96<<10)
	b := img
	copy(b[0:], []byte{0xEB, 0x58, 0x90})
	copy(b[3:], "mkfs.fat")
	binary.LittleEndian.PutUint16(b[11:], bps)
	b[13] = spc
	binary.LittleEndian.PutUint16(b[14:], reserved)
	b[16] = 2
	b[21] = 0xF8
	binary.LittleEndian.PutUint32(b[32:], 2048)    // total sectors
	binary.LittleEndian.PutUint32(b[36:], fatSize) // sectors per FAT
	binary.LittleEndian.PutUint32(b[44:], 2)       // root cluster
	b[66] = 0x29
	binary.LittleEndian.PutUint32(b[67:], serial)
	copy(b[71:82], padLabel(bpbLabel))
	copy(b[82:90], "FAT32   ")
	b[510], b[511] = 0x55, 0xAA
	root := (reserved + 2*fatSize) * bps
	e := img[root:]
	copy(e[0:11], "AB         ") // LFN part
	e[11] = 0x0F
	e = e[32:]
	e[0] = 0xE5 // deleted
	e[11] = 0x08
	e = e[32:]
	if rootLabel != "" {
		copy(e[0:11], padLabel(rootLabel))
		e[11] = 0x08
	}
	return img
}

func padLabel(s string) []byte {
	return []byte((s + "           ")[:11])
}

func extImage(label string, compat, incompat, roCompat uint32) []byte {
	img := make([]byte, 8<<10)
	sb := img[1024:]
	binary.LittleEndian.PutUint16(sb[56:], extMagic)
	binary.LittleEndian.PutUint32(sb[24:], 2) // 4 KiB blocks
	binary.LittleEndian.PutUint32(sb[92:], compat)
	binary.LittleEndian.PutUint32(sb[96:], incompat)
	binary.LittleEndian.PutUint32(sb[100:], roCompat)
	copy(sb[104:120], []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef})
	copy(sb[120:136], label)
	return img
}

func isoImage(label, created string) []byte {
	img := make([]byte, 64<<10)
	pvd := img[isoPVDOffset:]
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[40:72], (label + strings.Repeat(" ", 32))[:32])
	copy(pvd[813:830], created)
	return img
}

func TestProbeSynthetic(t *testing.T) {
	grubMBR := make([]byte, 64<<10)
	copy(grubMBR, []byte{0xEB, 0x63, 0x90})
	grubMBR[510], grubMBR[511] = 0x55, 0xAA

	tests := []struct {
		name string
		img  []byte
		want FSInfo
	}{
		{"fat16 bpb label", fat16Image(t, "SAVIOR", "", 0xAABBCCDD), FSInfo{"vfat", "SAVIOR", "AABB-CCDD"}},
		{"fat16 root label wins", fat16Image(t, "NO NAME", "MYSTICK", 0x01020304), FSInfo{"vfat", "MYSTICK", "0102-0304"}},
		{"fat16 no name", fat16Image(t, "NO NAME", "", 0x01020304), FSInfo{"vfat", "", "0102-0304"}},
		{"fat32 root label", fat32Image(t, "NO NAME", "SAVIOR", 0xDEADBEEF), FSInfo{"vfat", "SAVIOR", "DEAD-BEEF"}},
		{"fat32 bpb label only", fat32Image(t, "SAVIOR", "", 0x1234ABCD), FSInfo{"vfat", "SAVIOR", "1234-ABCD"}},
		{"ext2", extImage("scratch", 0, 0x2, 0), FSInfo{"ext2", "scratch", "12345678-9abc-def0-0123-456789abcdef"}},
		{"ext3", extImage("", extCompatHasJournal, 0x2, 0), FSInfo{"ext3", "", "12345678-9abc-def0-0123-456789abcdef"}},
		{"ext4", extImage("SAVIOR-DATA", extCompatHasJournal, 0x2|extIncompatExtents|extIncompatFlexBG, 0), FSInfo{"ext4", "SAVIOR-DATA", "12345678-9abc-def0-0123-456789abcdef"}},
		{"iso", isoImage("SAVIOR", "2026092312345600\x00"), FSInfo{"iso9660", "SAVIOR", "2026-09-23-12-34-56-00"}},
		{"iso no date", isoImage("CDROM", "0000000000000000\x00"), FSInfo{"iso9660", "CDROM", ""}},
		{"grub mbr", grubMBR, FSInfo{}},
		{"zeros", make([]byte, 4096), FSInfo{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Probe(bytes.NewReader(tc.img))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProbeRejects(t *testing.T) {
	// Journal device, bad FAT fields, short input.
	jdev := extImage("", 0, extIncompatJournalDev, 0)
	if fi, _ := Probe(bytes.NewReader(jdev)); fi.Type != "" {
		t.Errorf("journal device probed as %q", fi.Type)
	}
	bad := fat16Image(t, "X", "", 1)
	binary.LittleEndian.PutUint16(bad[11:], 500)
	if fi, _ := Probe(bytes.NewReader(bad)); fi.Type != "" {
		t.Errorf("bad bytes/sector probed as %q", fi.Type)
	}
	bad = fat16Image(t, "X", "", 1)
	bad[13] = 3
	if fi, _ := Probe(bytes.NewReader(bad)); fi.Type != "" {
		t.Errorf("bad sectors/cluster probed as %q", fi.Type)
	}
	ntfs := make([]byte, 4096)
	copy(ntfs, []byte{0xEB, 0x52, 0x90})
	copy(ntfs[3:], "NTFS    ")
	binary.LittleEndian.PutUint16(ntfs[11:], 512)
	ntfs[13] = 8
	ntfs[21] = 0xF8
	if fi, _ := Probe(bytes.NewReader(ntfs)); fi.Type != "" {
		t.Errorf("NTFS probed as %q", fi.Type)
	}
	if _, err := Probe(bytes.NewReader(make([]byte, 100))); err == nil {
		t.Error("short device: want error")
	}
}

// TestProbeRealImages formats images with the real tools (when installed)
// and compares Probe with blkid's view.
func TestProbeRealImages(t *testing.T) {
	dir := t.TempDir()
	type img struct {
		name string
		make func(path string) error
		want FSInfo
	}
	run := func(argv ...string) error {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			t.Logf("%v: %s", argv, out)
		}
		return err
	}
	sized := func(path string, mb int64) error {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Truncate(mb << 20)
	}
	images := []img{
		{"fat32", func(p string) error {
			if err := sized(p, 64); err != nil {
				return err
			}
			return run("mkfs.fat", "-F", "32", "-n", "SAVIOR", "-i", "AABBCCDD", p)
		}, FSInfo{"vfat", "SAVIOR", "AABB-CCDD"}},
		{"fat16", func(p string) error {
			if err := sized(p, 16); err != nil {
				return err
			}
			return run("mkfs.fat", "-F", "16", "-n", "STICK", "-i", "01234567", p)
		}, FSInfo{"vfat", "STICK", "0123-4567"}},
		{"fat12", func(p string) error {
			if err := sized(p, 2); err != nil {
				return err
			}
			return run("mkfs.fat", "-F", "12", "-i", "0000BEEF", p)
		}, FSInfo{"vfat", "", "0000-BEEF"}},
		{"ext4", func(p string) error {
			if err := sized(p, 32); err != nil {
				return err
			}
			return run("mkfs.ext4", "-q", "-F", "-L", "SAVIOR-DATA", "-U", "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", p)
		}, FSInfo{"ext4", "SAVIOR-DATA", "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"}},
		{"ext3", func(p string) error {
			if err := sized(p, 32); err != nil {
				return err
			}
			return run("mke2fs", "-q", "-F", "-t", "ext3", "-L", "old", "-U", "11111111-2222-3333-4444-555555555555", p)
		}, FSInfo{"ext3", "old", "11111111-2222-3333-4444-555555555555"}},
		{"ext2-busybox", func(p string) error {
			if err := sized(p, 8); err != nil {
				return err
			}
			return run("busybox", "mke2fs", "-F", "-L", "BBX", p)
		}, FSInfo{"ext2", "BBX", "?"}},
		{"iso", func(p string) error {
			src := filepath.Join(dir, "isosrc")
			os.MkdirAll(filepath.Join(src, "boot"), 0o755)
			os.WriteFile(filepath.Join(src, "savior.conf"), []byte("x"), 0o644)
			return run("xorriso", "-as", "mkisofs", "-quiet", "-o", p, "-V", "SAVIOR", "-r", "-J", src)
		}, FSInfo{"iso9660", "SAVIOR", "?"}},
	}
	for _, im := range images {
		t.Run(im.name, func(t *testing.T) {
			p := filepath.Join(dir, im.name+".img")
			if err := im.make(p); err != nil {
				t.Skipf("cannot build image (tool missing?): %v", err)
			}
			got, err := ProbeFile(p)
			if err != nil {
				t.Fatal(err)
			}
			want := im.want
			if want.UUID == "?" {
				want.UUID = got.UUID
				if got.UUID == "" && im.name != "ext2-busybox" {
					t.Errorf("empty UUID")
				}
			}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
			// Cross-check with blkid when available.
			if _, err := exec.LookPath("blkid"); err == nil {
				out, err := exec.Command("blkid", "-p", "-o", "export", p).Output()
				if err == nil {
					kv := map[string]string{}
					for _, l := range strings.Split(string(out), "\n") {
						if k, v, ok := strings.Cut(l, "="); ok {
							kv[k] = v
						}
					}
					if kv["TYPE"] != got.Type || kv["LABEL"] != got.Label || kv["UUID"] != got.UUID {
						t.Errorf("blkid says TYPE=%q LABEL=%q UUID=%q, Probe says %+v", kv["TYPE"], kv["LABEL"], kv["UUID"], got)
					}
				}
			}
		})
	}
}
