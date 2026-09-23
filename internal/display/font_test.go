package display

import (
	"image"
	"image/color"
	"image/draw"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/math/fixed"
)

func TestEmbeddedFontsParse(t *testing.T) {
	for st := fontStyle(0); st < numStyles; st++ {
		f := fontFor(st)
		if f == nil {
			t.Fatalf("style %d did not parse", st)
		}
		if f.upem != 2048 || f.ascent <= 0 || f.descent >= 0 || f.numGlyphs < 500 {
			t.Errorf("style %d: upem %d ascent %d descent %d glyphs %d", st, f.upem, f.ascent, f.descent, f.numGlyphs)
		}
		for _, r := range "Aa0Zß€ÄЖΩ→" {
			if f.glyphIndex(r) == 0 {
				t.Errorf("style %d: no glyph for %q", st, r)
			}
		}
		if f.glyphIndex(0x1F600) != 0 || f.glyphIndex(-5) != 0 {
			t.Errorf("style %d: unmapped runes should give .notdef", st)
		}
	}
	mono := fontFor(styleMono)
	w := mono.advance(mono.glyphIndex('i'))
	for _, r := range "MW.0@" {
		if a := mono.advance(mono.glyphIndex(r)); a != w {
			t.Errorf("mono advance of %q = %d, want %d", r, a, w)
		}
	}
	// Digits are tabular in the bold face too, so clocks do not jiggle.
	bold := fontFor(styleBold)
	d0 := bold.advance(bold.glyphIndex('0'))
	for _, r := range "123456789" {
		if bold.advance(bold.glyphIndex(r)) != d0 {
			t.Errorf("bold digit %q is not tabular", r)
		}
	}
}

// pathCounter counts outline operations.
type pathCounter struct{ moves, lines, quads int }

func (p *pathCounter) moveTo(x, y float32)           { p.moves++ }
func (p *pathCounter) lineTo(x, y float32)           { p.lines++ }
func (p *pathCounter) quadTo(bx, by, cx, cy float32) { p.quads++ }

