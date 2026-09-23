package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// FSInfo describes a filesystem found by Probe. Type is "" when nothing was
// recognized.
type FSInfo struct {
	Type  string // vfat, iso9660, ext2, ext3, ext4
	Label string
	// UUID uses the same format as blkid: vfat "ABCD-1234" (volume serial),
	// ext "8-4-4-4-12" lowercase hex, iso9660 "YYYY-MM-DD-HH-MM-SS-cc"
	// (volume creation time).
	UUID string
}

// IsExt reports whether the filesystem is ext2, ext3 or ext4.
func (f FSInfo) IsExt() bool { return strings.HasPrefix(f.Type, "ext") }

// probeSize is how much of a device Probe reads up front: enough for the
// FAT boot sector, the ext superblock at 1 KiB and the ISO 9660 primary
// volume descriptor at 32 KiB.
const probeSize = 64 << 10

// Probe identifies the filesystem on r by its superblock. It never depends on
// blkid. A device too short or unreadable returns an error; an unrecognized
// filesystem returns FSInfo{} and no error.
func Probe(r io.ReaderAt) (FSInfo, error) {
	buf := make([]byte, probeSize)
	n, err := r.ReadAt(buf, 0)
	if n < 512 {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return FSInfo{}, err
	}
	buf = buf[:n]
	if fi, ok := probeISO9660(buf); ok {
		return fi, nil
	}
	if fi, ok := probeExt(buf); ok {
		return fi, nil
	}
	if fi, ok := probeFAT(buf, r); ok {
		return fi, nil
	}
	return FSInfo{}, nil
}

// ProbeFile opens path read-only and probes it.
func ProbeFile(path string) (FSInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return FSInfo{}, err
	}
	defer f.Close()
	return Probe(f)
}

// ---------------------------------------------------------------------------
// ISO 9660

const isoPVDOffset = 16 * 2048

func probeISO9660(b []byte) (FSInfo, bool) {
	if len(b) < isoPVDOffset+2048 {
		return FSInfo{}, false
	}
	pvd := b[isoPVDOffset : isoPVDOffset+2048]
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" || pvd[6] != 1 {
		return FSInfo{}, false
	}
	fi := FSInfo{Type: "iso9660", Label: strings.TrimRight(string(pvd[40:72]), " \x00")}
	// blkid derives the UUID from the creation time, else the modification
	// time: 16 ASCII digits (YYYYMMDDHHMMSScc) plus a timezone byte.
	for _, off := range []int{813, 830} {
		if u, ok := isoDateUUID(pvd[off : off+16]); ok {
			fi.UUID = u
			break
		}
	}
	return fi, true
}

func isoDateUUID(d []byte) (string, bool) {
	allZero := true
	for _, c := range d {
		if c < '0' || c > '9' {
			return "", false
		}
		if c != '0' {
			allZero = false
		}
	}
	if allZero {
		return "", false
	}
	s := string(d)
	return fmt.Sprintf("%s-%s-%s-%s-%s-%s-%s", s[0:4], s[4:6], s[6:8], s[8:10], s[10:12], s[12:14], s[14:16]), true
}

// ---------------------------------------------------------------------------
// ext2/3/4

const (
	extSuperOffset = 1024
	extMagic       = 0xEF53

	extCompatHasJournal    = 0x0004
	extIncompatJournalDev  = 0x0008
	extIncompatExtents     = 0x0040
	extIncompat64Bit       = 0x0080
	extIncompatFlexBG      = 0x0200
	extRoCompatHugeFile    = 0x0008
	extRoCompatGdtCsum     = 0x0010
	extRoCompatDirNlink    = 0x0020
	extRoCompatExtraIsize  = 0x0040
	extRoCompatMetadataCsm = 0x0400
)

func probeExt(b []byte) (FSInfo, bool) {
	if len(b) < extSuperOffset+1024 {
		return FSInfo{}, false
	}
	sb := b[extSuperOffset : extSuperOffset+1024]
	if binary.LittleEndian.Uint16(sb[56:]) != extMagic {
		return FSInfo{}, false
	}
	// s_log_block_size: block size is 1024 << n, at most 64 KiB.
	if binary.LittleEndian.Uint32(sb[24:]) > 6 {
		return FSInfo{}, false
	}
	compat := binary.LittleEndian.Uint32(sb[92:])
	incompat := binary.LittleEndian.Uint32(sb[96:])
	roCompat := binary.LittleEndian.Uint32(sb[100:])
	if incompat&extIncompatJournalDev != 0 {
		return FSInfo{}, false // an external journal, not a filesystem
	}
	typ := "ext2"
	switch {
	case incompat&(extIncompatExtents|extIncompat64Bit|extIncompatFlexBG) != 0,
		roCompat&(extRoCompatHugeFile|extRoCompatGdtCsum|extRoCompatDirNlink|extRoCompatExtraIsize|extRoCompatMetadataCsm) != 0:
		typ = "ext4"
	case compat&extCompatHasJournal != 0:
		typ = "ext3"
	}
	u := sb[104:120]
	fi := FSInfo{Type: typ, Label: cString(sb[120:136])}
	if !allBytes(u, 0) {
		fi.UUID = fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
	}
	return fi, true
}

