package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// MBR layout.
const (
	mbrSize        = 512
	mbrTableOffset = 446
	mbrEntrySize   = 16
	mbrSlots       = 4

	partTypeLinux = 0x83
	partTypeGPT   = 0xEE

	mbrMaxSectors = 1 << 32 // MBR LBA fields are 32-bit
	alignBytes    = 1 << 20 // new partitions start on a 1 MiB boundary
)

// PartEntry is one primary partition table entry.
type PartEntry struct {
	Slot     int // 0..3; the kernel names it partition Slot+1
	Status   byte
	Type     byte
	CHSStart [3]byte
	CHSEnd   [3]byte
	StartLBA uint32
	Sectors  uint32
	raw      [mbrEntrySize]byte
}

// Empty reports whether the entry is unused (type 0).
func (p PartEntry) Empty() bool { return p.Type == 0 }

// Zero reports whether all 16 bytes of the entry are zero, i.e. it can be
// written without destroying anything.
func (p PartEntry) Zero() bool { return p.raw == [mbrEntrySize]byte{} }

// End is the first sector after the partition.
func (p PartEntry) End() uint64 { return uint64(p.StartLBA) + uint64(p.Sectors) }

// Errors returned by PlanDataPartition. The caller reports them as reasons to
// keep the hive data in RAM.
var (
	ErrNoMBR          = errors.New("no MBR partition table (missing 0x55AA signature)")
	ErrGPT            = errors.New("disk uses GPT; SaviorOS only extends MBR disks")
	ErrExtended       = errors.New("disk has an extended partition; refusing to edit it")
	ErrNoPartitions   = errors.New("disk has no partitions")
	ErrTooMany        = errors.New("disk has more than one partition; refusing to edit it")
	ErrInvalid        = errors.New("partition table looks invalid")
	ErrOverlap        = errors.New("partitions overlap or extend beyond the end of the disk")
	ErrNoSpace        = errors.New("not enough unallocated space after the boot partition")
	ErrNoSlot         = errors.New("no free partition table slot")
	ErrBootMismatch   = errors.New("the partition table does not match the booted partition")
	ErrUnexpectedPart = errors.New("a second partition exists that SaviorOS did not create")
)

// ParseMBR decodes the four primary entries of a 512-byte MBR.
func ParseMBR(mbr []byte) ([mbrSlots]PartEntry, error) {
	var out [mbrSlots]PartEntry
	if len(mbr) < mbrSize || mbr[510] != 0x55 || mbr[511] != 0xAA {
		return out, ErrNoMBR
	}
	for i := range out {
		e := mbr[mbrTableOffset+i*mbrEntrySize : mbrTableOffset+(i+1)*mbrEntrySize]
		p := PartEntry{Slot: i, Status: e[0], Type: e[4]}
		copy(p.CHSStart[:], e[1:4])
		copy(p.CHSEnd[:], e[5:8])
		p.StartLBA = binary.LittleEndian.Uint32(e[8:])
		p.Sectors = binary.LittleEndian.Uint32(e[12:])
		copy(p.raw[:], e)
		out[i] = p
	}
	return out, nil
}

// Geometry describes the disk in logical sectors.
type Geometry struct {
	Sectors    uint64 // disk size in logical sectors
	SectorSize int    // logical sector size in bytes (512 or 4096)
}

// DataPlan is the partition PlanDataPartition decided to add (or found
// already added by an earlier, interrupted run).
type DataPlan struct {
	Slot     int    // table slot 0..3
	PartNum  int    // kernel partition number (Slot+1)
	StartLBA uint64 // in logical sectors
	Sectors  uint64
	Entry    [mbrEntrySize]byte
	Existing bool // the entry is already in the table; nothing to write
}

// Offset is the byte offset of the entry within the MBR.
func (p DataPlan) Offset() int64 { return int64(mbrTableOffset + p.Slot*mbrEntrySize) }

// String describes the plan for logs.
func (p DataPlan) String() string {
	return fmt.Sprintf("partition %d (slot %d): start sector %d, %d sectors", p.PartNum, p.Slot, p.StartLBA, p.Sectors)
}

