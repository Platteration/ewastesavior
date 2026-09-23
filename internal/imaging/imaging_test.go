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
