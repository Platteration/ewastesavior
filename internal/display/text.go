package display

import (
	"image"
	"image/color"
	"strings"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Text measurement works in font units (exact, integer) and scales
// linearly with size because glyphs are not hinted.

// textUnits returns the advance width of s in font units.
func textUnits(st fontStyle, s string) int64 {
	f := fontFor(st)
	if f == nil {
		return 0
	}
	var n int64
	for _, r := range s {
		n += int64(f.advance(f.glyphIndex(r)))
	}
	return n
}

// textWidth returns the pixel width of s at size (matching drawString up to
// per-glyph 1/64 px rounding).
func textWidth(st fontStyle, size int, s string) int {
	f := fontFor(st)
	if f == nil || size <= 0 {
		return 0
	}
	return int(textUnits(st, s) * int64(size) / int64(f.upem))
}

// vmetrics holds pixel line metrics for a style at a size.
type vmetrics struct {
	ascent  int // above the baseline
	descent int // below the baseline
	height  int // baseline-to-baseline distance
}

func metricsFor(st fontStyle, size int) vmetrics {
	f := fontFor(st)
	if f == nil || size <= 0 {
		return vmetrics{}
	}
	s, u := int64(size), int64(f.upem)
	m := vmetrics{
		ascent:  int(ceilDiv(int64(f.ascent)*s, u)),
		descent: int(ceilDiv(-int64(f.descent)*s, u)),
	}
	m.height = int(ceilDiv(int64(f.ascent-f.descent+f.lineGap)*s, u))
	if m.height < m.ascent+m.descent {
		m.height = m.ascent + m.descent
	}
	return m
}

// bigTextPx is the size above which drawString uses the clipped tiled
// rasterizer instead of font.Drawer (whose Glyph must return whole masks).
const bigTextPx = 256

// drawString draws s with its baseline origin at (x, y), clipped to clip,
// and returns the x position after the last glyph.
func drawString(dst *image.RGBA, clip image.Rectangle, st fontStyle, size, x, y int, s string, c color.RGBA) int {
	if s == "" || size <= 0 {
		return x
	}
	fc := newFace(st, size)
	if fc.f == nil {
		return x
	}
	src := uniform(c)
	clip = clip.Intersect(dst.Bounds())
	if size <= bigTextPx {
		sub, ok := dst.SubImage(clip).(*image.RGBA)
		if !ok || clip.Empty() {
			return x + textWidth(st, size, s)
		}
		d := font.Drawer{Dst: sub, Src: src, Face: fc, Dot: fixed.P(x, y)}
		d.DrawString(s)
		return d.Dot.X.Round()
	}
	dot := fixed.I(x)
	for _, r := range s {
		fc.drawGlyph(dst, clip, r, dot.Round(), y, src)
		dot += fc.units(fc.f.advance(fc.f.glyphIndex(r)))
	}
	return dot.Round()
}

// fitLineSize returns the largest size (>= 1) at which s fits in maxW
// pixels and the line's ascent+descent fits in maxH.
func fitLineSize(st fontStyle, s string, maxW, maxH int) int {
	f := fontFor(st)
	if f == nil || maxW <= 0 || maxH <= 0 {
		return 1
	}
	u := int64(f.upem)
	size := int64(maxH) * u / int64(f.ascent-f.descent)
	if w := textUnits(st, s); w > 0 {
		if sw := int64(maxW) * u / w; sw < size {
			size = sw
		}
	}
	if size < 1 {
		size = 1
	}
	if size > maxFontPx {
		size = maxFontPx
	}
	return int(size)
}

// ellipsize shortens s with a trailing "…" until it fits maxW at size.
func ellipsize(st fontStyle, size int, s string, maxW int) string {
	if textWidth(st, size, s) <= maxW {
		return s
	}
	rs := []rune(s)
	lo, hi := 0, len(rs)
	for lo < hi { // largest n with rs[:n]+"…" fitting
		mid := (lo + hi + 1) / 2
		if textWidth(st, size, string(rs[:mid])+"…") <= maxW {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return ""
	}
	return strings.TrimRight(string(rs[:lo]), " ") + "…"
}

// wrapUnits breaks s into lines of at most maxUnits font units. Explicit
// newlines are kept, runs of spaces collapse, and words longer than a line
// are split between characters.
func wrapUnits(st fontStyle, s string, maxUnits int64) []string {
	f := fontFor(st)
	if f == nil {
		return nil
	}
	space := int64(f.advance(f.glyphIndex(' ')))
	var out []string
	for _, para := range strings.Split(s, "\n") {
		var line strings.Builder
		var lw int64
		flush := func() {
			out = append(out, line.String())
			line.Reset()
			lw = 0
		}
		for _, w := range strings.Fields(para) {
			ww := textUnits(st, w)
			if line.Len() > 0 && lw+space+ww <= maxUnits {
				line.WriteByte(' ')
				line.WriteString(w)
				lw += space + ww
				continue
			}
			if line.Len() > 0 {
				flush()
			}
			for ww > maxUnits && utf8.RuneCountInString(w) > 1 {
				n, acc := 0, int64(0)
				for i, r := range w {
					a := int64(f.advance(f.glyphIndex(r)))
					if acc+a > maxUnits && i > 0 {
						break
					}
					acc += a
					n = i + utf8.RuneLen(r)
				}
				out = append(out, w[:n])
				w = w[n:]
				ww = textUnits(st, w)
			}
			line.WriteString(w)
			lw = ww
		}
		flush()
	}
	return out
}

// wrapText wraps s for pixel width maxW at size.
func wrapText(st fontStyle, size int, s string, maxW int) []string {
	f := fontFor(st)
	if f == nil || size <= 0 {
		return nil
	}
	return wrapUnits(st, s, int64(maxW)*int64(f.upem)/int64(size))
}

// fitWrapped finds the largest size in [minSize, maxSize] at which s,
// wrapped to maxW, fits maxH. Sizes are integers in the caller's unit
// (pixels, or canvas units for walls).
func fitWrapped(st fontStyle, s string, maxW, maxH, minSize, maxSize int) (int, []string) {
	if minSize < 1 {
		minSize = 1
	}
	if maxSize < minSize {
		maxSize = minSize
	}
	fits := func(size int) ([]string, bool) {
		lines := wrapText(st, size, s, maxW)
		m := metricsFor(st, size)
		h := len(lines)*m.height - (m.height - m.ascent - m.descent)
		if h > maxH {
			return lines, false
		}
		for _, l := range lines {
			if textWidth(st, size, l) > maxW {
				return lines, false
			}
		}
		return lines, true
	}
	lo, hi := minSize, maxSize
	best, _ := fits(minSize)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if lines, ok := fits(mid); ok {
			lo, best = mid, lines
		} else {
			hi = mid - 1
		}
	}
	return lo, best
}

// cleanText removes control characters (keeping newlines when asked) and
// normalizes CRLF, so nothing unprintable reaches the rasterizer.
func cleanText(s string, max int, keepNewlines bool) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	return proto.Sanitize(s, max, keepNewlines)
}
