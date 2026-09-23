package display

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Theme colors for the built-in screens.
var (
	colBG     = rgb(0x10, 0x14, 0x1c)
	colPanel  = rgb(0x1d, 0x25, 0x33)
	colTrack  = rgb(0x2c, 0x36, 0x46)
	colText   = rgb(0xf2, 0xf4, 0xf7)
	colMuted  = rgb(0x9a, 0xa6, 0xb8)
	colGood   = rgb(0x3c, 0xc8, 0x5a)
	colWarn   = rgb(0xf2, 0xb3, 0x24)
	colBad    = rgb(0xe8, 0x4a, 0x3c)
	colAccent = rgb(0x4a, 0x9c, 0xf0)
	colBlack  = rgb(0, 0, 0)
	colWhite  = rgb(0xff, 0xff, 0xff)
)

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{r, g, b, 0xff} }

// mix blends a and b: num/den of a plus the rest of b.
func mix(a, b color.RGBA, num, den int) color.RGBA {
	ch := func(x, y uint8) uint8 { return uint8((int(x)*num + int(y)*(den-num)) / den) }
	return color.RGBA{ch(a.R, b.R), ch(a.G, b.G), ch(a.B, b.B), 0xff}
}

// specColor parses a spec color, falling back to def.
func specColor(s string, def color.RGBA) color.RGBA {
	if r, g, b, ok := proto.ParseColor(s); ok {
		return rgb(r, g, b)
	}
	return def
}

// fill paints r (clipped to dst) with c.
func fill(dst *image.RGBA, r image.Rectangle, c color.RGBA) {
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	// Fill the first row, then copy it: faster than draw.Draw on old CPUs.
	i0 := dst.PixOffset(r.Min.X, r.Min.Y)
	row := dst.Pix[i0 : i0+4*r.Dx()]
	for i := 0; i < len(row); i += 4 {
		row[i], row[i+1], row[i+2], row[i+3] = c.R, c.G, c.B, c.A
	}
	for y := r.Min.Y + 1; y < r.Max.Y; y++ {
		i := dst.PixOffset(r.Min.X, y)
		copy(dst.Pix[i:i+len(row)], row)
	}
}

// strokeRect draws a w-pixel frame inside r.
func strokeRect(dst *image.RGBA, r image.Rectangle, w int, c color.RGBA) {
	if w <= 0 {
		return
	}
	fill(dst, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+w), c)
	fill(dst, image.Rect(r.Min.X, r.Max.Y-w, r.Max.X, r.Max.Y), c)
	fill(dst, image.Rect(r.Min.X, r.Min.Y, r.Min.X+w, r.Max.Y), c)
	fill(dst, image.Rect(r.Max.X-w, r.Min.Y, r.Max.X, r.Max.Y), c)
}

// blend draws c with alpha a (0-255) over r.
func blend(dst *image.RGBA, r image.Rectangle, c color.RGBA, a uint8) {
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	src := image.NewUniform(color.RGBA{
		uint8(uint32(c.R) * uint32(a) / 255), uint8(uint32(c.G) * uint32(a) / 255),
		uint8(uint32(c.B) * uint32(a) / 255), a,
	})
	draw.Draw(dst, r, src, image.Point{}, draw.Over)
}

// bar draws a horizontal meter: a track with frac (0..1) filled.
func bar(dst *image.RGBA, r image.Rectangle, frac float64, c color.RGBA) {
	fill(dst, r, colTrack)
	if frac != frac || frac <= 0 { // NaN or empty
		return
	}
	if frac > 1 {
		frac = 1
	}
	w := int(float64(r.Dx())*frac + 0.5)
	fill(dst, image.Rect(r.Min.X, r.Min.Y, r.Min.X+w, r.Max.Y), c)
}

// levelColor maps a 0..1 load to green, yellow or red.
func levelColor(frac float64) color.RGBA {
	switch {
	case frac >= 0.85:
		return colBad
	case frac >= 0.6:
		return colWarn
	}
	return colGood
}

// plot fills a t×t square centered on (x, y), clipped.
func plot(dst *image.RGBA, clip image.Rectangle, x, y, t int, c color.RGBA) {
	h := t / 2
	fill(dst, image.Rect(x-h, y-h, x-h+t, y-h+t).Intersect(clip), c)
}

// line draws a segment of thickness t, clipped to clip before iterating so
// segments far outside the screen (wall calibration lines) cost nothing.
func line(dst *image.RGBA, clip image.Rectangle, x0, y0, x1, y1 float64, t int, c color.RGBA) {
	pad := float64(t)
	cr := clip.Intersect(dst.Bounds())
	if cr.Empty() {
		return
	}
	xmin, ymin := float64(cr.Min.X)-pad, float64(cr.Min.Y)-pad
	xmax, ymax := float64(cr.Max.X)+pad, float64(cr.Max.Y)+pad
	var ok bool
	if x0, y0, x1, y1, ok = clipSegment(x0, y0, x1, y1, xmin, ymin, xmax, ymax); !ok {
		return
	}
	dx, dy := x1-x0, y1-y0
	n := int(math.Max(math.Abs(dx), math.Abs(dy)))
	if n == 0 {
		plot(dst, cr, int(math.Round(x0)), int(math.Round(y0)), t, c)
		return
	}
	sx, sy := dx/float64(n), dy/float64(n)
	x, y := x0, y0
	for i := 0; i <= n; i++ {
		plot(dst, cr, int(math.Round(x)), int(math.Round(y)), t, c)
		x += sx
		y += sy
	}
}

