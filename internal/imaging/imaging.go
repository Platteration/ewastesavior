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
	"strings"

	_ "golang.org/x/image/bmp"  // register decoder
	_ "golang.org/x/image/webp" // register decoder
)

// Limits bound what DecodeLimited accepts.
type Limits struct {
	MaxSide   int   // max width or height in pixels
	MaxPixels int   // max width*height
	MaxBytes  int64 // max encoded size read from the stream
	// Check, when set, vets the header DecodeLimited parsed (after the
	// dimension limits, before any pixel is decoded); an error refuses the
	// image. The node uses it for its available-memory guard.
	Check func(image.Config) error
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
		// On 32-bit machines the PNG decoder itself refuses dimensions
		// whose pixel buffer would overflow an int.
		if strings.Contains(err.Error(), "dimension overflow") {
			return nil, format, fmt.Errorf("%w: %v", ErrTooLarge, err)
		}
		return nil, "", fmt.Errorf("unrecognized image: %w", err)
	}
	if err := checkDims(cfg.Width, cfg.Height, lim); err != nil {
		return nil, format, err
	}
	if lim.Check != nil {
		if err := lim.Check(cfg); err != nil {
			return nil, format, err
		}
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
//
// Every output pixel samples the source where its own centre lands on the
// canvas, in exact integer arithmetic. Rounding the source window to whole
// pixels instead would shift and stretch each wall tile by up to half a
// source pixel, in opposite directions on the two sides of a bezel.
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
	// The output pixels whose centres lie on the placed image.
	o := image.Rect(
		dr.Min.X+centreIndex(in.Min.X-region.Min.X, region.Dx(), dr.Dx()),
		dr.Min.Y+centreIndex(in.Min.Y-region.Min.Y, region.Dy(), dr.Dy()),
		dr.Min.X+centreIndex(in.Max.X-region.Min.X, region.Dx(), dr.Dx()),
		dr.Min.Y+centreIndex(in.Max.Y-region.Min.Y, region.Dy(), dr.Dy()),
	).Intersect(dr).Intersect(dst.Bounds())
	if o.Empty() {
		return
	}
	// Output position t (from dr.Min) is canvas position
	// region.Min + t*rw/pw, which is source position
	// sb.Min + (canvas - p.Min)*iw/pdx.
	xm := axisMap{
		num:  (int64(region.Min.X) - int64(p.Min.X)) * int64(dr.Dx()) * int64(sb.Dx()),
		step: int64(region.Dx()) * int64(sb.Dx()),
		den:  int64(dr.Dx()) * int64(p.Dx()),
		min:  sb.Min.X, max: sb.Max.X,
	}
	ym := axisMap{
		num:  (int64(region.Min.Y) - int64(p.Min.Y)) * int64(dr.Dy()) * int64(sb.Dy()),
		step: int64(region.Dy()) * int64(sb.Dy()),
		den:  int64(dr.Dy()) * int64(p.Dy()),
		min:  sb.Min.Y, max: sb.Max.Y,
	}
	scaleMapped(dst, o, img, xm, ym, o.Min.X-dr.Min.X, o.Min.Y-dr.Min.Y)
}

