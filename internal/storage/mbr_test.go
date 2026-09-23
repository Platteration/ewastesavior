package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

type testEntry struct {
	slot          int
	status, typ   byte
	start, length uint32
	chsS, chsE    [3]byte
}

// makeMBR builds an MBR with GRUB-like boot code, a disk signature and the
// given entries.
func makeMBR(entries ...testEntry) []byte {
	b := make([]byte, 512)
	for i := 0; i < 440; i++ {
		b[i] = byte(i*7 + 3) // boot code that must survive untouched
	}
	copy(b[440:444], []byte{0xDE, 0xAD, 0xBE, 0xEF})
	for _, e := range entries {
		o := mbrTableOffset + e.slot*16
		b[o] = e.status
		copy(b[o+1:o+4], e.chsS[:])
		b[o+4] = e.typ
		copy(b[o+5:o+8], e.chsE[:])
		binary.LittleEndian.PutUint32(b[o+8:], e.start)
		binary.LittleEndian.PutUint32(b[o+12:], e.length)
	}
	b[510], b[511] = 0x55, 0xAA
	return b
}

// savior is the partition mkimage writes: bootable FAT32 LBA at 1 MiB, CHS
// start 0/32/33, CHS end set to the LBA-overflow marker.
func savior(sectors uint32) testEntry {
	return testEntry{slot: 0, status: 0x80, typ: 0x0C, start: 2048, length: sectors,
		chsS: [3]byte{0x20, 0x21, 0x00}, chsE: [3]byte{0xFE, 0xFF, 0xFF}}
}

const (
	mib512 = 2048 // sectors per MiB at 512 bytes
	gib512 = 1024 * mib512
)

func TestLBAToCHS(t *testing.T) {
	cases := map[uint64][3]byte{
		0:                 {0, 1, 0},
		2048:              {0x20, 0x21, 0x00}, // what mkimage writes
		63:                {1, 1, 0},
		16065:             {0, 1, 1},             // cylinder 1
		16065 * 256:       {0, 1 | 0x40, 0},      // cylinder 256: high bits in the sector byte
		16065*1024 - 1:    {254, 63 | 0xC0, 255}, // last addressable: 1023/254/63
		16065 * 1024:      {0xFE, 0xFF, 0xFF},    // overflow marker
		8 * gib512 * 1024: {0xFE, 0xFF, 0xFF},
	}
	for lba, want := range cases {
		if got := lbaToCHS(lba); got != want {
			t.Errorf("lbaToCHS(%d) = % x, want % x", lba, got, want)
		}
	}
}