func TestOutlines(t *testing.T) {
	f := fontFor(styleRegular)
	var o pathCounter
	if err := f.outline(f.glyphIndex('O'), identity, &o, 0); err != nil {
		t.Fatal(err)
	}
	if o.moves != 2 || o.quads == 0 {
		t.Errorf("'O' outline: %+v, want 2 contours with curves", o)
	}
	var sp pathCounter
	if err := f.outline(f.glyphIndex(' '), identity, &sp, 0); err != nil || sp.moves != 0 {
		t.Errorf("space outline: %+v %v", sp, err)
	}
	// Ä is a compound glyph (A + dieresis): more contours than A.
	var a, ae pathCounter
	if err := f.outline(f.glyphIndex('A'), identity, &a, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.outline(f.glyphIndex('Ä'), identity, &ae, 0); err != nil {
		t.Fatal(err)
	}
	if ae.moves <= a.moves {
		t.Errorf("Ä has %d contours, A has %d", ae.moves, a.moves)
	}
}

func TestParseTTFRejectsGarbage(t *testing.T) {
	data := goregular.TTF
	for _, n := range []int{0, 5, 11, 12, 100, 300, 1000, len(data) / 2} {
		if _, err := parseTTF(data[:n]); err == nil {
			t.Errorf("truncated to %d bytes: expected error", n)
		}
	}
	bad := append([]byte(nil), data...)
	copy(bad, "OTTO")
	if _, err := parseTTF(bad); err == nil {
		t.Error("CFF font accepted")
	}
}

// FuzzParseTTF mutates the font and requires parsing and outlining never
// to panic.
func FuzzParseTTF(f *testing.F) {
	f.Add(0, byte(0))
	f.Add(4, byte(0xff))
	f.Add(1000, byte(0x80))
	f.Fuzz(func(t *testing.T, at int, v byte) {
		data := append([]byte(nil), goregular.TTF...)
		if at < 0 {
			at = -at
		}
		for i := at % len(data); i < len(data); i += 997 {
			data[i] ^= v
		}
		ft, err := parseTTF(data)
		if err != nil {
			return
		}
		var pc pathCounter
		for _, r := range "AÄgQ@" {
			_ = ft.outline(ft.glyphIndex(r), identity, &pc, 0)
			ft.advance(ft.glyphIndex(r))
		}
	})
}

func inkBounds(img *image.RGBA, bg color.RGBA) image.Rectangle {
	var r image.Rectangle
	for y := img.Rect.Min.Y; y < img.Rect.Max.Y; y++ {
		for x := img.Rect.Min.X; x < img.Rect.Max.X; x++ {
			if img.RGBAAt(x, y) != bg {
				r = r.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return r
}

func TestDrawStringPlacesInk(t *testing.T) {
	for _, size := range []int{12, 40, 300, 700} { // font.Drawer and tiled paths
		img := image.NewRGBA(image.Rect(0, 0, 2000, 1000))
		fill(img, img.Rect, colBlack)
		x, base := 10, 800
		end := drawString(img, img.Rect, styleBold, size, x, base, "HH", colWhite)
		ink := inkBounds(img, colBlack)
		m := metricsFor(styleBold, size)
		if ink.Empty() {
			t.Fatalf("size %d: nothing drawn", size)
		}
		if ink.Max.Y > base+1 || ink.Min.Y < base-m.ascent-1 {
			t.Errorf("size %d: ink %v outside ascent band [%d, %d]", size, ink, base-m.ascent, base)
		}
		if w := textWidth(styleBold, size, "HH"); absInt(end-x-w) > 2 {
			t.Errorf("size %d: end %d vs measured width %d", size, end-x, w)
		}
		if ink.Min.X < x || ink.Max.X > end+1 {
			t.Errorf("size %d: ink %v outside [%d, %d]", size, ink, x, end)
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestHugeGlyphClippedWithoutFullMask(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 200, 100))
	fill(img, img.Rect, colBlack)
	before := glyphCache.cachedBytes()
	// A 6000 px glyph whose stem crosses the small screen.
	fc := newFace(styleBold, 6000)
	pb, ok := fc.pixelBounds(fc.f.glyphIndex('I'))
	if !ok || pb.Dx() <= maxCachedGlyphSide {
		t.Fatalf("glyph bounds %v", pb)
	}
	x := 100 - (pb.Min.X+pb.Max.X)/2
	drawString(img, img.Rect, styleBold, 6000, x, 3000, "I", colWhite)
	if c := img.RGBAAt(100, 50); c != colWhite {
		t.Fatalf("center of the stem is %v, want white", c)
	}
	if inkBounds(img, colBlack).Empty() {
		t.Fatal("huge glyph left no ink on screen")
	}
	if after := glyphCache.cachedBytes(); after > before+64 {
		t.Fatalf("huge glyph was cached (%d -> %d bytes)", before, after)
	}
}

func TestGlyphCacheBounded(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 400, 400))
	for size := 20; size < 400; size += 7 {
		drawString(img, img.Rect, styleRegular, size, 0, 300, "WM@&", colWhite)
	}
	if b := glyphCache.cachedBytes(); b > glyphCacheBudget {
		t.Fatalf("glyph cache holds %d bytes, budget %d", b, glyphCacheBudget)
	}
}

func TestFaceWorksWithFontDrawer(t *testing.T) {
	a := image.NewRGBA(image.Rect(0, 0, 300, 60))
	b := image.NewRGBA(image.Rect(0, 0, 300, 60))
	fill(a, a.Rect, colBlack)
	fill(b, b.Rect, colBlack)
	fc := newFace(styleRegular, 24)
	d := font.Drawer{Dst: a, Src: image.NewUniform(colWhite), Face: fc, Dot: fixed.P(5, 40)}
	d.DrawString("Savior ÄÖÜ")
	drawString(b, b.Rect, styleRegular, 24, 5, 40, "Savior ÄÖÜ", colWhite)
	if string(a.Pix) != string(b.Pix) {
		t.Fatal("font.Drawer and drawString disagree")
	}
	m := fc.Metrics()
	if m.Ascent.Round() != metricsFor(styleRegular, 24).ascent && m.Ascent.Ceil() != metricsFor(styleRegular, 24).ascent {
		t.Errorf("metrics ascent %v vs %d", m.Ascent, metricsFor(styleRegular, 24).ascent)
	}
	if _, adv, ok := fc.GlyphBounds('A'); !ok || adv <= 0 {
		t.Error("GlyphBounds('A') failed")
	}
	if _, ok := fc.GlyphAdvance(0x1F600); ok {
		t.Error("GlyphAdvance should report missing glyphs")
	}
	// Clipping: drawing into a clip rectangle leaves the rest untouched.
	c := image.NewRGBA(image.Rect(0, 0, 300, 60))
	fill(c, c.Rect, colBlack)
	drawString(c, image.Rect(0, 0, 40, 60), styleRegular, 24, 5, 40, "Savior", colWhite)
	if ink := inkBounds(c, colBlack); ink.Max.X > 40 {
		t.Fatalf("ink %v escaped the clip", ink)
	}
}

func TestWrapAndFit(t *testing.T) {
	lines := wrapText(styleRegular, 20, "the quick brown fox jumps over the lazy dog", 120)
	if len(lines) < 3 {
		t.Fatalf("wrapped into %d lines: %q", len(lines), lines)
	}
	for _, l := range lines {
		if textWidth(styleRegular, 20, l) > 120 {
			t.Errorf("line %q is %d px wide", l, textWidth(styleRegular, 20, l))
		}
		if strings.HasPrefix(l, " ") || strings.HasSuffix(l, " ") {
			t.Errorf("line %q has edge spaces", l)
		}
	}
	// Explicit newlines and empty paragraphs are kept.
	if got := wrapText(styleRegular, 20, "a\n\nb", 500); len(got) != 3 || got[1] != "" {
		t.Errorf("newlines: %q", got)
	}
	// A word longer than a line is split, never dropped.
	long := strings.Repeat("W", 50)
	got := wrapText(styleRegular, 20, long, 100)
	if strings.Join(got, "") != long || len(got) < 5 {
		t.Errorf("long word split into %q", got)
	}
	// fitWrapped: larger boxes never get smaller text.
	prev := 0
	for _, h := range []int{50, 100, 200, 400} {
		size, lines := fitWrapped(styleRegular, "Old computers get a second life here.", 300, h, 1, h)
		m := metricsFor(styleRegular, size)
		if len(lines)*m.height-(m.height-m.ascent-m.descent) > h {
			t.Errorf("h=%d: %d lines of %d px overflow", h, len(lines), m.height)
		}
		if size < prev {
			t.Errorf("h=%d: size %d smaller than %d", h, size, prev)
		}
		prev = size
	}
	if s := ellipsize(styleRegular, 20, "a fairly long line of text", 80); !strings.HasSuffix(s, "…") || textWidth(styleRegular, 20, s) > 80 {
		t.Errorf("ellipsize = %q", s)
	}
	if s := ellipsize(styleRegular, 20, "short", 500); s != "short" {
		t.Errorf("ellipsize changed a fitting string: %q", s)
	}
	if got := cleanText("a\x00b\r\nc\td\x1b[31m", 100, true); got != "ab\nc    d[31m" {
		t.Errorf("cleanText = %q", got)
	}
}

func TestDrawPrimitivesClip(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 50, 40))
	fill(img, image.Rect(-10, -10, 1000, 5), colWhite) // clipped, no panic
	if img.RGBAAt(49, 4) != colWhite || img.RGBAAt(0, 5) == colWhite {
		t.Fatal("fill clipping wrong")
	}
	// Lines and ellipses far outside the image are cheap and harmless.
	line(img, img.Rect, -1e7, -1e7, 1e7, 1e7, 3, colWhite)
	if img.RGBAAt(20, 20) != colWhite {
		t.Fatal("diagonal through the image not drawn")
	}
	img2 := image.NewRGBA(image.Rect(0, 0, 50, 40))
	line(img2, img2.Rect, -100, -100, -50, 500, 3, colWhite)
	if !inkBounds(img2, color.RGBA{}).Empty() {
		t.Fatal("line outside the image left ink")
	}
	ellipse(img2, img2.Rect, 25, 20, 1e6, 1e6, 2, colWhite) // huge circle, edge far away
	if !inkBounds(img2, color.RGBA{}).Empty() {
		t.Fatal("huge circle drew inside")
	}
	ellipse(img2, img2.Rect, 25, 20, 10, 10, 1, colWhite)
	if img2.RGBAAt(35, 20) != colWhite || img2.RGBAAt(25, 10) != colWhite || img2.RGBAAt(25, 20) == colWhite {
		t.Fatal("circle outline wrong")
	}
	if _, _, _, _, ok := clipSegment(0, 0, 10, 0, 20, -1, 30, 1); ok {
		t.Fatal("segment left of the box should be rejected")
	}
	if x0, _, x1, _, ok := clipSegment(-10, 0, 40, 0, 0, -1, 30, 1); !ok || x0 != 0 || x1 != 30 {
		t.Fatalf("clipped to %v..%v %v", x0, x1, ok)
	}
	bar(img, image.Rect(0, 30, 50, 35), 2.5, colGood) // over-full clamps
	bar(img, image.Rect(0, 30, 50, 35), -1, colGood)
	var nan float64
	bar(img, image.Rect(0, 30, 50, 35), nan/nan, colGood)
	blend(img, image.Rect(0, 0, 10, 10), colBlack, 128)
	draw.Draw(img, img.Rect, image.Transparent, image.Point{}, draw.Src)
}
