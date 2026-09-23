// Package imaging decodes untrusted images safely and maps them onto
// screens and wall canvases with integer-only scaling (fast on 32-bit CPUs
// without SSE2). It is shared by the node's display controller and the
// hive's render endpoint. See docs/DESIGN.md sections 11.4 and 11.5.
package imaging

import (
	"bufio"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"  // register decoder
	_ "image/jpeg" // register decoder
	_ "image/png"  // register decoder
	"io"

	_ "golang.org/x/image/bmp"  // register decoder
	_ "golang.org/x/image/webp" // register decoder
)

// Limits bound what DecodeLimited accepts.
type Limits struct {
	MaxSide   int   // max width or height in pixels
	MaxPixels int   // max width*height
	MaxBytes  int64 // max encoded size read from the stream
}

// DefaultLimits matches proto.MaxMediaSide/MaxMediaPixels/MaxMediaBytes.
var DefaultLimits = Limits{MaxSide: 8192, MaxPixels: 16 << 20, MaxBytes: 64 << 20}

// maxPeek bounds how much of the stream is buffered to read the header.
const maxPeek = 1 << 20

// ErrTooLarge is returned (wrapped) when an image exceeds the limits.
var ErrTooLarge = errors.New("image too large")

// DecodeLimited decodes an image after checking its declared dimensions,
// so a tiny file declaring a huge canvas can't exhaust memory.
func DecodeLimited(r io.Reader, lim Limits) (image.Image, string, error) {
	if lim.MaxBytes <= 0 {
		lim = DefaultLimits
	}
	// Peek enough for DecodeConfig of every supported format. Camera JPEGs
	// can carry large EXIF/ICC blocks before the frame header, so retry
	// with a bigger window before giving up.
	br := bufio.NewReaderSize(io.LimitReader(r, lim.MaxBytes+1), maxPeek)
	var (
		cfg    image.Config
		format string
		err    error
	)
	for _, n := range []int{64 << 10, maxPeek} {
		head, perr := br.Peek(n)
		if perr != nil && perr != io.EOF && perr != bufio.ErrBufferFull {
			return nil, "", perr
		}
		cfg, format, err = image.DecodeConfig(bytesReader(head))
		if err == nil || len(head) < n {
			break
		}
	}
	if err != nil {
		return nil, "", fmt.Errorf("unrecognized image: %w", err)
	}
	if err := checkDims(cfg.Width, cfg.Height, lim); err != nil {
		return nil, format, err
	}
	cr := &countingReader{r: br, max: lim.MaxBytes}
	img, format, err := image.Decode(cr)
	if cr.over {
		return nil, format, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, lim.MaxBytes)
	}
	if err != nil {
		return nil, format, err
	}
	b := img.Bounds()
	if err := checkDims(b.Dx(), b.Dy(), lim); err != nil {
		return nil, format, err
	}
	return img, format, nil
}

func checkDims(w, h int, lim Limits) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("image has no pixels")
	}
	if w > lim.MaxSide || h > lim.MaxSide || int64(w)*int64(h) > int64(lim.MaxPixels) {
		return fmt.Errorf("%w: %dx%d (max side %d, max %d MP)", ErrTooLarge, w, h, lim.MaxSide, lim.MaxPixels>>20)
	}
	return nil
}

type countingReader struct {
	r    io.Reader
	n    int64
	max  int64
	over bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		c.over = true
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

type sliceReader struct {
	b []byte
	i int
}

func bytesReader(b []byte) *sliceReader { return &sliceReader{b: b} }

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.i >= len(s.b) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.i:])
	s.i += n
	return n, nil
}

