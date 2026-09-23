package display

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"runtime/debug"
)

// ErrDeviceGone means the display device disappeared or was replaced
// (ENODEV, a fault on the mapping, or a changed device node); the
// controller closes it and opens the display again.
var ErrDeviceGone = errors.New("display device gone")

// fbGeom describes where the visible screen lives in device memory: pixel
// (x, y) is at base + (y+yoff)*lineLen + (x+xoff)*Bpp.
type fbGeom struct {
	w, h       int // visible size (xres, yres)
	xoff, yoff int // panning offsets
	lineLen    int // bytes per line (fix.line_length)
	base       int // offset of the virtual screen's first byte in the target
}

// fbTarget is where converted rows go: a memory mapping (or plain byte
// slice) or, when mmap is unavailable, an io.WriterAt (pwrite).
type fbTarget struct {
	mem []byte
	wa  io.WriterAt
}

const (
	bandRows = 16 // rows converted per band (bounded scratch buffer)
	tileCols = 64 // column tile width for 90/270 rotation
)

// fbCore keeps a RAM shadow of the visible screen in device format (stride =
// lineLen), converts frames into it with rotation, and copies only rows that
// actually changed into the target. The target is never read.
type fbCore struct {
	g       fbGeom
	pf      *pixelFormat
	rotate  int
	shadow  []byte // h rows of lineLen bytes
	valid   bool   // shadow mirrors the target
	band    []byte
	changed []bool
	target  fbTarget
	rows    int // rows written to the target so far (statistics)
}

func newFBCore(g fbGeom, pf *pixelFormat, rotate int, t fbTarget) (*fbCore, error) {
	bpp := pf.bytesPP()
	if g.w <= 0 || g.h <= 0 || g.w > 16384 || g.h > 16384 {
		return nil, fmt.Errorf("unsupported resolution %dx%d", g.w, g.h)
	}
	if g.xoff < 0 || g.yoff < 0 || g.lineLen < (g.xoff+g.w)*bpp {
		return nil, fmt.Errorf("line length %d too short for %dx%d+%d at %d bpp", g.lineLen, g.w, g.h, g.xoff, pf.bpp)
	}
	if t.mem != nil && g.base+(g.yoff+g.h)*g.lineLen > len(t.mem) {
		return nil, fmt.Errorf("framebuffer memory (%d bytes) too small for %dx%d+%d+%d", len(t.mem), g.w, g.h, g.xoff, g.yoff)
	}
	if t.mem == nil && t.wa == nil {
		return nil, errors.New("no framebuffer target")
	}
	c := &fbCore{
		g:       g,
		pf:      pf,
		shadow:  make([]byte, g.h*g.lineLen),
		changed: make([]bool, g.h),
		target:  t,
	}
	if err := c.setRotate(rotate); err != nil {
		return nil, err
	}
	return c, nil
}

// normRotate maps any multiple of 90 degrees to 0, 90, 180 or 270.
func normRotate(deg int) (int, error) {
	d := ((deg % 360) + 360) % 360
	if d%90 != 0 {
		return 0, fmt.Errorf("rotation must be a multiple of 90 degrees, got %d", deg)
	}
	return d, nil
}

func (c *fbCore) setRotate(deg int) error {
	d, err := normRotate(deg)
	if err != nil {
		return err
	}
	c.rotate = d
	return nil
}

// logicalSize is the frame size callers render at.
func (c *fbCore) logicalSize() (int, int) {
	if c.rotate == 90 || c.rotate == 270 {
		return c.g.h, c.g.w
	}
	return c.g.w, c.g.h
}

// toPhysical maps a logical rectangle (origin 0,0) to physical pixels.
// Rotation r turns the logical image r degrees clockwise onto the panel:
// with 90, logical top-left lands at physical top-right.
func (c *fbCore) toPhysical(r image.Rectangle) image.Rectangle {
	W, H := c.g.w, c.g.h
	switch c.rotate {
	case 90: // (x, y) -> (W-1-y, x)
		return image.Rect(W-r.Max.Y, r.Min.X, W-r.Min.Y, r.Max.X)
	case 180: // (x, y) -> (W-1-x, H-1-y)
		return image.Rect(W-r.Max.X, H-r.Max.Y, W-r.Min.X, H-r.Min.Y)
	case 270: // (x, y) -> (y, H-1-x)
		return image.Rect(r.Min.Y, H-r.Max.X, r.Max.Y, H-r.Min.X)
	}
	return r
}

