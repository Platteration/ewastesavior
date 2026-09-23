//go:build linux

package display

import (
	"errors"
	"fmt"
	"image"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/platteration/ewastesavior/internal/proto"
)

// fbDevice drives a Linux fbdev framebuffer (/dev/fbN).
type fbDevice struct {
	path     string
	f        *os.File
	fd       int
	core     *fbCore
	mapping  []byte // whole mmap, for munmap
	vi       fbVarScreeninfo
	sysRoot  string
	sysName  string // /sys/class/graphics/fbN/name at open
	fixID    string // fb_fix_screeninfo.id
	rdev     uint64
	auto     bool // opened via display_device=auto
	autoMiss int  // consecutive polls where auto selection differed
	warnings []string
	blankVia string
	bl       *backlight
}

// fbOpts tunes setupFB (tests use a regular file instead of /dev/fbN).
type fbOpts struct {
	sysRoot string
	noMmap  bool
}

// OpenFramebuffer opens a Linux framebuffer device (DESIGN 11.1): it
// validates the pixel layout, loads identity ramps for DIRECTCOLOR, pans to
// 0,0 when possible, and maps the framebuffer memory (or falls back to
// pwrite).
func OpenFramebuffer(path string, rotate int) (Device, error) {
	return openFramebuffer(path, rotate, fbOpts{sysRoot: "/"})
}

func openAuto(sysRoot string, rotate int) (Device, error) {
	path, err := AutoDevicePath(sysRoot)
	if err != nil {
		return nil, err
	}
	d, err := openFramebuffer(path, rotate, fbOpts{sysRoot: sysRoot})
	if err != nil {
		return nil, err
	}
	d.auto = true
	return d, nil
}

func openFramebuffer(path string, rotate int, opt fbOpts) (*fbDevice, error) {
	_ = restoreSavedBacklight() // a previous agent may have died while blanked
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	fd := int(f.Fd())
	var vi fbVarScreeninfo
	var fi fbFixScreeninfo
	if err := ioctlPtr(fd, fbioGetVScreenInfo, unsafe.Pointer(&vi)); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: FBIOGET_VSCREENINFO: %w", path, err)
	}
	if err := ioctlPtr(fd, fbioGetFScreenInfo, unsafe.Pointer(&fi)); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: FBIOGET_FSCREENINFO: %w", path, err)
	}
	if err := validateFB(&vi, &fi); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s (%s): %w", path, fixID(&fi), err)
	}
	var warns []string
	if fi.Visual == fbVisualDirectColor {
		if err := putIdentityCmap(fd, &vi); err != nil {
			warns = append(warns, "DIRECTCOLOR ramps: "+err.Error())
		}
	}
	if vi.XOffset != 0 || vi.YOffset != 0 {
		pan := vi
		pan.XOffset, pan.YOffset = 0, 0
		if err := ioctlPtr(fd, fbioPanDisplay, unsafe.Pointer(&pan)); err == nil {
			vi.XOffset, vi.YOffset = 0, 0
		} else {
			warns = append(warns, fmt.Sprintf("cannot pan to 0,0 (%v); drawing at offset %d,%d", err, vi.XOffset, vi.YOffset))
		}
	}
	d, err := setupFB(f, path, &vi, &fi, rotate, opt)
	if err != nil {
		f.Close()
		return nil, err
	}
	d.warnings = append(warns, d.warnings...)
	return d, nil
}

func fixID(fi *fbFixScreeninfo) string {
	return strings.TrimRight(string(fi.ID[:]), "\x00")
}

// validateFB applies the DESIGN 11.1 requirements to the screen info.
func validateFB(vi *fbVarScreeninfo, fi *fbFixScreeninfo) error {
	if fi.Type != fbTypePackedPixels {
		return fmt.Errorf("unsupported framebuffer type %d (need packed pixels)", fi.Type)
	}
	if fi.Visual != fbVisualTrueColor && fi.Visual != fbVisualDirectColor {
		return fmt.Errorf("unsupported visual %d (need truecolor or directcolor; palette modes are not supported)", fi.Visual)
	}
	if vi.Grayscale != 0 {
		return errors.New("grayscale framebuffers are not supported")
	}
	switch vi.BitsPerPixel {
	case 16, 24, 32:
	default:
		return fmt.Errorf("unsupported depth %d bpp (need 16, 24 or 32; try the safe graphics boot entry)", vi.BitsPerPixel)
	}
	if vi.XRes == 0 || vi.YRes == 0 {
		return errors.New("framebuffer reports no resolution")
	}
	return nil
}

