//go:build linux

package display

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// Kernel ABI from <linux/fb.h>. These request numbers are fixed values
// (not _IOR-encoded), so they are the same on 386 and amd64.
const (
	fbioGetVScreenInfo = 0x4600
	fbioGetFScreenInfo = 0x4602
	fbioPutCmap        = 0x4605
	fbioPanDisplay     = 0x4606
	fbioBlank          = 0x4611

	fbTypePackedPixels  = 0
	fbVisualTrueColor   = 2
	fbVisualDirectColor = 4

	fbBlankUnblank   = 0
	fbBlankPowerdown = 4
)

// fbBitfieldABI is struct fb_bitfield.
type fbBitfieldABI struct {
	Offset   uint32
	Length   uint32
	MSBRight uint32
}

// fbVarScreeninfo is struct fb_var_screeninfo (160 bytes on every arch).
type fbVarScreeninfo struct {
	XRes, YRes               uint32
	XResVirtual, YResVirtual uint32
	XOffset, YOffset         uint32
	BitsPerPixel             uint32
	Grayscale                uint32
	Red, Green, Blue, Transp fbBitfieldABI
	Nonstd, Activate         uint32
	Height, Width            uint32 // mm
	AccelFlags               uint32
	Pixclock                 uint32
	LeftMargin, RightMargin  uint32
	UpperMargin, LowerMargin uint32
	HsyncLen, VsyncLen       uint32
	Sync, Vmode, Rotate      uint32
	Colorspace               uint32
	Reserved                 [4]uint32
}

// fbFixScreeninfo is struct fb_fix_screeninfo. smem_start and mmio_start
// are C unsigned longs, so the struct is 68 bytes on 386 and 80 on amd64;
// uintptr has exactly that size and alignment on both.
type fbFixScreeninfo struct {
	ID           [16]byte
	SmemStart    uintptr
	SmemLen      uint32
	Type         uint32
	TypeAux      uint32
	Visual       uint32
	XPanStep     uint16
	YPanStep     uint16
	YWrapStep    uint16
	LineLength   uint32
	MmioStart    uintptr
	MmioLen      uint32
	Accel        uint32
	Capabilities uint16
	Reserved     [2]uint16
}

// fbCmap is struct fb_cmap.
type fbCmap struct {
	Start  uint32
	Len    uint32
	Red    *uint16
	Green  *uint16
	Blue   *uint16
	Transp *uint16
}

// ioctlPtr issues an ioctl whose argument points to p.
func ioctlPtr(fd int, req uintptr, p unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(p))
	if e != 0 {
		return e
	}
	return nil
}

// ioctlVal issues an ioctl with an integer argument.
func ioctlVal(fd int, req uintptr, v uintptr) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, v)
	if e != 0 {
		return e
	}
	return nil
}

func (b fbBitfieldABI) field() bitfield {
	return bitfield{off: b.Offset, len: b.Length, msbRight: b.MSBRight != 0}
}