// clipSegment clips a segment to a rectangle (Liang-Barsky).
func clipSegment(x0, y0, x1, y1, xmin, ymin, xmax, ymax float64) (float64, float64, float64, float64, bool) {
	t0, t1 := 0.0, 1.0
	dx, dy := x1-x0, y1-y0
	for _, pq := range [4][2]float64{{-dx, x0 - xmin}, {dx, xmax - x0}, {-dy, y0 - ymin}, {dy, ymax - y0}} {
		p, q := pq[0], pq[1]
		if p == 0 {
			if q < 0 {
				return 0, 0, 0, 0, false
			}
			continue
		}
		r := q / p
		if p < 0 {
			if r > t1 {
				return 0, 0, 0, 0, false
			}
			if r > t0 {
				t0 = r
			}
		} else {
			if r < t0 {
				return 0, 0, 0, 0, false
			}
			if r < t1 {
				t1 = r
			}
		}
	}
	return x0 + t0*dx, y0 + t0*dy, x0 + t1*dx, y0 + t1*dy, true
}

// ellipse draws an outline of thickness t with center (cx, cy) and radii
// (rx, ry), visiting only rows and columns inside clip, so a circle much
// larger than the screen costs O(width + height).
func ellipse(dst *image.RGBA, clip image.Rectangle, cx, cy, rx, ry float64, t int, c color.RGBA) {
	cr := clip.Intersect(dst.Bounds())
	if cr.Empty() || rx <= 0 || ry <= 0 {
		return
	}
	// One point per row (flat parts) and one per column (steep parts)
	// together give a gap-free outline.
	for y := cr.Min.Y; y < cr.Max.Y; y++ {
		v := (float64(y) - cy) / ry
		if v < -1 || v > 1 {
			continue
		}
		dx := rx * math.Sqrt(1-v*v)
		plot(dst, cr, int(math.Round(cx-dx)), y, t, c)
		plot(dst, cr, int(math.Round(cx+dx)), y, t, c)
	}
	for x := cr.Min.X; x < cr.Max.X; x++ {
		u := (float64(x) - cx) / rx
		if u < -1 || u > 1 {
			continue
		}
		dy := ry * math.Sqrt(1-u*u)
		plot(dst, cr, x, int(math.Round(cy-dy)), t, c)
		plot(dst, cr, x, int(math.Round(cy+dy)), t, c)
	}
}

// drawCentered draws one line of text centered horizontally in r with its
// block vertically centered, shrinking the size so it fits r.
func drawCentered(dst *image.RGBA, r image.Rectangle, st fontStyle, maxSize int, s string, c color.RGBA) int {
	if r.Empty() || s == "" {
		return 0
	}
	size := fitLineSize(st, s, r.Dx(), r.Dy())
	if maxSize > 0 && size > maxSize {
		size = maxSize
	}
	m := metricsFor(st, size)
	w := textWidth(st, size, s)
	x := r.Min.X + (r.Dx()-w)/2
	y := r.Min.Y + (r.Dy()-(m.ascent+m.descent))/2 + m.ascent
	drawString(dst, r, st, size, x, y, s, c)
	return size
}

// drawLeft draws one line left-aligned in r (baseline chosen so the text
// is vertically centered), shrinking to at least 60% of size and then
// truncating with an ellipsis. It returns the width drawn.
func drawLeft(dst *image.RGBA, r image.Rectangle, st fontStyle, size int, s string, c color.RGBA) int {
	if r.Empty() || s == "" || size <= 0 {
		return 0
	}
	if w := textWidth(st, size, s); w > r.Dx() {
		if lo := size * 6 / 10; lo >= minFontPx {
			if fit := fitLineSize(st, s, r.Dx(), r.Dy()); fit < size {
				size = max(fit, lo)
			}
		}
		s = ellipsize(st, size, s, r.Dx())
	}
	m := metricsFor(st, size)
	y := r.Min.Y + (r.Dy()-(m.ascent+m.descent))/2 + m.ascent
	return drawString(dst, r, st, size, r.Min.X, y, s, c) - r.Min.X
}

// drawRight is drawLeft aligned to the right edge (no shrinking).
func drawRight(dst *image.RGBA, r image.Rectangle, st fontStyle, size int, s string, c color.RGBA) {
	if r.Empty() || s == "" {
		return
	}
	s = ellipsize(st, size, s, r.Dx())
	m := metricsFor(st, size)
	y := r.Min.Y + (r.Dy()-(m.ascent+m.descent))/2 + m.ascent
	drawString(dst, r, st, size, r.Max.X-textWidth(st, size, s), y, s, c)
}