// setupFB builds the device from screen info: geometry, format, target.
func setupFB(f *os.File, path string, vi *fbVarScreeninfo, fi *fbFixScreeninfo, rotate int, opt fbOpts) (*fbDevice, error) {
	pf, err := newPixelFormat(int(vi.BitsPerPixel), vi.Red.field(), vi.Green.field(), vi.Blue.field(), vi.Transp.field())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	bpp := pf.bytesPP()
	lineLen := int(fi.LineLength)
	if lineLen == 0 {
		lineLen = int(max(vi.XResVirtual, vi.XRes)) * bpp
	}
	g := fbGeom{w: int(vi.XRes), h: int(vi.YRes), xoff: int(vi.XOffset), yoff: int(vi.YOffset), lineLen: lineLen}
	need := (g.yoff + g.h) * lineLen
	if fi.SmemLen != 0 && uint64(need) > uint64(fi.SmemLen) {
		return nil, fmt.Errorf("%s: %dx%d at offset %d,%d needs %d bytes, framebuffer has %d", path, g.w, g.h, g.xoff, g.yoff, need, fi.SmemLen)
	}
	d := &fbDevice{path: path, f: f, fd: int(f.Fd()), vi: *vi, sysRoot: opt.sysRoot, fixID: fixID(fi)}
	var target fbTarget
	if !opt.noMmap && fi.SmemLen > 0 {
		page := os.Getpagesize()
		base := int(fi.SmemStart & uintptr(page-1))
		length := (base + int(fi.SmemLen) + page - 1) &^ (page - 1)
		m, err := unix.Mmap(d.fd, 0, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err == nil {
			d.mapping = m
			g.base = base
			target.mem = m
		} else {
			d.warnings = append(d.warnings, fmt.Sprintf("mmap failed (%v); using pwrite", err))
		}
	}
	if target.mem == nil {
		// fb_write offsets are relative to the start of the framebuffer
		// memory itself, so the page offset of smem_start does not apply.
		target.wa = f
	}
	d.core, err = newFBCore(g, pf, rotate, target)
	if err != nil {
		d.unmap()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(d.fd, &st); err == nil {
		d.rdev = uint64(st.Rdev)
	}
	d.sysName = fbName(opt.sysRoot, path)
	return d, nil
}

// putIdentityCmap loads linear ramps so DIRECTCOLOR looks like TRUECOLOR.
func putIdentityCmap(fd int, vi *fbVarScreeninfo) error {
	bits := max(vi.Red.Length, vi.Green.Length, vi.Blue.Length)
	if bits == 0 || bits > 16 {
		return fmt.Errorf("bad channel length %d", bits)
	}
	n := 1 << bits
	ramp := func(l uint32) []uint16 {
		out := make([]uint16, n)
		size := 1 << l
		for i := range out {
			v := min(i, size-1)
			if size > 1 {
				out[i] = uint16(v * 65535 / (size - 1))
			}
		}
		return out
	}
	r, g, b := ramp(vi.Red.Length), ramp(vi.Green.Length), ramp(vi.Blue.Length)
	cm := fbCmap{Len: uint32(n), Red: &r[0], Green: &g[0], Blue: &b[0]}
	err := ioctlPtr(fd, fbioPutCmap, unsafe.Pointer(&cm))
	runtime.KeepAlive(r)
	runtime.KeepAlive(g)
	runtime.KeepAlive(b)
	return err
}

// gone wraps errors that mean the device went away.
func gone(err error) error {
	if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENXIO) {
		return fmt.Errorf("%w: %v", ErrDeviceGone, err)
	}
	return err
}

func (d *fbDevice) Size() (int, int) { return d.core.g.w, d.core.g.h }

func (d *fbDevice) Show(img *image.RGBA, dirty []image.Rectangle) error {
	return gone(d.core.show(img, dirty))
}

// Blank implements the fallback chain FBIOBLANK → backlight → black frame.
func (d *fbDevice) Blank(on bool) (string, error) {
	if !on {
		via := d.blankVia
		d.blankVia = ""
		switch via {
		case "fbioblank":
			return via, gone(ioctlVal(d.fd, fbioBlank, fbBlankUnblank))
		case "backlight":
			if d.bl != nil {
				return via, d.bl.on()
			}
		}
		return via, nil
	}
	if d.blankVia != "" {
		return d.blankVia, nil
	}
	err := ioctlVal(d.fd, fbioBlank, fbBlankPowerdown)
	if err == nil {
		d.blankVia = "fbioblank"
		return d.blankVia, nil
	}
	if errors.Is(err, syscall.ENODEV) {
		return "", gone(err)
	}
	if d.bl == nil {
		d.bl = findBacklight(d.sysRoot)
	}
	if d.bl != nil && d.bl.off() == nil {
		d.blankVia = "backlight"
		return d.blankVia, nil
	}
	d.core.fillShadow(d.core.pf.pack(0, 0, 0))
	if err := d.core.flush(); err != nil {
		return "black", gone(err)
	}
	d.blankVia = "black"
	return d.blankVia, nil
}

func (d *fbDevice) Info() proto.DisplayState {
	st := proto.DisplayState{Active: true, Device: d.path, Driver: d.sysName}
	if st.Driver == "" {
		st.Driver = d.fixID
	}
	d.core.fillInfo(&st)
	if len(d.warnings) > 0 {
		st.Warning = strings.Join(d.warnings, "; ")
	}
	return st
}

func (d *fbDevice) unmap() {
	if d.mapping != nil {
		_ = unix.Munmap(d.mapping)
		d.mapping = nil
	}
}

// Close unblanks (so the text console is visible after the agent stops)
// and releases the mapping.
func (d *fbDevice) Close() error {
	if d.blankVia != "" {
		_, _ = d.Blank(false)
	}
	d.unmap()
	return d.f.Close()
}

func (d *fbDevice) SetRotate(deg int) error {
	if err := d.core.setRotate(deg); err != nil {
		return err
	}
	d.core.invalidate()
	return nil
}

func (d *fbDevice) Invalidate() { d.core.invalidate() }

func (d *fbDevice) usesVT() bool { return true }

// Check detects a replaced or removed framebuffer: a different device
// node, a different driver name, ENODEV, a mode change, or (for auto
// selection) a better framebuffer that stayed preferred for 3 polls.
func (d *fbDevice) Check() error {
	var st unix.Stat_t
	if err := unix.Stat(d.path, &st); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrDeviceGone, d.path, err)
	}
	if d.rdev != 0 && uint64(st.Rdev) != d.rdev {
		return fmt.Errorf("%w: %s now refers to another device", ErrDeviceGone, d.path)
	}
	if n := fbName(d.sysRoot, d.path); n != d.sysName {
		return fmt.Errorf("%w: %s driver changed from %q to %q", ErrDeviceGone, d.path, d.sysName, n)
	}
	var vi fbVarScreeninfo
	if err := ioctlPtr(d.fd, fbioGetVScreenInfo, unsafe.Pointer(&vi)); err != nil {
		if errors.Is(err, syscall.ENODEV) {
			return fmt.Errorf("%w: %s: %v", ErrDeviceGone, d.path, err)
		}
	} else if vi.XRes != d.vi.XRes || vi.YRes != d.vi.YRes || vi.BitsPerPixel != d.vi.BitsPerPixel ||
		vi.Red != d.vi.Red || vi.Green != d.vi.Green || vi.Blue != d.vi.Blue {
		return fmt.Errorf("%w: %s mode changed to %dx%d-%d", ErrDeviceGone, d.path, vi.XRes, vi.YRes, vi.BitsPerPixel)
	}
	if d.auto {
		if p, err := AutoDevicePath(d.sysRoot); err == nil && p != d.path {
			if d.autoMiss++; d.autoMiss >= 3 {
				return fmt.Errorf("%w: display moved to %s", ErrDeviceGone, p)
			}
		} else {
			d.autoMiss = 0
		}
	}
	return nil
}