// centreIndex returns the first output pixel (of pw spanning rw canvas
// units) whose centre lies at or after canvas offset k, clamped to [0, pw].
func centreIndex(k, rw, pw int) int {
	// Smallest t with (t+0.5)*rw/pw >= k.
	t := ceilDiv(2*int64(k)*int64(pw)-int64(rw), 2*int64(rw))
	return int(min(max(t, 0), int64(pw)))
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func ceilDiv(a, b int64) int64 { return -floorDiv(-a, b) }

// axisMap maps output positions on one axis to source positions: output
// position τ (counted from the origin the map was built for) lands on source
// position min + (num + τ*step)/den. Source pixels are [min, max).
type axisMap struct {
	num, step, den int64
	min, max       int
}

// spans returns, for n output pixels starting at index t0, the source
// pixels [lo[i], hi[i]) each one averages: all the pixels it covers when
// the axis shrinks (box filter), else the one under its centre (nearest).
func (m axisMap) spans(t0, n int) (lo, hi []int, box bool) {
	lo, hi = make([]int, n), make([]int, n)
	box = m.step > m.den
	clamp := func(v, a, b int64) int { return int(min(max(v, a), b)) }
	smin, smax := int64(m.min), int64(m.max)
	for i := range n {
		t := int64(t0 + i)
		if !box {
			c := clamp(smin+floorDiv(2*m.num+(2*t+1)*m.step, 2*m.den), smin, smax-1)
			lo[i], hi[i] = c, c+1
			continue
		}
		a := clamp(smin+floorDiv(m.num+t*m.step, m.den), smin, smax-1)
		b := clamp(smin+floorDiv(m.num+(t+1)*m.step, m.den), smin, smax)
		if b <= a {
			b = a + 1
		}
		lo[i], hi[i] = a, b
	}
	return lo, hi, box
}

// Scale draws src's rectangle sr into dst's rectangle dr using integer
// arithmetic only: an area-averaging box filter on an axis that shrinks and
// nearest neighbor on one that enlarges. Parts of dr outside dst are
// clipped without changing the mapping.
func Scale(dst *image.RGBA, dr image.Rectangle, src image.Image, sr image.Rectangle) {
	sr = sr.Intersect(src.Bounds())
	o := dr.Intersect(dst.Bounds())
	if o.Empty() || sr.Empty() {
		return
	}
	xm := axisMap{step: int64(sr.Dx()), den: int64(dr.Dx()), min: sr.Min.X, max: sr.Max.X}
	ym := axisMap{step: int64(sr.Dy()), den: int64(dr.Dy()), min: sr.Min.Y, max: sr.Max.Y}
	scaleMapped(dst, o, src, xm, ym, o.Min.X-dr.Min.X, o.Min.Y-dr.Min.Y)
}

// scaleMapped fills dst's rectangle o from src through the axis maps; o's
// first column and row are output indices tx and ty of the maps.
func scaleMapped(dst *image.RGBA, o image.Rectangle, src image.Image, xm, ym axisMap, tx, ty int) {
	dw, dh := o.Dx(), o.Dy()
	xlo, xhi, xbox := xm.spans(tx, dw)
	ylo, yhi, ybox := ym.spans(ty, dh)
	get := pixelGetter(src)
	if !xbox && !ybox {
		for y := 0; y < dh; y++ {
			sy := ylo[y]
			row := dst.Pix[dst.PixOffset(o.Min.X, o.Min.Y+y):]
			for x := 0; x < dw; x++ {
				r, g, b, a := get(xlo[x], sy)
				i := 4 * x
				row[i], row[i+1], row[i+2], row[i+3] = r, g, b, a
			}
		}
		return
	}
	// Box filter: each axis is handled independently (separable), which is
	// exact for integer ratios and a good approximation otherwise.
	acc := make([]uint32, 4*dw)
	for y := 0; y < dh; y++ {
		clear(acc)
		rows := uint32(yhi[y] - ylo[y])
		for sy := ylo[y]; sy < yhi[y]; sy++ {
			for x := 0; x < dw; x++ {
				var sr, sg, sb, sa uint32
				for sx := xlo[x]; sx < xhi[x]; sx++ {
					r, g, b, a := get(sx, sy)
					sr += uint32(r)
					sg += uint32(g)
					sb += uint32(b)
					sa += uint32(a)
				}
				n := uint32(xhi[x] - xlo[x])
				acc[4*x] += sr / n
				acc[4*x+1] += sg / n
				acc[4*x+2] += sb / n
				acc[4*x+3] += sa / n
			}
		}
		row := dst.Pix[dst.PixOffset(o.Min.X, o.Min.Y+y):]
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
