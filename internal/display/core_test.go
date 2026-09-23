package display

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// testFrame returns a w×h image with varied colors (gradients, noise and
// primaries) so every channel bit is exercised.
func testFrame(w, h int, seed int64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(seed))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			switch (x + y) % 3 {
			case 0:
				img.Pix[i], img.Pix[i+1], img.Pix[i+2] = uint8(x*255/max(1, w-1)), uint8(y*255/max(1, h-1)), uint8(rnd.Intn(256))
			case 1:
				img.Pix[i], img.Pix[i+1], img.Pix[i+2] = uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256))
			default:
				p := []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {255, 255, 255, 255}, {0, 0, 0, 255}}[(x/3)%5]
				img.Pix[i], img.Pix[i+1], img.Pix[i+2] = p.R, p.G, p.B
			}
			img.Pix[i+3] = 255
		}
	}
	return img
}

// tolerance is the largest per-channel error after quantizing to bits.
func tolerance(bits uint32) int {
	if bits >= 8 {
		return 0
	}
	return 256>>bits + 1
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

// compareImages fails if any channel differs by more than the format's
// quantization allows.
func compareImages(t *testing.T, got, want *image.RGBA, pf *pixelFormat) {
	t.Helper()
	if got.Rect.Size() != want.Rect.Size() {
		t.Fatalf("size %v, want %v", got.Rect.Size(), want.Rect.Size())
	}
	tol := [3]int{tolerance(pf.r.len), tolerance(pf.g.len), tolerance(pf.b.len)}
	bad := 0
	for y := 0; y < want.Rect.Dy(); y++ {
		for x := 0; x < want.Rect.Dx(); x++ {
			g, w := got.Pix[got.PixOffset(x, y):], want.Pix[want.PixOffset(x, y):]
			for c := 0; c < 3; c++ {
				if absDiff(g[c], w[c]) > tol[c] {
					if bad < 5 {
						t.Errorf("pixel %d,%d channel %d: got %d want %d (tolerance %d)", x, y, c, g[c], w[c], tol[c])
					}
					bad++
				}
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d channel mismatches", bad)
	}
}

func TestConvertersAllFormats(t *testing.T) {
	formats := []string{"XRGB8888", "ARGB8888", "XBGR8888", "ABGR8888", "RGBX8888", "XRGB2101010",
		"RGB888", "BGR888", "RGB565", "BGR565", "XRGB1555", "ARGB1555", "XRGB4444"}
	for _, name := range formats {
		for _, rot := range []int{0, 90, 180, 270} {
			t.Run(name+"/"+strconv.Itoa(rot), func(t *testing.T) {
				pf, err := formatByName(name)
				if err != nil {
					t.Fatal(err)
				}
				// Physical 37x23, padded lines, panned by 5,7.
				const pw, ph, xoff, yoff = 37, 23, 5, 7
				ll := (pw+xoff)*pf.bytesPP() + 13
				d := newMemDevice(pw, ph, pf.bpp, name, ll, xoff, yoff, rot)
				if d.Format() != name || d.LineLength() != ll {
					t.Fatalf("format %s line %d, want %s %d", d.Format(), d.LineLength(), name, ll)
				}
				info := d.Info()
				frame := testFrame(info.Width, info.Height, int64(rot))
				if err := d.Show(frame, nil); err != nil {
					t.Fatal(err)
				}
				compareImages(t, d.DecodeLogical(), frame, pf)
				// Bytes outside the visible window (padding, rows above
				// yoff, columns left of xoff) must stay untouched.
				mem := d.Bytes()
				bpp := pf.bytesPP()
				for y := 0; y < yoff+ph; y++ {
					for x := 0; x < ll; x++ {
						inside := y >= yoff && x >= xoff*bpp && x < (xoff+pw)*bpp
						if !inside && mem[y*ll+x] != 0 {
							t.Fatalf("byte %d of row %d outside the screen was written", x, y)
						}
					}
				}
			})
		}
	}
}

// TestFastPathsMatchGeneric checks each fast converter against the
// lookup-table path bit for bit.
func TestFastPathsMatchGeneric(t *testing.T) {
	src := testFrame(64, 1, 3)
	for _, name := range []string{"XRGB8888", "ARGB8888", "XBGR8888", "RGB888", "BGR888", "RGB565", "XRGB1555", "ARGB1555"} {
		pf, _ := formatByName(name)
		n := 64
		fast := make([]byte, n*pf.bytesPP())
		gen := make([]byte, n*pf.bytesPP())
		pf.conv(pf, fast, src.Pix, 0, 4, n)
		convGeneric(pf, gen, src.Pix, 0, 4, n)
		if !bytes.Equal(fast, gen) {
			t.Errorf("%s: fast path differs from generic\nfast %x\ngen  %x", name, fast[:16], gen[:16])
		}
	}
}

func TestPixelFormatValidation(t *testing.T) {
	bf := func(o, l uint32) bitfield { return bitfield{off: o, len: l} }
	tests := []struct {
		name       string
		bpp        int
		r, g, b, a bitfield
		wantErr    bool
		wantName   string
	}{
		{"xrgb", 32, bf(16, 8), bf(8, 8), bf(0, 8), bitfield{}, false, "XRGB8888"},
		{"rgb565", 16, bf(11, 5), bf(5, 6), bf(0, 5), bitfield{}, false, "RGB565"},
		{"8bpp", 8, bf(5, 3), bf(2, 3), bf(0, 2), bitfield{}, true, ""},
		{"overflow", 16, bf(12, 5), bf(5, 6), bf(0, 5), bitfield{}, true, ""},
		{"zero green", 32, bf(16, 8), bf(8, 0), bf(0, 8), bitfield{}, true, ""},
		{"odd generic", 32, bf(0, 11), bf(11, 11), bf(22, 10), bitfield{}, false, "R0:11 G11:11 B22:10 @32bpp"},
	}
	for _, tt := range tests {
		pf, err := newPixelFormat(tt.bpp, tt.r, tt.g, tt.b, tt.a)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
			continue
		}
		if err == nil && pf.name != tt.wantName {
			t.Errorf("%s: name %q, want %q", tt.name, pf.name, tt.wantName)
		}
	}
}

func TestMSBRightGeneric(t *testing.T) {
	r := bitfield{off: 11, len: 5, msbRight: true}
	pf, err := newPixelFormat(16, r, bitfield{off: 5, len: 6}, bitfield{off: 0, len: 5}, bitfield{})
	if err != nil {
		t.Fatal(err)
	}
	// Red 0b10000 (0x80 input) is stored reversed as 0b00001.
	if v := pf.pack(0x80, 0, 0); v != 1<<11 {
		t.Fatalf("pack = %#x, want %#x", v, 1<<11)
	}
	if r, _, _ := pf.unpack(1 << 11); r < 0x7c || r > 0x88 {
		t.Fatalf("unpack red = %#x", r)
	}
}

func TestRotationCorners(t *testing.T) {
	const W, H = 8, 6 // physical
	red, green, blue, white := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}, color.RGBA{255, 255, 255, 255}
	// Logical corners: top-left red, top-right green, bottom-left blue,
	// bottom-right white. Rotation turns the image clockwise.
	tests := []struct {
		rot            string
		deg            int
		tl, tr, bl, br color.RGBA // physical corners
	}{
		{"0", 0, red, green, blue, white},
		{"90", 90, blue, red, white, green},
		{"180", 180, white, blue, green, red},
		{"270", 270, green, white, red, blue},
	}
	for _, tt := range tests {
		t.Run(tt.rot, func(t *testing.T) {
			d := NewMemDevice(W, H, 32, "XRGB8888", 0, tt.deg)
			info := d.Info()
			if (tt.deg == 90 || tt.deg == 270) != (info.Width == H && info.Height == W) {
				t.Fatalf("logical size %dx%d for rotation %d", info.Width, info.Height, tt.deg)
			}
			lw, lh := info.Width, info.Height
			img := image.NewRGBA(image.Rect(0, 0, lw, lh))
			fill(img, img.Rect, color.RGBA{0, 0, 0, 255})
			img.SetRGBA(0, 0, red)
			img.SetRGBA(lw-1, 0, green)
			img.SetRGBA(0, lh-1, blue)
			img.SetRGBA(lw-1, lh-1, white)
			if err := d.Show(img, nil); err != nil {
				t.Fatal(err)
			}
			p := d.Decode()
			for _, c := range []struct {
				x, y int
				want color.RGBA
			}{{0, 0, tt.tl}, {W - 1, 0, tt.tr}, {0, H - 1, tt.bl}, {W - 1, H - 1, tt.br}} {
				if got := p.RGBAAt(c.x, c.y); got != c.want {
					t.Errorf("physical %d,%d = %v, want %v", c.x, c.y, got, c.want)
				}
			}
			if got := d.DecodeLogical(); !bytes.Equal(got.Pix, img.Pix) {
				t.Error("DecodeLogical does not round-trip")
			}
		})
	}
}

func TestOnlyChangedRowsWritten(t *testing.T) {
	for _, rot := range []int{0, 90} {
		d := NewMemDevice(64, 48, 16, "RGB565", 0, rot)
		info := d.Info()
		frame := testFrame(info.Width, info.Height, 1)
		if err := d.Show(frame, nil); err != nil {
			t.Fatal(err)
		}
		if got := d.RowsWritten(); got != 48 {
			t.Fatalf("rot %d: first show wrote %d rows, want 48", rot, got)
		}
		if err := d.Show(frame, nil); err != nil {
			t.Fatal(err)
		}
		if got := d.RowsWritten(); got != 48 {
			t.Fatalf("rot %d: identical frame wrote %d more rows", rot, got-48)
		}
		frame.SetRGBA(5, 7, color.RGBA{1, 2, 3, 255})
		frame.SetRGBA(6, 7, color.RGBA{250, 2, 3, 255})
		if err := d.Show(frame, nil); err != nil {
			t.Fatal(err)
		}
		want := 49
		if rot == 90 {
			want = 50 // the two logical pixels sit in two physical rows
		}
		if got := d.RowsWritten(); got != want {
			t.Fatalf("rot %d: changing one logical row wrote %d rows total, want %d", rot, got, want)
		}
		// A dirty rectangle limits conversion: changes outside it are not
		// shown until a full show.
		frame.SetRGBA(0, 0, color.RGBA{255, 255, 255, 255})
		if err := d.Show(frame, []image.Rectangle{image.Rect(10, 10, 12, 12)}); err != nil {
			t.Fatal(err)
		}
		if got := d.DecodeLogical().RGBAAt(0, 0); got == (color.RGBA{255, 255, 255, 255}) {
			t.Fatalf("rot %d: pixel outside the dirty rect was converted", rot)
		}
		d.Invalidate()
		before := d.RowsWritten()
		if err := d.Show(frame, nil); err != nil {
			t.Fatal(err)
		}
		if got := d.RowsWritten() - before; got != 48 {
			t.Fatalf("rot %d: show after Invalidate wrote %d rows, want 48", rot, got)
		}
		compareImages(t, d.DecodeLogical(), frame, d.core.pf)
	}
}

func TestShowRejectsWrongSize(t *testing.T) {
	d := NewMemDevice(20, 10, 32, "", 0, 90)
	if err := d.Show(image.NewRGBA(image.Rect(0, 0, 20, 10)), nil); err == nil {
		t.Fatal("expected size error for an unrotated frame on a rotated device")
	}
	if err := d.Show(nil, nil); err == nil {
		t.Fatal("expected error for nil frame")
	}
	// A frame with a non-zero origin (sub-image) is fine.
	big := testFrame(30, 40, 9)
	sub := big.SubImage(image.Rect(5, 5, 15, 25)).(*image.RGBA)
	if err := d.Show(sub, nil); err != nil {
		t.Fatal(err)
	}
	got := d.DecodeLogical()
	if a, b := got.RGBAAt(0, 0), sub.RGBAAt(5, 5); a != b {
		t.Fatalf("sub-image origin: got %v want %v", a, b)
	}
}

func TestMemDeviceBadArgsFallBack(t *testing.T) {
	d := NewMemDevice(-1, 0, 12, "NOPE", 0, 45)
	info := d.Info()
	if info.FBWidth != 640 || info.FBHeight != 480 || info.Format != "XRGB8888" || info.Rotate != 0 {
		t.Fatalf("fallback info %+v", info)
	}
	if info.Warning == "" {
		t.Fatal("expected a warning")
	}
	if d := NewMemDevice(10, 10, 16, "XRGB8888", 0, 0); d.Format() != "RGB565" || d.Info().Warning == "" {
		t.Fatalf("bpp/format mismatch: format %s warning %q", d.Format(), d.Info().Warning)
	}
}

func TestMemDeviceBlankAndFail(t *testing.T) {
	d := NewMemDevice(10, 10, 32, "", 0, 0)
	frame := image.NewRGBA(image.Rect(0, 0, 10, 10))
	fill(frame, frame.Rect, color.RGBA{200, 100, 50, 255})
	if err := d.Show(frame, nil); err != nil {
		t.Fatal(err)
	}
	m, err := d.Blank(true)
	if err != nil || m != "black" || !d.Blanked() {
		t.Fatalf("blank: %q %v", m, err)
	}
	if c := d.Decode().RGBAAt(5, 5); c != (color.RGBA{0, 0, 0, 255}) {
		t.Fatalf("black blank left %v", c)
	}
	if _, err := d.Blank(false); err != nil {
		t.Fatal(err)
	}
	if err := d.Show(frame, nil); err != nil {
		t.Fatal(err)
	}
	if c := d.Decode().RGBAAt(5, 5); c != (color.RGBA{200, 100, 50, 255}) {
		t.Fatalf("after unblank %v", c)
	}
	d.FailNextShow(ErrDeviceGone)
	if err := d.Show(frame, nil); !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("injected error = %v", err)
	}
	if err := d.Show(frame, nil); err != nil {
		t.Fatalf("second show: %v", err)
	}
	d.Close()
	if err := d.Show(frame, nil); !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("show after close = %v", err)
	}
}

