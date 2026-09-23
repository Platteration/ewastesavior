package display

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// bitfield describes one color channel inside a pixel value, as in the
// kernel's struct fb_bitfield: the channel occupies bits [off, off+len) of
// the little-endian pixel word.
type bitfield struct {
	off, len uint32
	msbRight bool
}

// convFunc converts n logical pixels into device format. src holds RGBA
// bytes; the first pixel is at si and each next pixel is step bytes away
// (4, -4 or ±stride, which is how rotation is applied). dst receives n
// packed device pixels.
type convFunc func(pf *pixelFormat, dst, src []byte, si, step, n int)

// pixelFormat is a framebuffer pixel layout plus its converter.
type pixelFormat struct {
	name       string
	bpp        int // bits per pixel: 16, 24 or 32
	r, g, b, a bitfield
	conv       convFunc
	alpha      uint32 // constant OR'd into every pixel (alpha bits all set)
	lut        *[3][256]uint32
}

// bytesPP returns bytes per pixel.
func (pf *pixelFormat) bytesPP() int { return pf.bpp / 8 }

// namedFormats are the layouts with a name (DRM fourcc style: channels
// listed from the most significant bit of the little-endian word).
var namedFormats = map[string]struct {
	bpp        int
	r, g, b, a bitfield
}{
	"XRGB8888":    {32, bitfield{16, 8, false}, bitfield{8, 8, false}, bitfield{0, 8, false}, bitfield{}},
	"ARGB8888":    {32, bitfield{16, 8, false}, bitfield{8, 8, false}, bitfield{0, 8, false}, bitfield{24, 8, false}},
	"XBGR8888":    {32, bitfield{0, 8, false}, bitfield{8, 8, false}, bitfield{16, 8, false}, bitfield{}},
	"ABGR8888":    {32, bitfield{0, 8, false}, bitfield{8, 8, false}, bitfield{16, 8, false}, bitfield{24, 8, false}},
	"RGBX8888":    {32, bitfield{24, 8, false}, bitfield{16, 8, false}, bitfield{8, 8, false}, bitfield{}},
	"XRGB2101010": {32, bitfield{20, 10, false}, bitfield{10, 10, false}, bitfield{0, 10, false}, bitfield{}},
	"RGB888":      {24, bitfield{16, 8, false}, bitfield{8, 8, false}, bitfield{0, 8, false}, bitfield{}},
	"BGR888":      {24, bitfield{0, 8, false}, bitfield{8, 8, false}, bitfield{16, 8, false}, bitfield{}},
	"RGB565":      {16, bitfield{11, 5, false}, bitfield{5, 6, false}, bitfield{0, 5, false}, bitfield{}},
	"BGR565":      {16, bitfield{0, 5, false}, bitfield{5, 6, false}, bitfield{11, 5, false}, bitfield{}},
	"XRGB1555":    {16, bitfield{10, 5, false}, bitfield{5, 5, false}, bitfield{0, 5, false}, bitfield{}},
	"ARGB1555":    {16, bitfield{10, 5, false}, bitfield{5, 5, false}, bitfield{0, 5, false}, bitfield{15, 1, false}},
	"XRGB4444":    {16, bitfield{8, 4, false}, bitfield{4, 4, false}, bitfield{0, 4, false}, bitfield{}},
}

// formatByName returns a named layout.
func formatByName(name string) (*pixelFormat, error) {
	f, ok := namedFormats[strings.ToUpper(name)]
	if !ok {
		return nil, fmt.Errorf("unknown pixel format %q", name)
	}
	return newPixelFormat(f.bpp, f.r, f.g, f.b, f.a)
}

// defaultFormat is the usual layout for a depth.
func defaultFormat(bpp int) string {
	switch bpp {
	case 16:
		return "RGB565"
	case 24:
		return "RGB888"
	}
	return "XRGB8888"
}

// newPixelFormat validates a bitfield layout and picks a converter: a fast
// path for the common layouts, else a lookup-table path for any bitfields.
func newPixelFormat(bpp int, r, g, b, a bitfield) (*pixelFormat, error) {
	if bpp != 16 && bpp != 24 && bpp != 32 {
		return nil, fmt.Errorf("unsupported depth %d bpp (need 16, 24 or 32)", bpp)
	}
	for i, f := range []bitfield{r, g, b, a} {
		if f.len == 0 && i == 3 {
			continue
		}
		if f.len == 0 || f.len > 16 || f.off+f.len > uint32(bpp) {
			return nil, fmt.Errorf("unsupported bitfield %d:%d for %d bpp", f.off, f.len, bpp)
		}
	}
	pf := &pixelFormat{bpp: bpp, r: r, g: g, b: b, a: a}
	if a.len > 0 {
		pf.alpha = ((1 << a.len) - 1) << a.off
	}
	pf.name = pf.detectName()
	switch {
	case pf.anyMSBRight():
		pf.conv = convGeneric
	case pf.is(32, 16, 8, 0):
		pf.conv = convXRGB8888
	case pf.is(32, 0, 8, 16):
		pf.conv = convXBGR8888
	case pf.is(24, 16, 8, 0):
		pf.conv = convRGB888
	case pf.is(24, 0, 8, 16):
		pf.conv = convBGR888
	case bpp == 16 && pf.lens(5, 6, 5) && r.off == 11 && g.off == 5 && b.off == 0:
		pf.conv = convRGB565
	case bpp == 16 && pf.lens(5, 5, 5) && r.off == 10 && g.off == 5 && b.off == 0:
		pf.conv = convXRGB1555
	default:
		pf.conv = convGeneric
	}
	pf.buildLUT() // 3 KiB; also used by pack
	return pf, nil
}

