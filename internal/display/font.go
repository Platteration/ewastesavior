package display

import (
	"container/list"
	"image"
	"image/color"
	"image/draw"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// fontStyle selects one of the embedded Go fonts.
type fontStyle uint8

const (
	styleRegular fontStyle = iota
	styleBold
	styleMono
	numStyles
)

var (
	fontOnce  [numStyles]sync.Once
	fontCache [numStyles]*ttf
	fontData  = [numStyles][]byte{goregular.TTF, gobold.TTF, gomono.TTF}
)

// fontFor returns the parsed font. The embedded fonts are known good; if
// parsing ever failed, text would silently not render, so fall back to the
// regular face and finally to nil (callers treat nil as "draw nothing").
func fontFor(st fontStyle) *ttf {
	if st >= numStyles {
		st = styleRegular
	}
	fontOnce[st].Do(func() {
		f, err := parseTTF(fontData[st])
		if err == nil {
			fontCache[st] = f
		}
	})
	if fontCache[st] == nil && st != styleRegular {
		return fontFor(styleRegular)
	}
	return fontCache[st]
}

const (
	// glyphCacheBudget bounds the total bytes of cached glyph masks.
	glyphCacheBudget = 4 << 20
	// maxCachedGlyphSide: bigger glyphs are rasterized directly into the
	// destination, clipped, in tiles of this size (never fully allocated).
	maxCachedGlyphSide = 512
	// minFontPx and maxFontPx bound the pixel sizes a Face accepts.
	minFontPx = 4
	maxFontPx = 1 << 16
)

type glyphKey struct {
	style fontStyle
	size  int32
	r     rune
}

// glyphMask is a rasterized glyph. The mask's top-left corner sits at
// off relative to the glyph origin (the dot, on the baseline).
type glyphMask struct {
	key  glyphKey
	mask *image.Alpha
	off  image.Point
}

// glyphLRU caches glyph masks by (face, size, rune) within a byte budget.
// The shared rasterizer is used under the same lock, which keeps its
// accumulation buffers bounded (maxCachedGlyphSide squared).
type glyphLRU struct {
	mu     sync.Mutex
	budget int
	used   int
	items  map[glyphKey]*list.Element
	order  list.List // front = most recently used
	rast   vector.Rasterizer
}

var glyphCache = &glyphLRU{budget: glyphCacheBudget, items: map[glyphKey]*list.Element{}}

func (c *glyphLRU) get(k glyphKey) *glyphMask {
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*glyphMask)
	}
	return nil
}

func (c *glyphLRU) put(g *glyphMask) {
	n := len(g.mask.Pix) + 64
	if n > c.budget/4 {
		return
	}
	for c.used+n > c.budget && c.order.Len() > 0 {
		e := c.order.Back()
		old := e.Value.(*glyphMask)
		c.order.Remove(e)
		delete(c.items, old.key)
		c.used -= len(old.mask.Pix) + 64
	}
	c.items[g.key] = c.order.PushFront(g)
	c.used += n
}

// cachedBytes reports the bytes held by the glyph cache (for tests).
func (c *glyphLRU) cachedBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// face is a font.Face for one embedded font at an integer pixel size.
// Glyphs are positioned on whole pixels (no subpixel variants), so every
// (style, size, rune) has exactly one cached mask.
type face struct {
	f     *ttf
	style fontStyle
	size  int32 // pixels per em
}

var _ font.Face = (*face)(nil)

// faces caches faces per (style, size); a few dozen sizes are in use at
// any time, and the map is reset if hive specs ever produce many more.
var faces = struct {
	sync.Mutex
	m map[glyphKey]*face
}{m: map[glyphKey]*face{}}

// newFace returns the face for style at size pixels per em (clamped).
func newFace(st fontStyle, size int) *face {
	size = min(max(size, minFontPx), maxFontPx)
	k := glyphKey{style: st, size: int32(size)}
	faces.Lock()
	defer faces.Unlock()
	if fc, ok := faces.m[k]; ok {
		return fc
	}
	if len(faces.m) >= 256 {
		clear(faces.m)
	}
	fc := &face{f: fontFor(st), style: st, size: int32(size)}
	faces.m[k] = fc
	return fc
}

