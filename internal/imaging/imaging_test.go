package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecodeLimitedOK(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 40, 30))
	src.Set(1, 1, color.RGBA{255, 0, 0, 255})
	img, format, err := DecodeLimited(bytes.NewReader(encodePNG(t, src)), DefaultLimits)
	if err != nil || format != "png" || img.Bounds().Dx() != 40 {
		t.Fatalf("decode: %v %s %v", err, format, img)
	}
}

// pngBomb returns a tiny PNG whose header declares w×h pixels.
func pngBomb(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	chunk := func(typ string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(typ)
		b.Write(data)
		c := crc32.NewIEEE()
		c.Write([]byte(typ))
		c.Write(data)
		binary.Write(&b, binary.BigEndian, c.Sum32())
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	return b.Bytes()
}

func TestDecodeLimitedRejectsBomb(t *testing.T) {
	_, _, err := DecodeLimited(bytes.NewReader(pngBomb(30000, 30000)), DefaultLimits)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("bomb accepted or wrong error: %v", err)
	}
	_, _, err = DecodeLimited(bytes.NewReader(pngBomb(9000, 10)), DefaultLimits)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over-wide image accepted: %v", err)
	}
}

func TestDecodeLimitedByteCap(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for i := range src.Pix {
		src.Pix[i] = uint8(i * 7)
	}
	data := encodePNG(t, src)
	_, _, err := DecodeLimited(bytes.NewReader(data), Limits{MaxSide: 8192, MaxPixels: 1 << 24, MaxBytes: int64(len(data) / 2)})
	if err == nil {
		t.Fatal("byte cap not enforced")
	}
}

func TestPlace(t *testing.T) {
	cases := []struct {
		iw, ih, cw, ch int
		fit            string
		want           image.Rectangle
	}{
		{200, 100, 100, 100, "contain", image.Rect(0, 25, 100, 75)},
		{100, 200, 100, 100, "contain", image.Rect(25, 0, 75, 100)},
		{200, 100, 100, 100, "cover", image.Rect(-50, 0, 150, 100)},
		{100, 200, 100, 100, "cover", image.Rect(0, -50, 100, 150)},
		{123, 45, 100, 100, "stretch", image.Rect(0, 0, 100, 100)},
		{100, 100, 100, 100, "", image.Rect(0, 0, 100, 100)},
	}
	for _, c := range cases {
		if got := Place(c.iw, c.ih, c.cw, c.ch, c.fit); got != c.want {
			t.Errorf("Place(%d,%d,%d,%d,%s)=%v want %v", c.iw, c.ih, c.cw, c.ch, c.fit, got, c.want)
		}
	}
}

// A 2x1 wall of 100x100 tiles showing a 200x100 image left red / right blue.
func TestRenderRegionWallTiles(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			if x < 100 {
				src.SetRGBA(x, y, color.RGBA{255, 0, 0, 255})
			} else {
				src.SetRGBA(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	bg := color.RGBA{0, 0, 0, 255}
	left := RenderRegion(src, 200, 100, "contain", image.Rect(0, 0, 100, 100), 50, 50, bg)
	right := RenderRegion(src, 200, 100, "contain", image.Rect(100, 0, 200, 100), 50, 50, bg)
	if c := left.RGBAAt(25, 25); c.R != 255 || c.B != 0 {
		t.Fatalf("left tile center %v", c)
	}
	if c := right.RGBAAt(25, 25); c.B != 255 || c.R != 0 {
		t.Fatalf("right tile center %v", c)
	}
}

func TestRenderRegionLetterbox(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for i := range src.Pix {
		src.Pix[i] = 255
	}
	out := RenderRegion(src, 100, 100, "contain", image.Rect(0, 0, 100, 100), 100, 100, color.RGBA{0, 0, 0, 255})
	if c := out.RGBAAt(50, 5); c.R != 0 {
		t.Fatalf("letterbox top should be bg, got %v", c)
	}
	if c := out.RGBAAt(50, 50); c.R != 255 {
		t.Fatalf("center should be image, got %v", c)
	}
}

func TestScaleBoxAverages(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	src.SetRGBA(0, 0, color.RGBA{0, 0, 0, 255})
	src.SetRGBA(1, 0, color.RGBA{200, 200, 200, 255})
	src.SetRGBA(0, 1, color.RGBA{0, 0, 0, 255})
	src.SetRGBA(1, 1, color.RGBA{200, 200, 200, 255})
	dst := image.NewRGBA(image.Rect(0, 0, 1, 1))
	Scale(dst, dst.Bounds(), src, src.Bounds())
	if c := dst.RGBAAt(0, 0); c.R != 100 || c.A != 255 {
		t.Fatalf("box average %v", c)
	}
}

func TestScaleYCbCrAndNearest(t *testing.T) {
	src := image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)
	for i := range src.Y {
		src.Y[i] = 235
	}
	for i := range src.Cb {
		src.Cb[i], src.Cr[i] = 128, 128
	}
	dst := image.NewRGBA(image.Rect(0, 0, 8, 8))
	Scale(dst, dst.Bounds(), src, src.Bounds())
	if c := dst.RGBAAt(7, 7); c.R < 230 || c.A != 255 {
		t.Fatalf("nearest from YCbCr %v", c)
	}
}

func BenchmarkRenderRegion1080to768(b *testing.B) {
	src := image.NewYCbCr(image.Rect(0, 0, 1920, 1080), image.YCbCrSubsampleRatio420)
	for i := 0; i < b.N; i++ {
		RenderRegion(src, 1024, 768, "contain", image.Rect(0, 0, 1024, 768), 1024, 768, color.RGBA{A: 255})
	}
}

// columnImage returns a w×h image whose red channel is the column index.
func columnImage(w, h int) *image.RGBA {
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetRGBA(x, y, color.RGBA{uint8(x), 0, 0, 255})
		}
	}
	return src
}