func (pf *pixelFormat) anyMSBRight() bool { return pf.r.msbRight || pf.g.msbRight || pf.b.msbRight }

func (pf *pixelFormat) lens(r, g, b uint32) bool {
	return pf.r.len == r && pf.g.len == g && pf.b.len == b
}

// is reports an 8-bit-per-channel layout at the given offsets.
func (pf *pixelFormat) is(bpp int, r, g, b uint32) bool {
	return pf.bpp == bpp && pf.lens(8, 8, 8) && pf.r.off == r && pf.g.off == g && pf.b.off == b
}

func (pf *pixelFormat) detectName() string {
	if !pf.anyMSBRight() {
		for name, f := range namedFormats {
			if f.bpp == pf.bpp && f.r == pf.r && f.g == pf.g && f.b == pf.b && f.a == pf.a {
				return name
			}
		}
	}
	s := fmt.Sprintf("R%d:%d G%d:%d B%d:%d", pf.r.off, pf.r.len, pf.g.off, pf.g.len, pf.b.off, pf.b.len)
	if pf.a.len > 0 {
		s += fmt.Sprintf(" A%d:%d", pf.a.off, pf.a.len)
	}
	return fmt.Sprintf("%s @%dbpp", s, pf.bpp)
}

// buildLUT precomputes each channel's contribution for all 8-bit values:
// the top bits for narrow channels (exactly what the fast paths do), bit
// replication for channels wider than 8 bits.
func (pf *pixelFormat) buildLUT() {
	pf.lut = new([3][256]uint32)
	for c, f := range []bitfield{pf.r, pf.g, pf.b} {
		for v := uint32(0); v < 256; v++ {
			var q uint32
			if f.len <= 8 {
				q = v >> (8 - f.len)
			} else {
				q = v<<(f.len-8) | v>>(16-f.len)
			}
			if f.msbRight {
				q = reverseBits(q, f.len)
			}
			pf.lut[c][v] = q << f.off
		}
	}
}

func reverseBits(v, n uint32) uint32 {
	var out uint32
	for i := uint32(0); i < n; i++ {
		out = out<<1 | (v>>i)&1
	}
	return out
}

// pack converts one 8-bit RGB color into the device pixel value.
func (pf *pixelFormat) pack(r, g, b uint8) uint32 {
	return pf.lut[0][r] | pf.lut[1][g] | pf.lut[2][b] | pf.alpha
}

// unpack converts a device pixel value back to 8-bit RGB (for tests and
// MemDevice.Decode).
func (pf *pixelFormat) unpack(v uint32) (r, g, b uint8) {
	ch := func(f bitfield) uint8 {
		maxv := uint32(1)<<f.len - 1
		q := (v >> f.off) & maxv
		if f.msbRight {
			q = reverseBits(q, f.len)
		}
		return uint8((q*255 + maxv/2) / maxv)
	}
	return ch(pf.r), ch(pf.g), ch(pf.b)
}

// Fast paths. RGBA source pixels are read as one little-endian word
// (R | G<<8 | B<<16 | A<<24), which the compiler turns into a single load.

func convXRGB8888(pf *pixelFormat, dst, src []byte, si, step, n int) {
	a := pf.alpha
	for i := 0; i < n; i++ {
		v := binary.LittleEndian.Uint32(src[si:])
		binary.LittleEndian.PutUint32(dst[4*i:], (v>>16)&0xff|v&0xff00|(v&0xff)<<16|a)
		si += step
	}
}

func convXBGR8888(pf *pixelFormat, dst, src []byte, si, step, n int) {
	a := pf.alpha
	for i := 0; i < n; i++ {
		v := binary.LittleEndian.Uint32(src[si:])
		binary.LittleEndian.PutUint32(dst[4*i:], v&0xffffff|a)
		si += step
	}
}

func convRGB888(_ *pixelFormat, dst, src []byte, si, step, n int) {
	for i := 0; i < n; i++ {
		s := src[si : si+4 : si+4]
		d := dst[3*i : 3*i+3 : 3*i+3]
		d[0], d[1], d[2] = s[2], s[1], s[0]
		si += step
	}
}

func convBGR888(_ *pixelFormat, dst, src []byte, si, step, n int) {
	for i := 0; i < n; i++ {
		s := src[si : si+4 : si+4]
		d := dst[3*i : 3*i+3 : 3*i+3]
		d[0], d[1], d[2] = s[0], s[1], s[2]
		si += step
	}
}

func convRGB565(_ *pixelFormat, dst, src []byte, si, step, n int) {
	for i := 0; i < n; i++ {
		v := binary.LittleEndian.Uint32(src[si:])
		binary.LittleEndian.PutUint16(dst[2*i:], uint16((v&0xf8)<<8|(v>>5)&0x7e0|(v>>19)&0x1f))
		si += step
	}
}

func convXRGB1555(pf *pixelFormat, dst, src []byte, si, step, n int) {
	a := pf.alpha
	for i := 0; i < n; i++ {
		v := binary.LittleEndian.Uint32(src[si:])
		binary.LittleEndian.PutUint16(dst[2*i:], uint16((v&0xf8)<<7|(v>>6)&0x3e0|(v>>19)&0x1f|a))
		si += step
	}
}

func convGeneric(pf *pixelFormat, dst, src []byte, si, step, n int) {
	lut := pf.lut
	bpp := pf.bpp / 8
	for i := 0; i < n; i++ {
		s := src[si : si+4 : si+4]
		v := lut[0][s[0]] | lut[1][s[1]] | lut[2][s[2]] | pf.alpha
		d := dst[bpp*i : bpp*i+bpp]
		for k := range d {
			d[k] = byte(v >> (8 * k))
		}
		si += step
	}
}