// fileWriterAt is an io.WriterAt that records writes, for the pwrite path.
type countingWriterAt struct {
	f      *os.File
	writes int
}

func (c *countingWriterAt) WriteAt(p []byte, off int64) (int, error) {
	c.writes++
	return c.f.WriteAt(p, off)
}

func TestPwriteTarget(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "fb"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pf, _ := formatByName("BGR888")
	g := fbGeom{w: 16, h: 9, xoff: 2, yoff: 3, lineLen: 70}
	wa := &countingWriterAt{f: f}
	core, err := newFBCore(g, pf, 180, fbTarget{wa: wa})
	if err != nil {
		t.Fatal(err)
	}
	frame := testFrame(16, 9, 4)
	if err := core.show(frame, nil); err != nil {
		t.Fatal(err)
	}
	if wa.writes != 1 {
		t.Fatalf("contiguous changed rows took %d pwrites, want 1", wa.writes)
	}
	mem, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	compareImages(t, unrotate(core.decode(mem), 180), frame, pf)
}

func TestPNGDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.png")
	d := NewPNGDevice(path, 40, 30, 90)
	info := d.Info()
	if info.Width != 30 || info.Height != 40 || info.FBWidth != 40 {
		t.Fatalf("info %+v", info)
	}
	img := image.NewRGBA(image.Rect(0, 0, 30, 40))
	img.SetRGBA(0, 0, color.RGBA{255, 0, 0, 255})
	if err := d.Show(img, nil); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if b := p.Bounds(); b.Dx() != 40 || b.Dy() != 30 {
		t.Fatalf("png is %v", b)
	}
	// Rotated 90° clockwise: the logical top-left is the physical top-right.
	if r, g, b, _ := p.At(39, 0).RGBA(); r>>8 != 255 || g != 0 || b != 0 {
		t.Fatalf("physical top-right = %v", p.At(39, 0))
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
	if m, err := d.Blank(true); err != nil || m != "black" {
		t.Fatalf("blank %q %v", m, err)
	}
	if err := d.(Rotator).SetRotate(45); err == nil {
		t.Fatal("expected error for 45 degrees")
	}
}

func TestNormRotate(t *testing.T) {
	for in, want := range map[int]int{0: 0, 90: 90, 180: 180, 270: 270, 360: 0, -90: 270, 450: 90} {
		if got, err := normRotate(in); err != nil || got != want {
			t.Errorf("normRotate(%d) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []int{1, 45, 91, -1} {
		if _, err := normRotate(bad); err == nil {
			t.Errorf("normRotate(%d) should fail", bad)
		}
	}
}