// ---------------------------------------------------------------------------
// FAT12/16/32

func probeFAT(b []byte, r io.ReaderAt) (FSInfo, bool) {
	if len(b) < 512 {
		return FSInfo{}, false
	}
	if !((b[0] == 0xEB && b[2] == 0x90) || b[0] == 0xE9) {
		return FSInfo{}, false
	}
	bps := int(binary.LittleEndian.Uint16(b[11:]))
	spc := int(b[13])
	reserved := int(binary.LittleEndian.Uint16(b[14:]))
	nfats := int(b[16])
	rootEntries := int(binary.LittleEndian.Uint16(b[17:]))
	media := b[21]
	fat16Size := int(binary.LittleEndian.Uint16(b[22:]))
	fat32Size := int(binary.LittleEndian.Uint32(b[36:]))
	switch bps {
	case 512, 1024, 2048, 4096:
	default:
		return FSInfo{}, false
	}
	if spc == 0 || spc&(spc-1) != 0 || reserved == 0 || nfats == 0 || nfats > 4 {
		return FSInfo{}, false
	}
	if media != 0xF0 && media < 0xF8 {
		return FSInfo{}, false
	}
	var serial, bpbLabel []byte
	var rootOff int64
	var rootLen int
	switch {
	case fat16Size == 0 && fat32Size != 0: // FAT32
		if b[66] == 0x29 || b[66] == 0x28 {
			serial = b[67:71]
			if b[66] == 0x29 {
				bpbLabel = b[71:82]
			}
		}
		rootCluster := int64(binary.LittleEndian.Uint32(b[44:]))
		if rootCluster >= 2 {
			firstData := int64(reserved) + int64(nfats)*int64(fat32Size)
			rootOff = (firstData + (rootCluster-2)*int64(spc)) * int64(bps)
			rootLen = spc * bps
		}
	case fat16Size != 0: // FAT12/16
		if b[38] == 0x29 || b[38] == 0x28 {
			serial = b[39:43]
			if b[38] == 0x29 {
				bpbLabel = b[43:54]
			}
		}
		rootOff = (int64(reserved) + int64(nfats)*int64(fat16Size)) * int64(bps)
		rootLen = rootEntries * 32
	default:
		return FSInfo{}, false
	}
	fi := FSInfo{Type: "vfat"}
	if serial != nil {
		fi.UUID = fmt.Sprintf("%02X%02X-%02X%02X", serial[3], serial[2], serial[1], serial[0])
	}
	// Like blkid, prefer the volume label entry in the root directory (what
	// Windows and mlabel change) over the copy in the boot sector.
	if l, ok := fatRootLabel(r, rootOff, rootLen); ok {
		fi.Label = l
	} else if bpbLabel != nil {
		fi.Label = fatLabel(bpbLabel)
	}
	return fi, true
}

// fatRootLabel scans the first root directory cluster (at most 64 KiB) for a
// volume label entry.
func fatRootLabel(r io.ReaderAt, off int64, n int) (string, bool) {
	if r == nil || off <= 0 || n <= 0 {
		return "", false
	}
	if n > 64<<10 {
		n = 64 << 10
	}
	buf := make([]byte, n)
	got, _ := r.ReadAt(buf, off)
	buf = buf[:got-got%32]
	for i := 0; i < len(buf); i += 32 {
		e := buf[i : i+32]
		switch {
		case e[0] == 0x00:
			return "", false // end of directory
		case e[0] == 0xE5:
			continue // deleted
		case e[11] == 0x0F:
			continue // long file name part
		case e[11]&0x08 != 0 && e[11]&0x10 == 0:
			l := fatLabel(e[0:11])
			return l, l != ""
		}
	}
	return "", false
}

func fatLabel(b []byte) string {
	l := strings.TrimRight(string(b), " \x00")
	if l == "NO NAME" {
		return ""
	}
	if len(l) > 0 && l[0] == 0x05 { // 0xE5 stored as 0x05 in directory entries
		l = "\xe5" + l[1:]
	}
	return l
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func allBytes(b []byte, v byte) bool {
	for _, c := range b {
		if c != v {
			return false
		}
	}
	return true
}

// normUUID canonicalizes a UUID/serial for comparison: uppercase, no dashes.
func normUUID(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}