// source returns the byte offset in img.Pix of the logical pixel shown at
// physical (px, py), and the byte step to the logical pixel shown at
// (px+1, py).
func (c *fbCore) source(img *image.RGBA, px, py int) (si, step int) {
	W, H := c.g.w, c.g.h
	var lx, ly int
	switch c.rotate {
	case 90:
		lx, ly, step = py, W-1-px, -img.Stride
	case 180:
		lx, ly, step = W-1-px, H-1-py, -4
	case 270:
		lx, ly, step = H-1-py, px, img.Stride
	default:
		lx, ly, step = px, py, 4
	}
	return img.PixOffset(img.Rect.Min.X+lx, img.Rect.Min.Y+ly), step
}

// invalidate forgets what the target holds, so the next show rewrites
// every row (after a VT switch or reopen).
func (c *fbCore) invalidate() { c.valid = false }

// show converts the dirty parts of the logical frame img and writes the
// rows that changed.
func (c *fbCore) show(img *image.RGBA, dirty []image.Rectangle) error {
	lw, lh := c.logicalSize()
	if img == nil || img.Rect.Dx() != lw || img.Rect.Dy() != lh {
		var got image.Rectangle
		if img != nil {
			got = img.Rect
		}
		return fmt.Errorf("frame is %dx%d, display wants %dx%d", got.Dx(), got.Dy(), lw, lh)
	}
	full := image.Rect(0, 0, lw, lh)
	if len(dirty) == 0 || !c.valid {
		dirty = []image.Rectangle{full}
	}
	for _, d := range dirty {
		d = d.Sub(img.Rect.Min).Intersect(full)
		if !d.Empty() {
			c.convert(img, c.toPhysical(d))
		}
	}
	err := c.flush()
	if err == nil {
		c.valid = true
	}
	return err
}

// convert renders the physical rectangle p into the shadow band by band,
// marking rows whose bytes differ from the shadow.
func (c *fbCore) convert(img *image.RGBA, p image.Rectangle) {
	bpp := c.pf.bytesPP()
	rowBytes := p.Dx() * bpp
	if need := bandRows * rowBytes; cap(c.band) < need {
		c.band = make([]byte, need)
	}
	rotated := c.rotate == 90 || c.rotate == 270
	for by := p.Min.Y; by < p.Max.Y; by += bandRows {
		bh := min(bandRows, p.Max.Y-by)
		band := c.band[:bh*rowBytes]
		if rotated {
			// Column tiles keep the source walk (down a column of the
			// logical image) inside a few cache lines per row.
			for tx := p.Min.X; tx < p.Max.X; tx += tileCols {
				n := min(tileCols, p.Max.X-tx)
				for y := 0; y < bh; y++ {
					si, step := c.source(img, tx, by+y)
					o := y*rowBytes + (tx-p.Min.X)*bpp
					c.pf.conv(c.pf, band[o:o+n*bpp], img.Pix, si, step, n)
				}
			}
		} else {
			for y := 0; y < bh; y++ {
				si, step := c.source(img, p.Min.X, by+y)
				c.pf.conv(c.pf, band[y*rowBytes:(y+1)*rowBytes], img.Pix, si, step, p.Dx())
			}
		}
		for y := 0; y < bh; y++ {
			py := by + y
			o := py*c.g.lineLen + (c.g.xoff+p.Min.X)*bpp
			row := band[y*rowBytes : (y+1)*rowBytes]
			if c.valid && bytes.Equal(c.shadow[o:o+rowBytes], row) {
				continue
			}
			copy(c.shadow[o:o+rowBytes], row)
			c.changed[py] = true
		}
	}
}