// units converts font units to 26.6 pixels.
func (fc *face) units(v int32) fixed.Int26_6 {
	return fixed.Int26_6(int64(v) * int64(fc.size) * 64 / int64(fc.f.upem))
}

// pixelBounds returns the glyph's integer pixel box relative to the origin
// (y down).
func (fc *face) pixelBounds(gi uint16) (image.Rectangle, bool) {
	x0, y0, x1, y1, ok := fc.f.glyphBounds(gi)
	if !ok || x1 <= x0 || y1 <= y0 {
		return image.Rectangle{}, false
	}
	s, u := int64(fc.size), int64(fc.f.upem)
	return image.Rect(
		int(floorDiv(int64(x0)*s, u)), int(floorDiv(-int64(y1)*s, u)),
		int(ceilDiv(int64(x1)*s, u)), int(ceilDiv(-int64(y0)*s, u)),
	), true
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func ceilDiv(a, b int64) int64 { return -floorDiv(-a, b) }

// Close implements font.Face.
func (fc *face) Close() error { return nil }

// Kern implements font.Face (the Go fonts have no kerning table).
func (fc *face) Kern(r0, r1 rune) fixed.Int26_6 { return 0 }

// Metrics implements font.Face.
func (fc *face) Metrics() font.Metrics {
	if fc.f == nil {
		return font.Metrics{}
	}
	return font.Metrics{
		Height:  fc.units(fc.f.ascent - fc.f.descent + fc.f.lineGap),
		Ascent:  fc.units(fc.f.ascent),
		Descent: fc.units(-fc.f.descent),
	}
}

// GlyphAdvance implements font.Face.
func (fc *face) GlyphAdvance(r rune) (fixed.Int26_6, bool) {
	if fc.f == nil {
		return 0, false
	}
	gi := fc.f.glyphIndex(r)
	return fc.units(fc.f.advance(gi)), gi != 0
}

// GlyphBounds implements font.Face.
func (fc *face) GlyphBounds(r rune) (fixed.Rectangle26_6, fixed.Int26_6, bool) {
	if fc.f == nil {
		return fixed.Rectangle26_6{}, 0, false
	}
	gi := fc.f.glyphIndex(r)
	adv := fc.units(fc.f.advance(gi))
	pb, ok := fc.pixelBounds(gi)
	if !ok {
		return fixed.Rectangle26_6{}, adv, gi != 0
	}
	return fixed.R(pb.Min.X, pb.Min.Y, pb.Max.X, pb.Max.Y), adv, gi != 0
}

// Glyph implements font.Face. The returned mask is shared (cached) and
// must not be modified. Glyphs too big for the cache are rasterized into a
// fresh mask, so prefer drawString, which clips them instead.
func (fc *face) Glyph(dot fixed.Point26_6, r rune) (image.Rectangle, image.Image, image.Point, fixed.Int26_6, bool) {
	if fc.f == nil {
		return image.Rectangle{}, nil, image.Point{}, 0, false
	}
	gi := fc.f.glyphIndex(r)
	adv := fc.units(fc.f.advance(gi))
	g := fc.mask(r, gi, true)
	if g == nil {
		return image.Rectangle{}, nil, image.Point{}, adv, gi != 0
	}
	p := image.Pt(dot.X.Round(), dot.Y.Round()).Add(g.off)
	return g.mask.Bounds().Add(p), g.mask, image.Point{}, adv, gi != 0
}

// mask returns the glyph's mask from the cache, rasterizing it on a miss.
// Oversized glyphs return nil unless force is set (then a mask of at most
// 16 MP is built without caching).
func (fc *face) mask(r rune, gi uint16, force bool) *glyphMask {
	key := glyphKey{fc.style, fc.size, r}
	glyphCache.mu.Lock()
	defer glyphCache.mu.Unlock()
	if g := glyphCache.get(key); g != nil {
		return g
	}
	pb, ok := fc.pixelBounds(gi)
	if !ok {
		return nil
	}
	big := pb.Dx() > maxCachedGlyphSide || pb.Dy() > maxCachedGlyphSide
	if big && (!force || pb.Dx()*pb.Dy() > 16<<20) {
		return nil
	}
	m := image.NewAlpha(image.Rect(0, 0, pb.Dx(), pb.Dy()))
	rast := &glyphCache.rast
	if big {
		rast = &vector.Rasterizer{}
	}
	fc.rasterize(rast, gi, m, m.Bounds(), -pb.Min.X, -pb.Min.Y, image.Opaque)
	g := &glyphMask{key: key, mask: m, off: pb.Min}
	if !big {
		glyphCache.put(g)
	}
	return g
}

// rasterize fills glyph gi, with its origin at (ox, oy) in dst coordinates,
// into the rectangle r of dst (r must fit the rasterizer limits).
func (fc *face) rasterize(rast *vector.Rasterizer, gi uint16, dst draw.Image, r image.Rectangle, ox, oy int, src image.Image) {
	rast.Reset(r.Dx(), r.Dy()) // also resets DrawOp to Over
	if _, ok := dst.(*image.Alpha); ok {
		rast.DrawOp = draw.Src
	}
	s := float32(fc.size) / float32(fc.f.upem)
	sink := &rastSink{r: rast, s: s, ox: float32(ox - r.Min.X), oy: float32(oy - r.Min.Y)}
	if err := fc.f.outline(gi, identity, sink, 0); err != nil {
		return
	}
	rast.Draw(dst, r, src, image.Point{})
}

// rastSink scales font units to pixels (flipping y) for the rasterizer.
type rastSink struct {
	r      *vector.Rasterizer
	s      float32
	ox, oy float32
}

func (k *rastSink) moveTo(x, y float32) { k.r.MoveTo(k.ox+x*k.s, k.oy-y*k.s) }
func (k *rastSink) lineTo(x, y float32) { k.r.LineTo(k.ox+x*k.s, k.oy-y*k.s) }
func (k *rastSink) quadTo(bx, by, cx, cy float32) {
	k.r.QuadTo(k.ox+bx*k.s, k.oy-by*k.s, k.ox+cx*k.s, k.oy-cy*k.s)
}

// drawGlyph draws rune r with its origin at pixel (x, y) into dst,
// clipped to clip. Cacheable glyphs use the mask cache; huge glyphs (wall
// text, identify codes on big screens) are rasterized only where visible,
// in bounded tiles.
func (fc *face) drawGlyph(dst *image.RGBA, clip image.Rectangle, r rune, x, y int, src *image.Uniform) {
	if fc.f == nil {
		return
	}
	gi := fc.f.glyphIndex(r)
	pb, ok := fc.pixelBounds(gi)
	if !ok {
		return
	}
	clip = clip.Intersect(dst.Bounds())
	vis := pb.Add(image.Pt(x, y)).Intersect(clip)
	if vis.Empty() {
		return
	}
	if g := fc.mask(r, gi, false); g != nil {
		mr := g.mask.Bounds().Add(image.Pt(x, y).Add(g.off))
		draw.DrawMask(dst, vis, src, image.Point{}, g.mask, vis.Min.Sub(mr.Min), draw.Over)
		return
	}
	glyphCache.mu.Lock()
	defer glyphCache.mu.Unlock()
	rast := &glyphCache.rast
	for ty := vis.Min.Y; ty < vis.Max.Y; ty += maxCachedGlyphSide {
		for tx := vis.Min.X; tx < vis.Max.X; tx += maxCachedGlyphSide {
			t := image.Rect(tx, ty, tx+maxCachedGlyphSide, ty+maxCachedGlyphSide).Intersect(vis)
			fc.rasterize(rast, gi, dst, t, x, y, src)
		}
	}
}

// uniform returns an opaque uniform source for c.
func uniform(c color.RGBA) *image.Uniform { return image.NewUniform(c) }