// Place returns where an iw×ih image lands on a cw×ch canvas for a fit mode:
// "contain" (default: whole image visible, centered, letterboxed), "cover"
// (canvas filled, overflow cropped; the rectangle may extend beyond the
// canvas) or "stretch" (exactly the canvas).
func Place(iw, ih, cw, ch int, fit string) image.Rectangle {
	if iw <= 0 || ih <= 0 || cw <= 0 || ch <= 0 {
		return image.Rectangle{}
	}
	switch fit {
	case "stretch":
		return image.Rect(0, 0, cw, ch)
	case "cover":
		// scale = max(cw/iw, ch/ih)
		if int64(cw)*int64(ih) >= int64(ch)*int64(iw) {
			h := int(int64(ih) * int64(cw) / int64(iw))
			y := (ch - h) / 2
			return image.Rect(0, y, cw, y+h)
		}
		w := int(int64(iw) * int64(ch) / int64(ih))
		x := (cw - w) / 2
		return image.Rect(x, 0, x+w, ch)
	default: // contain
		if int64(cw)*int64(ih) <= int64(ch)*int64(iw) {
			h := int(int64(ih) * int64(cw) / int64(iw))
			if h < 1 {
				h = 1
			}
			y := (ch - h) / 2
			return image.Rect(0, y, cw, y+h)
		}
		w := int(int64(iw) * int64(ch) / int64(ih))
		if w < 1 {
			w = 1
		}
		x := (cw - w) / 2
		return image.Rect(x, 0, x+w, ch)
	}
}

// RenderRegion renders the part `region` (canvas coordinates) of a cw×ch
// canvas, on which img is placed with fit, into a new pw×ph image. Canvas
// areas not covered by the image are filled with bg. Only the needed source
// rectangle is scaled; the full canvas is never allocated.
func RenderRegion(img image.Image, cw, ch int, fit string, region image.Rectangle, pw, ph int, bg color.RGBA) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, pw, ph))
	RenderRegionInto(dst, dst.Bounds(), img, cw, ch, fit, region, bg)
	return dst
}

// RenderRegionInto is RenderRegion drawing into dr of an existing image.
func RenderRegionInto(dst *image.RGBA, dr image.Rectangle, img image.Image, cw, ch int, fit string, region image.Rectangle, bg color.RGBA) {
	draw.Draw(dst, dr, image.NewUniform(bg), image.Point{}, draw.Src)
	if img == nil || region.Empty() || dr.Empty() {
		return
	}
	sb := img.Bounds()
	p := Place(sb.Dx(), sb.Dy(), cw, ch, fit)
	in := p.Intersect(region)
	if in.Empty() {
		return
	}
	rw, rh := int64(region.Dx()), int64(region.Dy())
	pw, ph := int64(dr.Dx()), int64(dr.Dy())
	// Output rectangle for the intersection.
	o := image.Rect(
		dr.Min.X+int(int64(in.Min.X-region.Min.X)*pw/rw),
		dr.Min.Y+int(int64(in.Min.Y-region.Min.Y)*ph/rh),
		dr.Min.X+int(int64(in.Max.X-region.Min.X)*pw/rw),
		dr.Min.Y+int(int64(in.Max.Y-region.Min.Y)*ph/rh),
	)
	// Source rectangle for the intersection.
	iw, ih := int64(sb.Dx()), int64(sb.Dy())
	pdx, pdy := int64(p.Dx()), int64(p.Dy())
	s := image.Rect(
		sb.Min.X+int(int64(in.Min.X-p.Min.X)*iw/pdx),
		sb.Min.Y+int(int64(in.Min.Y-p.Min.Y)*ih/pdy),
		sb.Min.X+int((int64(in.Max.X-p.Min.X)*iw+pdx-1)/pdx),
		sb.Min.Y+int((int64(in.Max.Y-p.Min.Y)*ih+pdy-1)/pdy),
	).Intersect(sb)
	if o.Empty() || s.Empty() {
		return
	}
	Scale(dst, o, img, s)
}

// Scale draws src's rectangle sr into dst's rectangle dr using integer
// arithmetic only: an area-averaging box filter when shrinking and nearest
// neighbor when enlarging.
func Scale(dst *image.RGBA, dr image.Rectangle, src image.Image, sr image.Rectangle) {
	dr = dr.Intersect(dst.Bounds())
	sr = sr.Intersect(src.Bounds())
	if dr.Empty() || sr.Empty() {
		return
	}
	if sr.Dx() > dr.Dx() || sr.Dy() > dr.Dy() {
		scaleBox(dst, dr, src, sr)
		return
	}
	scaleNearest(dst, dr, src, sr)
}