// PlanDataPartition decides where the SAVIOR-DATA partition goes (DESIGN
// 13.4). The disk must hold exactly one primary partition (the SaviorOS boot
// partition, starting at bootStartLBA when that is non-zero) and at least
// minFreeBytes of unallocated space after it. The new partition (type 0x83)
// starts at the next 1 MiB boundary and runs to the end of the disk (capped
// at the 2^32-sector MBR limit). If the table already holds exactly that
// second partition (an earlier run was interrupted after writing it), the
// plan is returned with Existing set. It is a pure function: mbr is not
// modified.
func PlanDataPartition(mbr []byte, g Geometry, bootStartLBA uint64, minFreeBytes uint64) (DataPlan, error) {
	entries, err := ParseMBR(mbr)
	if err != nil {
		return DataPlan{}, err
	}
	if g.SectorSize < 512 || g.SectorSize&(g.SectorSize-1) != 0 || g.Sectors == 0 {
		return DataPlan{}, fmt.Errorf("invalid disk geometry %d x %d bytes", g.Sectors, g.SectorSize)
	}
	var used []PartEntry
	for _, e := range entries {
		switch e.Type {
		case 0:
			continue
		case partTypeGPT:
			return DataPlan{}, ErrGPT
		case 0x05, 0x0F, 0x85:
			return DataPlan{}, ErrExtended
		}
		if (e.Status != 0x00 && e.Status != 0x80) || e.Sectors == 0 || e.StartLBA == 0 {
			return DataPlan{}, ErrInvalid
		}
		if e.End() > g.Sectors {
			return DataPlan{}, ErrOverlap
		}
		used = append(used, e)
	}
	for i := range used {
		for j := i + 1; j < len(used); j++ {
			a, b := used[i], used[j]
			if uint64(a.StartLBA) < b.End() && uint64(b.StartLBA) < a.End() {
				return DataPlan{}, ErrOverlap
			}
		}
	}
	if len(used) == 0 {
		return DataPlan{}, ErrNoPartitions
	}
	if len(used) > 2 {
		return DataPlan{}, ErrTooMany
	}

	// Identify the boot partition.
	boot := used[0]
	if len(used) == 2 && used[1].StartLBA < boot.StartLBA {
		boot = used[1]
	}
	if bootStartLBA != 0 {
		found := false
		for _, e := range used {
			if uint64(e.StartLBA) == bootStartLBA {
				boot, found = e, true
			}
		}
		if !found {
			return DataPlan{}, ErrBootMismatch
		}
	}

	align := uint64(alignBytes / g.SectorSize)
	if align == 0 {
		align = 1
	}
	start := (boot.End() + align - 1) / align * align
	end := g.Sectors
	if end > mbrMaxSectors {
		end = mbrMaxSectors
	}
	if start >= end || start >= mbrMaxSectors || (end-start)*uint64(g.SectorSize) < minFreeBytes {
		if len(used) == 1 {
			return DataPlan{}, ErrNoSpace
		}
	}

	if len(used) == 2 {
		// Only accept the exact partition this function would have created.
		other := used[0]
		if other.Slot == boot.Slot {
			other = used[1]
		}
		if other.Type == partTypeLinux && uint64(other.StartLBA) == start && other.End() == end {
			return DataPlan{Slot: other.Slot, PartNum: other.Slot + 1, StartLBA: start,
				Sectors: end - start, Entry: other.raw, Existing: true}, nil
		}
		return DataPlan{}, ErrTooMany
	}

	slot := -1
	for i := boot.Slot + 1; i < mbrSlots && slot < 0; i++ {
		if entries[i].Zero() {
			slot = i
		}
	}
	for i := 0; i < mbrSlots && slot < 0; i++ {
		if entries[i].Zero() {
			slot = i
		}
	}
	if slot < 0 {
		return DataPlan{}, ErrNoSlot
	}
	p := DataPlan{Slot: slot, PartNum: slot + 1, StartLBA: start, Sectors: end - start}
	e := &p.Entry
	e[0] = 0x00 // not bootable
	chs := lbaToCHS(start)
	copy(e[1:4], chs[:])
	e[4] = partTypeLinux
	chs = lbaToCHS(end - 1)
	copy(e[5:8], chs[:])
	binary.LittleEndian.PutUint32(e[8:], uint32(start))
	binary.LittleEndian.PutUint32(e[12:], uint32(end-start))
	return p, nil
}

// ApplyPlan returns a copy of mbr with the plan's entry written; every other
// byte is unchanged.
func ApplyPlan(mbr []byte, p DataPlan) []byte {
	out := append([]byte(nil), mbr...)
	copy(out[p.Offset():], p.Entry[:])
	return out
}

// lbaToCHS converts an LBA to the packed CHS triple using the conventional
// 255-head, 63-sector geometry. Addresses beyond cylinder 1023 get the
// LBA-overflow marker 1023/254/63 (FE FF FF), as fdisk writes.
func lbaToCHS(lba uint64) [3]byte {
	const heads, sectors = 255, 63
	c := lba / (heads * sectors)
	if c > 1023 {
		return [3]byte{0xFE, 0xFF, 0xFF}
	}
	h := (lba / sectors) % heads
	s := lba%sectors + 1
	return [3]byte{byte(h), byte(s&0x3F) | byte((c>>2)&0xC0), byte(c & 0xFF)}
}

// HasGPTHeader reports whether sector 1 (lba1, one logical sector) starts
// with the GPT signature.
func HasGPTHeader(lba1 []byte) bool {
	return len(lba1) >= 8 && string(lba1[:8]) == "EFI PART"
}
