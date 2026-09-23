//go:build linux

package display

import (
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func TestKernelStructLayouts(t *testing.T) {
	if got := unsafe.Sizeof(fbVarScreeninfo{}); got != 160 {
		t.Errorf("fb_var_screeninfo is %d bytes, want 160", got)
	}
	var fi fbFixScreeninfo
	wantFix, wantLine, wantMmio := uintptr(80), uintptr(48), uintptr(56)
	wantCmap := uintptr(40)
	if runtime.GOARCH == "386" {
		wantFix, wantLine, wantMmio, wantCmap = 68, 44, 48, 24
	}
	if got := unsafe.Sizeof(fi); got != wantFix {
		t.Errorf("fb_fix_screeninfo is %d bytes on %s, want %d", got, runtime.GOARCH, wantFix)
	}
	if got := unsafe.Offsetof(fi.LineLength); got != wantLine {
		t.Errorf("line_length at %d, want %d", got, wantLine)
	}
	if got := unsafe.Offsetof(fi.MmioStart); got != wantMmio {
		t.Errorf("mmio_start at %d, want %d", got, wantMmio)
	}
	if got := unsafe.Offsetof(fi.SmemStart); got != 16 {
		t.Errorf("smem_start at %d, want 16", got)
	}
	var vi fbVarScreeninfo
	if got := unsafe.Offsetof(vi.Red); got != 32 {
		t.Errorf("red bitfield at %d, want 32", got)
	}
	if got := unsafe.Offsetof(vi.Rotate); got != 136 {
		t.Errorf("rotate at %d, want 136", got)
	}
	if got := unsafe.Sizeof(fbCmap{}); got != wantCmap {
		t.Errorf("fb_cmap is %d bytes, want %d", got, wantCmap)
	}
	if got := unsafe.Sizeof(vtMode{}); got != 8 {
		t.Errorf("vt_mode is %d bytes, want 8", got)
	}
	if got := unsafe.Sizeof(vtStat{}); got != 6 {
		t.Errorf("vt_stat is %d bytes, want 6", got)
	}
}

func goodScreenInfo(bpp uint32) (fbVarScreeninfo, fbFixScreeninfo) {
	vi := fbVarScreeninfo{XRes: 32, YRes: 20, XResVirtual: 40, YResVirtual: 60, BitsPerPixel: bpp}
	switch bpp {
	case 16:
		vi.Red, vi.Green, vi.Blue = fbBitfieldABI{Offset: 11, Length: 5}, fbBitfieldABI{Offset: 5, Length: 6}, fbBitfieldABI{Length: 5}
	default:
		vi.Red, vi.Green, vi.Blue = fbBitfieldABI{Offset: 16, Length: 8}, fbBitfieldABI{Offset: 8, Length: 8}, fbBitfieldABI{Length: 8}
	}
	fi := fbFixScreeninfo{Type: fbTypePackedPixels, Visual: fbVisualTrueColor, LineLength: 40 * bpp / 8, SmemLen: 40 * 60 * bpp / 8}
	copy(fi.ID[:], "test fb")
	return vi, fi
}

func TestValidateFB(t *testing.T) {
	tests := []struct {
		name string
		mod  func(vi *fbVarScreeninfo, fi *fbFixScreeninfo)
		want string // "" = ok
	}{
		{"truecolor", func(*fbVarScreeninfo, *fbFixScreeninfo) {}, ""},
		{"directcolor", func(_ *fbVarScreeninfo, fi *fbFixScreeninfo) { fi.Visual = fbVisualDirectColor }, ""},
		{"planes", func(_ *fbVarScreeninfo, fi *fbFixScreeninfo) { fi.Type = 1 }, "type"},
		{"pseudocolor", func(_ *fbVarScreeninfo, fi *fbFixScreeninfo) { fi.Visual = 3 }, "visual"},
		{"grayscale", func(vi *fbVarScreeninfo, _ *fbFixScreeninfo) { vi.Grayscale = 1 }, "grayscale"},
		{"8bpp", func(vi *fbVarScreeninfo, _ *fbFixScreeninfo) { vi.BitsPerPixel = 8 }, "depth"},
		{"15bpp", func(vi *fbVarScreeninfo, _ *fbFixScreeninfo) { vi.BitsPerPixel = 15 }, "depth"},
		{"no resolution", func(vi *fbVarScreeninfo, _ *fbFixScreeninfo) { vi.XRes = 0 }, "resolution"},
	}
	for _, tt := range tests {
		vi, fi := goodScreenInfo(32)
		tt.mod(&vi, &fi)
		err := validateFB(&vi, &fi)
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

// fakeFB creates a regular file standing in for framebuffer memory.
func fakeFB(t *testing.T, size int) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "fb0"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestFakeFramebuffer(t *testing.T) {
	page := os.Getpagesize()
	for _, tc := range []struct {
		name   string
		bpp    uint32
		noMmap bool
		base   int // smem_start page offset
	}{
		{"mmap-32", 32, false, 0},
		{"mmap-16-offset", 16, false, 0x120},
		{"pwrite-32", 32, true, 0},
		{"pwrite-16-offset", 16, true, 0x120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vi, fi := goodScreenInfo(tc.bpp)
			vi.XOffset, vi.YOffset = 3, 25 // panned; pan failed, honor it
			fi.SmemStart = uintptr(0x40000000 + tc.base)
			fileSize := tc.base + int(fi.SmemLen)
			fileSize = (fileSize + page - 1) &^ (page - 1)
			f := fakeFB(t, fileSize)
			d, err := setupFB(f, f.Name(), &vi, &fi, 90, fbOpts{sysRoot: t.TempDir(), noMmap: tc.noMmap})
			if err != nil {
				t.Fatal(err)
			}
			if tc.noMmap != (d.mapping == nil) {
				t.Fatalf("mapping %v, noMmap %v", d.mapping != nil, tc.noMmap)
			}
			info := d.Info()
			if info.Width != 20 || info.Height != 32 || info.FBWidth != 32 || info.Driver != "test fb" {
				t.Fatalf("info %+v", info)
			}
			frame := testFrame(info.Width, info.Height, 7)
			if err := d.Show(frame, nil); err != nil {
				t.Fatal(err)
			}
			mem, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			// mmap writes land at the smem_start page offset; pwrite
			// offsets are relative to smem_start itself.
			core := *d.core
			core.g.base = tc.base
			if tc.noMmap {
				core.g.base = 0
			}
			compareImages(t, unrotate(core.decode(mem), 90), frame, d.core.pf)
			m, err := d.Blank(true) // FBIOBLANK fails on a file; no backlight in sysRoot
			if err != nil || m != "black" {
				t.Fatalf("blank = %q, %v", m, err)
			}
			mem, _ = os.ReadFile(f.Name())
			if c := core.decode(mem).RGBAAt(5, 5); c != (color.RGBA{0, 0, 0, 255}) {
				t.Fatalf("black blank left %v", c)
			}
			if _, err := d.Blank(false); err != nil {
				t.Fatal(err)
			}
			// A regular file never "changes driver"; Check must not
			// report it gone (ENOTTY from the ioctl is not ENODEV).
			if err := d.Check(); err != nil {
				t.Fatalf("check: %v", err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFakeFramebufferReplaced(t *testing.T) {
	vi, fi := goodScreenInfo(32)
	f := fakeFB(t, int(fi.SmemLen))
	d, err := setupFB(f, f.Name(), &vi, &fi, 0, fbOpts{sysRoot: t.TempDir(), noMmap: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.rdev = 12345 // pretend it was a different device node
	if err := d.Check(); !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("check after replacement = %v", err)
	}
	d.rdev = 0
	os.Remove(f.Name())
	if err := d.Check(); !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("check after removal = %v", err)
	}
}

func TestSetupFBRejectsBadGeometry(t *testing.T) {
	vi, fi := goodScreenInfo(32)
	fi.SmemLen = 100 // far too small for 32x20 at yoffset 0
	f := fakeFB(t, 4096)
	if _, err := setupFB(f, f.Name(), &vi, &fi, 0, fbOpts{sysRoot: t.TempDir()}); err == nil {
		t.Fatal("expected error for a framebuffer smaller than the screen")
	}
	vi, fi = goodScreenInfo(32)
	fi.LineLength = 10 // shorter than a row
	if _, err := setupFB(f, f.Name(), &vi, &fi, 0, fbOpts{sysRoot: t.TempDir(), noMmap: true}); err == nil {
		t.Fatal("expected error for a line length shorter than a row")
	}
	vi, fi = goodScreenInfo(32)
	vi.Red.Offset = 30 // bitfield beyond 32 bits
	if _, err := setupFB(f, f.Name(), &vi, &fi, 0, fbOpts{sysRoot: t.TempDir(), noMmap: true}); err == nil {
		t.Fatal("expected error for a bad bitfield")
	}
}

func TestOpenFramebufferErrors(t *testing.T) {
	if _, err := OpenFramebuffer(filepath.Join(t.TempDir(), "missing"), 0); err == nil {
		t.Fatal("expected error for a missing device")
	}
	// A regular file is not a framebuffer: the ioctl fails cleanly.
	f := fakeFB(t, 4096)
	if _, err := OpenFramebuffer(f.Name(), 0); err == nil || !strings.Contains(err.Error(), "FBIOGET_VSCREENINFO") {
		t.Fatalf("regular file: %v", err)
	}
}

func TestVTNumber(t *testing.T) {
	for path, want := range map[string]int{"/dev/tty7": 7, "/dev/tty1": 1, "/dev/tty63": 63} {
		if got, err := vtNumber(path); err != nil || got != want {
			t.Errorf("vtNumber(%q) = %d, %v", path, got, err)
		}
	}
	for _, bad := range []string{"/dev/tty0", "/dev/tty64", "/dev/ttyS0", "/dev/fb0", "tty7", ""} {
		if _, err := vtNumber(bad); err == nil {
			t.Errorf("vtNumber(%q) should fail", bad)
		}
	}
	if _, err := openVT("/dev/ttyS0", nil, nil); err == nil {
		t.Error("openVT accepted a serial port")
	}
}