// Wall tiles whose edges fall inside a source pixel must still sample each
// output pixel where its centre lands on the canvas, so the image lines up
// across the bezel (DISPLAY-HW-3).
func TestRenderRegionSeamPlacement(t *testing.T) {
	// 100 source columns on a 1000-unit canvas: 10 units per column.
	src := columnImage(100, 10)
	for _, tile := range []image.Rectangle{
		image.Rect(0, 0, 485, 100),
		image.Rect(515, 0, 1000, 100),
		image.Rect(333, 0, 667, 100),
	} {
		const pw, ph = 1024, 64
		out := RenderRegion(src, 1000, 100, "contain", tile, pw, ph, color.RGBA{})
		rw := float64(tile.Dx())
		worst := 0.0
		for x := 0; x < pw; x++ {
			// Canvas position of the output pixel's centre and the source
			// column the sample came from.
			c := float64(tile.Min.X) + (float64(x)+0.5)*rw/pw
			col := float64(out.RGBAAt(x, ph/2).R)
			var miss float64 // canvas units between c and that column
			if lo, hi := col*10, col*10+10; c < lo {
				miss = lo - c
			} else if c >= hi {
				miss = c - hi
			}
			if px := miss * pw / rw; px > worst {
				worst = px
			}
		}
		if worst > 1 {
			t.Errorf("tile %v: content misplaced by up to %.1f output pixels", tile, worst)
		}
	}
}

// The same holds vertically and when shrinking (box filter).
func TestRenderRegionSeamShrink(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 10, 1000))
	for y := 0; y < 1000; y++ {
		for x := 0; x < 10; x++ {
			src.SetRGBA(x, y, color.RGBA{0, uint8(y / 4), 0, 255}) // 4 rows per value
		}
	}
	// Canvas 10×1000 (1 unit per source pixel); a tile of rows [2, 998)
	// rendered to 249 rows: each output row averages about 4 source rows.
	tile := image.Rect(0, 2, 10, 998)
	out := RenderRegion(src, 10, 1000, "stretch", tile, 5, 249, color.RGBA{})
	for y := 0; y < 249; y++ {
		c := 2 + (float64(y)+0.5)*996/249          // canvas row of the output centre
		got := float64(out.RGBAAt(2, y).G)*4 + 1.5 // centre of the value's 4 rows
		if d := got - c; d > 2.5 || d < -2.5 {
			t.Fatalf("row %d: sampled around source row %.1f, want %.1f", y, got, c)
		}
	}
}

// Scale keeps its mapping when the destination is clipped by dst's bounds.
func TestScaleClippedKeepsMapping(t *testing.T) {
	src := columnImage(10, 1)
	dst := image.NewRGBA(image.Rect(0, 0, 50, 1))
	Scale(dst, image.Rect(-50, 0, 50, 1), src, src.Bounds()) // 10 px per column, left half off-screen
	for x := 0; x < 50; x++ {
		if want := uint8((x + 50) / 10); dst.RGBAAt(x, 0).R != want {
			t.Fatalf("x=%d: column %d, want %d", x, dst.RGBAAt(x, 0).R, want)
		}
	}
}

func TestDecodeLimitedCheck(t *testing.T) {
	data := encodePNG(t, image.NewRGBA(image.Rect(0, 0, 40, 30)))
	var seen image.Config
	refuse := errors.New("refused")
	lim := DefaultLimits
	lim.Check = func(c image.Config) error { seen = c; return refuse }
	if _, _, err := DecodeLimited(bytes.NewReader(data), lim); !errors.Is(err, refuse) || seen.Width != 40 || seen.Height != 30 {
		t.Fatalf("err %v, check saw %+v", err, seen)
	}
	// The dimension limits come first: Check never sees a bomb.
	seen = image.Config{}
	if _, _, err := DecodeLimited(bytes.NewReader(pngBomb(30000, 30000)), lim); !errors.Is(err, ErrTooLarge) || seen.Width != 0 {
		t.Fatalf("bomb: err %v, check saw %+v", err, seen)
	}
	lim.Check = func(image.Config) error { return nil }
	if img, _, err := DecodeLimited(bytes.NewReader(data), lim); err != nil || img.Bounds().Dx() != 40 {
		t.Fatalf("accepted image: %v", err)
	}
}