func TestPlanAppend(t *testing.T) {
	mbr := makeMBR(savior(200 * mib512)) // 200 MiB FAT at 1 MiB
	g := Geometry{Sectors: 8 * gib512, SectorSize: 512}
	p, err := PlanDataPartition(mbr, g, 2048, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if p.Existing || p.Slot != 1 || p.PartNum != 2 {
		t.Fatalf("plan %+v", p)
	}
	if p.StartLBA != 201*mib512 {
		t.Errorf("start %d, want %d (aligned right after the FAT partition)", p.StartLBA, 201*mib512)
	}
	if p.StartLBA%mib512 != 0 {
		t.Errorf("start %d not 1 MiB aligned", p.StartLBA)
	}
	if p.StartLBA+p.Sectors != g.Sectors {
		t.Errorf("end %d, want disk end %d", p.StartLBA+p.Sectors, g.Sectors)
	}
	e := p.Entry
	if e[0] != 0 || e[4] != 0x83 {
		t.Errorf("status %#x type %#x", e[0], e[4])
	}
	if got := binary.LittleEndian.Uint32(e[8:]); uint64(got) != p.StartLBA {
		t.Errorf("entry start %d", got)
	}
	if got := binary.LittleEndian.Uint32(e[12:]); uint64(got) != p.Sectors {
		t.Errorf("entry length %d", got)
	}
	if want := lbaToCHS(p.StartLBA); [3]byte(e[1:4]) != want {
		t.Errorf("CHS start % x, want % x", e[1:4], want)
	}
	if [3]byte(e[5:8]) != [3]byte{0xFE, 0xFF, 0xFF} {
		t.Errorf("CHS end % x, want overflow marker (8 GiB disk)", e[5:8])
	}

	// Applying touches exactly the 16 bytes of slot 1.
	out := ApplyPlan(mbr, p)
	if len(out) != 512 {
		t.Fatal("length changed")
	}
	for i := range out {
		inEntry := i >= 462 && i < 478
		if !inEntry && out[i] != mbr[i] {
			t.Fatalf("byte %d changed outside the entry", i)
		}
	}
	if !bytes.Equal(out[462:478], p.Entry[:]) {
		t.Fatal("entry not written")
	}
	// The input is not modified.
	if mbr[462+4] != 0 {
		t.Fatal("PlanDataPartition/ApplyPlan modified the input")
	}

	// A second plan on the written table recognizes the partition.
	again, err := PlanDataPartition(out, g, 2048, 256<<20)
	if err != nil || !again.Existing || again.Slot != 1 || again.StartLBA != p.StartLBA || again.Sectors != p.Sectors {
		t.Fatalf("replan: %+v, %v", again, err)
	}
}

func TestPlanAlignment(t *testing.T) {
	// FAT partition ending on an odd sector: the new one starts at the next MiB.
	e := savior(100*mib512 + 1)
	p, err := PlanDataPartition(makeMBR(e), Geometry{Sectors: 2 * gib512, SectorSize: 512}, 0, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if p.StartLBA != 102*mib512 {
		t.Errorf("start %d, want %d", p.StartLBA, 102*mib512)
	}
	// Small disk: CHS end is computed, not the marker.
	if p.StartLBA+p.Sectors-1 >= 16065*1024 {
		t.Fatal("test disk too large")
	}
	if got, want := [3]byte(p.Entry[5:8]), lbaToCHS(p.StartLBA+p.Sectors-1); got != want {
		t.Errorf("CHS end % x, want % x", got, want)
	}

	// 4 KiB logical sectors: 1 MiB = 256 sectors.
	e4k := testEntry{slot: 0, status: 0x80, typ: 0x0C, start: 256, length: 25600 + 3}
	p, err = PlanDataPartition(makeMBR(e4k), Geometry{Sectors: 1 << 20, SectorSize: 4096}, 256, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if p.StartLBA%256 != 0 || p.StartLBA != 25856+256 {
		t.Errorf("4k start %d", p.StartLBA)
	}
}

func TestPlanMBRLimit(t *testing.T) {
	// A 4 TiB disk with 512-byte sectors: the partition stops at 2^32 sectors.
	g := Geometry{Sectors: 8 << 30, SectorSize: 512}
	p, err := PlanDataPartition(makeMBR(savior(64*mib512)), g, 0, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if p.StartLBA+p.Sectors != 1<<32 {
		t.Errorf("end %d, want 2^32", p.StartLBA+p.Sectors)
	}
	if binary.LittleEndian.Uint32(p.Entry[12:]) != uint32(p.Sectors) {
		t.Error("length does not fit the entry")
	}
}

func TestPlanRefusals(t *testing.T) {
	g := Geometry{Sectors: 8 * gib512, SectorSize: 512}
	fat := savior(200 * mib512)
	cases := []struct {
		name string
		mbr  []byte
		g    Geometry
		boot uint64
		want error
	}{
		{"no signature", func() []byte { b := makeMBR(fat); b[511] = 0; return b }(), g, 0, ErrNoMBR},
		{"short", make([]byte, 100), g, 0, ErrNoMBR},
		{"gpt protective", makeMBR(testEntry{slot: 0, typ: 0xEE, start: 1, length: 0xFFFFFFFF}), g, 0, ErrGPT},
		{"hybrid gpt", makeMBR(fat, testEntry{slot: 1, typ: 0xEE, start: 1, length: 2047}), g, 0, ErrGPT},
		{"extended", makeMBR(fat, testEntry{slot: 1, typ: 0x05, start: 300 * mib512, length: mib512}), g, 0, ErrExtended},
		{"extended lba", makeMBR(fat, testEntry{slot: 3, typ: 0x0F, start: 300 * mib512, length: mib512}), g, 0, ErrExtended},
		{"linux extended", makeMBR(fat, testEntry{slot: 2, typ: 0x85, start: 300 * mib512, length: mib512}), g, 0, ErrExtended},
		{"empty table", makeMBR(), g, 0, ErrNoPartitions},
		{"two partitions", makeMBR(fat, testEntry{slot: 1, typ: 0x07, start: 300 * mib512, length: mib512}), g, 0, ErrTooMany},
		{"three partitions", makeMBR(fat, testEntry{slot: 1, typ: 0x83, start: 300 * mib512, length: mib512},
			testEntry{slot: 2, typ: 0x83, start: 400 * mib512, length: mib512}), g, 0, ErrTooMany},
		{"overlap", makeMBR(fat, testEntry{slot: 1, typ: 0x83, start: 100 * mib512, length: 200 * mib512}), g, 0, ErrOverlap},
		{"beyond disk", makeMBR(savior(9 * gib512)), g, 0, ErrOverlap},
		{"garbage status", makeMBR(testEntry{slot: 0, status: 0x12, typ: 0x0C, start: 2048, length: 100}), g, 0, ErrInvalid},
		{"zero start", makeMBR(testEntry{slot: 0, typ: 0x0C, start: 0, length: 100}), g, 0, ErrInvalid},
		{"no space", makeMBR(savior(200 * mib512)), Geometry{Sectors: 400 * mib512, SectorSize: 512}, 0, ErrNoSpace},
		{"full disk", makeMBR(savior(8*gib512 - 2048)), g, 0, ErrNoSpace},
		{"boot mismatch", makeMBR(fat), g, 4096, ErrBootMismatch},
		{"bad geometry", makeMBR(fat), Geometry{Sectors: 100, SectorSize: 1000}, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanDataPartition(tc.mbr, tc.g, tc.boot, 256<<20)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPlanSlots(t *testing.T) {
	g := Geometry{Sectors: 8 * gib512, SectorSize: 512}
	// Boot partition in slot 2: the new entry goes into slot 3.
	e := savior(200 * mib512)
	e.slot = 2
	p, err := PlanDataPartition(makeMBR(e), g, 0, 256<<20)
	if err != nil || p.Slot != 3 || p.PartNum != 4 {
		t.Fatalf("plan %+v %v", p, err)
	}
	// Boot partition in slot 3: the first free slot (0) is used.
	e.slot = 3
	p, err = PlanDataPartition(makeMBR(e), g, 0, 256<<20)
	if err != nil || p.Slot != 0 {
		t.Fatalf("plan %+v %v", p, err)
	}
	// Slot 1 has type 0 but leftover bytes: not overwritten, slot 2 is used.
	mbr := makeMBR(savior(200 * mib512))
	mbr[462+8] = 0x42
	p, err = PlanDataPartition(mbr, g, 0, 256<<20)
	if err != nil || p.Slot != 2 {
		t.Fatalf("plan %+v %v", p, err)
	}
}

func TestPlanRecoverOnlyOwnPartition(t *testing.T) {
	g := Geometry{Sectors: 8 * gib512, SectorSize: 512}
	fat := savior(200 * mib512)
	own := testEntry{slot: 1, typ: 0x83, start: 201 * mib512, length: uint32(g.Sectors - 201*mib512)}
	p, err := PlanDataPartition(makeMBR(fat, own), g, 2048, 256<<20)
	if err != nil || !p.Existing || p.PartNum != 2 {
		t.Fatalf("plan %+v %v", p, err)
	}
	// Same place but a different type, start or end: not ours.
	for _, mod := range []func(e *testEntry){
		func(e *testEntry) { e.typ = 0x07 },
		func(e *testEntry) { e.start += mib512; e.length -= mib512 },
		func(e *testEntry) { e.length -= mib512 },
	} {
		o := own
		mod(&o)
		if _, err := PlanDataPartition(makeMBR(fat, o), g, 2048, 256<<20); !errors.Is(err, ErrTooMany) {
			t.Errorf("modified %+v: err %v, want ErrTooMany", o, err)
		}
	}
}

func TestHasGPTHeader(t *testing.T) {
	if !HasGPTHeader([]byte("EFI PART\x00\x00\x01\x00")) || HasGPTHeader(make([]byte, 512)) {
		t.Fatal("HasGPTHeader")
	}
}