// fillShadow sets every visible pixel of the shadow to one device value and
// marks all rows changed (used for the black-frame blank).
func (c *fbCore) fillShadow(v uint32) {
	bpp := c.pf.bytesPP()
	for y := 0; y < c.g.h; y++ {
		row := c.shadow[y*c.g.lineLen+c.g.xoff*bpp : y*c.g.lineLen+(c.g.xoff+c.g.w)*bpp]
		for i := 0; i < len(row); i += bpp {
			for k := 0; k < bpp; k++ {
				row[i+k] = byte(v >> (8 * k))
			}
		}
		c.changed[y] = true
	}
}

// flush copies changed rows to the target: the visible span of each row
// for a mapping, whole lines per contiguous run for pwrite.
func (c *fbCore) flush() error {
	bpp := c.pf.bytesPP()
	ll := c.g.lineLen
	x0, x1 := c.g.xoff*bpp, (c.g.xoff+c.g.w)*bpp
	for y := 0; y < c.g.h; {
		if !c.changed[y] {
			y++
			continue
		}
		end := y
		for end < c.g.h && c.changed[end] {
			c.changed[end] = false
			end++
		}
		var err error
		if c.target.mem != nil {
			err = copyRows(c.target.mem, c.shadow, c.g.base+c.g.yoff*ll, ll, y, end, x0, x1)
		} else {
			off := int64(c.g.base + (c.g.yoff+y)*ll)
			_, err = c.target.wa.WriteAt(c.shadow[y*ll:end*ll], off)
		}
		if err != nil {
			for ; y < c.g.h; y++ { // retry everything after a failure
				c.changed[y] = true
			}
			c.valid = false
			return err
		}
		c.rows += end - y
		y = end
	}
	return nil
}

// copyRows copies rows [y0, y1) of shadow into mem. A memory mapping whose
// device went away may fault (SIGBUS); that becomes ErrDeviceGone instead
// of crashing the agent.
func copyRows(mem, shadow []byte, base, ll, y0, y1, x0, x1 int) (err error) {
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: fault writing framebuffer: %v", ErrDeviceGone, r)
		}
	}()
	for y := y0; y < y1; y++ {
		o := base + y*ll
		copy(mem[o+x0:o+x1], shadow[y*ll+x0:y*ll+x1])
	}
	return nil
}

// decode converts the target memory back into a physical RGBA image.
func (c *fbCore) decode(mem []byte) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, c.g.w, c.g.h))
	bpp := c.pf.bytesPP()
	for y := 0; y < c.g.h; y++ {
		for x := 0; x < c.g.w; x++ {
			o := c.g.base + (y+c.g.yoff)*c.g.lineLen + (x+c.g.xoff)*bpp
			if o+bpp > len(mem) {
				continue
			}
			var v uint32
			for k := 0; k < bpp; k++ {
				v |= uint32(mem[o+k]) << (8 * k)
			}
			r, g, b := c.pf.unpack(v)
			i := out.PixOffset(x, y)
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = r, g, b, 0xff
		}
	}
	return out
}

// unrotate maps a physical image back to logical orientation.
func unrotate(phys *image.RGBA, rotate int) *image.RGBA {
	W, H := phys.Rect.Dx(), phys.Rect.Dy()
	lw, lh := W, H
	if rotate == 90 || rotate == 270 {
		lw, lh = H, W
	}
	out := image.NewRGBA(image.Rect(0, 0, lw, lh))
	for ly := 0; ly < lh; ly++ {
		for lx := 0; lx < lw; lx++ {
			var px, py int
			switch rotate {
			case 90:
				px, py = W-1-ly, lx
			case 180:
				px, py = W-1-lx, H-1-ly
			case 270:
				px, py = ly, H-1-lx
			default:
				px, py = lx, ly
			}
			copy(out.Pix[out.PixOffset(lx, ly):out.PixOffset(lx, ly)+4], phys.Pix[phys.PixOffset(px, py):phys.PixOffset(px, py)+4])
		}
	}
	return out
}