func scaleNearest(dst *image.RGBA, dr image.Rectangle, src image.Image, sr image.Rectangle) {
	dw, dh := dr.Dx(), dr.Dy()
	sw, sh := sr.Dx(), sr.Dy()
	xs := make([]int, dw)
	for x := 0; x < dw; x++ {
		xs[x] = sr.Min.X + (2*x+1)*sw/(2*dw)
	}
	get := pixelGetter(src)
	for y := 0; y < dh; y++ {
		sy := sr.Min.Y + (2*y+1)*sh/(2*dh)
		row := dst.Pix[dst.PixOffset(dr.Min.X, dr.Min.Y+y):]
		for x := 0; x < dw; x++ {
			r, g, b, a := get(xs[x], sy)
			i := 4 * x
			row[i], row[i+1], row[i+2], row[i+3] = r, g, b, a
		}
	}
}

// scaleBox averages all source pixels that map onto each destination pixel.
// Each axis is handled independently (separable), which is exact for
// integer ratios and a good approximation otherwise.
func scaleBox(dst *image.RGBA, dr image.Rectangle, src image.Image, sr image.Rectangle) {
	dw, dh := dr.Dx(), dr.Dy()
	sw, sh := sr.Dx(), sr.Dy()
	x0 := make([]int, dw+1)
	for x := 0; x <= dw; x++ {
		x0[x] = sr.Min.X + x*sw/dw
	}
	get := pixelGetter(src)
	acc := make([]uint32, 4*dw)
	for y := 0; y < dh; y++ {
		sy0 := sr.Min.Y + y*sh/dh
		sy1 := sr.Min.Y + (y+1)*sh/dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for i := range acc {
			acc[i] = 0
		}
		rows := uint32(sy1 - sy0)
		for sy := sy0; sy < sy1; sy++ {
			for x := 0; x < dw; x++ {
				a, b := x0[x], x0[x+1]
				if b <= a {
					b = a + 1
				}
				var sr_, sg, sb_, sa uint32
				for sx := a; sx < b; sx++ {
					r, g, bb, aa := get(sx, sy)
					sr_ += uint32(r)
					sg += uint32(g)
					sb_ += uint32(bb)
					sa += uint32(aa)
				}
				n := uint32(b - a)
				acc[4*x] += sr_ / n
				acc[4*x+1] += sg / n
				acc[4*x+2] += sb_ / n
				acc[4*x+3] += sa / n
			}
		}
		row := dst.Pix[dst.PixOffset(dr.Min.X, dr.Min.Y+y):]
		for i := 0; i < 4*dw; i++ {
			row[i] = uint8(acc[i] / rows)
		}
	}
}

// pixelGetter returns a fast accessor returning premultiplied 8-bit RGBA.
func pixelGetter(src image.Image) func(x, y int) (r, g, b, a uint8) {
	switch s := src.(type) {
	case *image.RGBA:
		return func(x, y int) (uint8, uint8, uint8, uint8) {
			i := s.PixOffset(x, y)
			return s.Pix[i], s.Pix[i+1], s.Pix[i+2], s.Pix[i+3]
		}
	case *image.NRGBA:
		return func(x, y int) (uint8, uint8, uint8, uint8) {
			i := s.PixOffset(x, y)
			a := uint32(s.Pix[i+3])
			return uint8(uint32(s.Pix[i]) * a / 255), uint8(uint32(s.Pix[i+1]) * a / 255), uint8(uint32(s.Pix[i+2]) * a / 255), uint8(a)
		}
	case *image.YCbCr:
		return func(x, y int) (uint8, uint8, uint8, uint8) {
			yi := s.YOffset(x, y)
			ci := s.COffset(x, y)
			r, g, b := color.YCbCrToRGB(s.Y[yi], s.Cb[ci], s.Cr[ci])
			return r, g, b, 255
		}
	case *image.Gray:
		return func(x, y int) (uint8, uint8, uint8, uint8) {
			v := s.Pix[s.PixOffset(x, y)]
			return v, v, v, 255
		}
	case *image.Paletted:
		pal := make([][4]uint8, len(s.Palette))
		for i, c := range s.Palette {
			r, g, b, a := c.RGBA()
			pal[i] = [4]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
		}
		return func(x, y int) (uint8, uint8, uint8, uint8) {
			idx := int(s.Pix[s.PixOffset(x, y)])
			if idx >= len(pal) {
				return 0, 0, 0, 0
			}
			p := pal[idx]
			return p[0], p[1], p[2], p[3]
		}
	}
	return func(x, y int) (uint8, uint8, uint8, uint8) {
		r, g, b, a := src.At(x, y).RGBA()
		return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)
	}
}
