package display

import (
	"errors"
	"fmt"
	"sort"
)

// ttf is a minimal reader for TrueType ('glyf' outline) fonts, enough for
// the embedded Go fonts: metrics, character mapping and quadratic outlines
// including compound glyphs. Hinting, kerning and variations are not
// supported. It exists because x/image/font/sfnt pulls in golang.org/x/text,
// which is not a permitted dependency.
type ttf struct {
	upem        int32
	ascent      int32 // hhea, font units, positive up
	descent     int32 // hhea, font units, negative
	lineGap     int32
	numGlyphs   int
	numHMetrics int
	hmtx        []byte
	glyf        []byte
	loca        []uint32 // numGlyphs+1 offsets into glyf
	cmap        []byte   // selected subtable
	cmapFormat  int      // 4 or 12
}

var errBadFont = errors.New("malformed TrueType font")

func be16(b []byte, i int) uint16 { return uint16(b[i])<<8 | uint16(b[i+1]) }
func be32(b []byte, i int) uint32 {
	return uint32(b[i])<<24 | uint32(b[i+1])<<16 | uint32(b[i+2])<<8 | uint32(b[i+3])
}

// parseTTF parses the tables needed for rendering. Every offset is bounds
// checked, so a corrupt font yields an error rather than a panic.
func parseTTF(data []byte) (*ttf, error) {
	if len(data) < 12 {
		return nil, errBadFont
	}
	if v := be32(data, 0); v != 0x00010000 && v != 0x74727565 { // 1.0 or 'true'
		return nil, fmt.Errorf("%w: not a TrueType outline font", errBadFont)
	}
	n := int(be16(data, 4))
	if 12+16*n > len(data) {
		return nil, errBadFont
	}
	tables := map[string][]byte{}
	for i := 0; i < n; i++ {
		rec := 12 + 16*i
		off, ln := be32(data, rec+8), be32(data, rec+12)
		if uint64(off)+uint64(ln) > uint64(len(data)) {
			return nil, fmt.Errorf("%w: table %q out of range", errBadFont, data[rec:rec+4])
		}
		tables[string(data[rec:rec+4])] = data[off : off+ln]
	}
	head, maxp, hhea := tables["head"], tables["maxp"], tables["hhea"]
	if len(head) < 54 || len(maxp) < 6 || len(hhea) < 36 {
		return nil, fmt.Errorf("%w: missing head, maxp or hhea", errBadFont)
	}
	f := &ttf{
		upem:        int32(be16(head, 18)),
		numGlyphs:   int(be16(maxp, 4)),
		ascent:      int32(int16(be16(hhea, 4))),
		descent:     int32(int16(be16(hhea, 6))),
		lineGap:     int32(int16(be16(hhea, 8))),
		numHMetrics: int(be16(hhea, 34)),
		hmtx:        tables["hmtx"],
		glyf:        tables["glyf"],
	}
	if f.upem < 16 || f.upem > 16384 || f.numGlyphs == 0 || f.numHMetrics == 0 ||
		f.numHMetrics > f.numGlyphs || len(f.hmtx) < 4*f.numHMetrics {
		return nil, fmt.Errorf("%w: bad metrics", errBadFont)
	}
	loca := tables["loca"]
	f.loca = make([]uint32, f.numGlyphs+1)
	long := int16(be16(head, 50)) != 0
	for i := range f.loca {
		switch {
		case long && 4*i+4 <= len(loca):
			f.loca[i] = be32(loca, 4*i)
		case !long && 2*i+2 <= len(loca):
			f.loca[i] = 2 * uint32(be16(loca, 2*i))
		default:
			return nil, fmt.Errorf("%w: short loca table", errBadFont)
		}
		if f.loca[i] > uint32(len(f.glyf)) || (i > 0 && f.loca[i] < f.loca[i-1]) {
			return nil, fmt.Errorf("%w: bad loca entry %d", errBadFont, i)
		}
	}
	if err := f.pickCmap(tables["cmap"]); err != nil {
		return nil, err
	}
	return f, nil
}

// pickCmap selects the best Unicode subtable: format 12 (full Unicode)
// before format 4 (BMP).
func (f *ttf) pickCmap(cm []byte) error {
	if len(cm) < 4 {
		return fmt.Errorf("%w: missing cmap", errBadFont)
	}
	best := 0
	for i, n := 0, int(be16(cm, 2)); i < n; i++ {
		rec := 4 + 8*i
		if rec+8 > len(cm) {
			break
		}
		pid, eid, off := be16(cm, rec), be16(cm, rec+2), int(be32(cm, rec+4))
		unicode := pid == 0 || pid == 3 && (eid == 1 || eid == 10)
		if !unicode || off+4 > len(cm) {
			continue
		}
		sub := cm[off:]
		format, prio := int(be16(sub, 0)), 0
		switch format {
		case 4:
			if len(sub) >= 14 {
				if l := int(be16(sub, 2)); l <= len(sub) {
					sub, prio = sub[:l], 1
				}
			}
		case 12:
			if len(sub) >= 16 {
				if l := int64(be32(sub, 4)); l <= int64(len(sub)) {
					sub, prio = sub[:l], 2
				}
			}
		}
		if prio > best {
			best, f.cmap, f.cmapFormat = prio, sub, format
		}
	}
	if best == 0 {
		return fmt.Errorf("%w: no Unicode cmap", errBadFont)
	}
	return nil
}

// glyphIndex maps a rune to a glyph index; 0 (.notdef) when unmapped.
func (f *ttf) glyphIndex(r rune) uint16 {
	var g uint32
	switch f.cmapFormat {
	case 4:
		g = f.cmap4(r)
	case 12:
		g = f.cmap12(r)
	}
	if g >= uint32(f.numGlyphs) {
		return 0
	}
	return uint16(g)
}

func (f *ttf) cmap4(r rune) uint32 {
	if r < 0 || r > 0xffff {
		return 0
	}
	c := uint32(r)
	sub := f.cmap
	segX2 := int(be16(sub, 6))
	seg := segX2 / 2
	if 16+4*segX2 > len(sub) {
		return 0
	}
	ends, starts, deltas, ranges := 14, 16+segX2, 16+2*segX2, 16+3*segX2
	i := sort.Search(seg, func(i int) bool { return uint32(be16(sub, ends+2*i)) >= c })
	if i == seg {
		return 0
	}
	start := uint32(be16(sub, starts+2*i))
	if start > c {
		return 0
	}
	delta := uint32(be16(sub, deltas+2*i))
	ro := int(be16(sub, ranges+2*i))
	if ro == 0 {
		return (c + delta) & 0xffff
	}
	at := ranges + 2*i + ro + 2*int(c-start)
	if at+2 > len(sub) {
		return 0
	}
	g := uint32(be16(sub, at))
	if g == 0 {
		return 0
	}
	return (g + delta) & 0xffff
}

func (f *ttf) cmap12(r rune) uint32 {
	if r < 0 {
		return 0
	}
	c := uint32(r)
	sub := f.cmap
	n := int(be32(sub, 12))
	if n < 0 || 16+12*int64(n) > int64(len(sub)) {
		return 0
	}
	i := sort.Search(n, func(i int) bool { return be32(sub, 16+12*i+4) >= c })
	if i == n {
		return 0
	}
	g := 16 + 12*i
	if start := be32(sub, g); start <= c {
		return be32(sub, g+8) + (c - start)
	}
	return 0
}

// advance returns the horizontal advance of glyph gi in font units.
func (f *ttf) advance(gi uint16) int32 {
	i := int(gi)
	if i >= f.numHMetrics {
		i = f.numHMetrics - 1
	}
	return int32(be16(f.hmtx, 4*i))
}

// glyphBounds returns the glyph's bounding box in font units (y up).
func (f *ttf) glyphBounds(gi uint16) (xMin, yMin, xMax, yMax int32, ok bool) {
	d := f.glyphData(gi)
	if len(d) < 10 {
		return 0, 0, 0, 0, false
	}
	return int32(int16(be16(d, 2))), int32(int16(be16(d, 4))),
		int32(int16(be16(d, 6))), int32(int16(be16(d, 8))), true
}

func (f *ttf) glyphData(gi uint16) []byte {
	if int(gi) >= f.numGlyphs {
		return nil
	}
	return f.glyf[f.loca[gi]:f.loca[gi+1]]
}

// pathSink receives outline segments in font units (y up).
type pathSink interface {
	moveTo(x, y float32)
	lineTo(x, y float32)
	quadTo(bx, by, cx, cy float32)
}

// affine maps (x, y) to (a*x + c*y + e, b*x + d*y + f).
type affine struct{ a, b, c, d, e, f float32 }

var identity = affine{a: 1, d: 1}

func (t affine) apply(x, y float32) (float32, float32) {
	return t.a*x + t.c*y + t.e, t.b*x + t.d*y + t.f
}

// then returns the transform "apply u, then t".
func (t affine) then(u affine) affine {
	return affine{
		a: t.a*u.a + t.c*u.b, b: t.b*u.a + t.d*u.b,
		c: t.a*u.c + t.c*u.d, d: t.b*u.c + t.d*u.d,
		e: t.a*u.e + t.c*u.f + t.e, f: t.b*u.e + t.d*u.f + t.f,
	}
}

type ttPoint struct {
	x, y float32
	on   bool
}

const maxCompoundDepth = 8

// outline emits glyph gi's contours, transformed by t, into sink.
func (f *ttf) outline(gi uint16, t affine, sink pathSink, depth int) error {
	d := f.glyphData(gi)
	if len(d) == 0 {
		return nil // empty glyph (space)
	}
	if len(d) < 10 {
		return errBadFont
	}
	nc := int(int16(be16(d, 0)))
	switch {
	case nc < 0:
		if depth >= maxCompoundDepth {
			return fmt.Errorf("%w: compound glyph nesting too deep", errBadFont)
		}
		return f.compound(d[10:], t, sink, depth)
	case nc == 0:
		return nil
	}
	pts, ends, err := simplePoints(d, nc)
	if err != nil {
		return err
	}
	for i := range pts {
		pts[i].x, pts[i].y = t.apply(pts[i].x, pts[i].y)
	}
	start := 0
	for _, end := range ends {
		emitContour(pts[start:end+1], sink)
		start = end + 1
	}
	return nil
}

// simplePoints decodes a simple glyph's points and contour end indexes.
func simplePoints(d []byte, nc int) ([]ttPoint, []int, error) {
	i := 10
	if i+2*nc+2 > len(d) {
		return nil, nil, errBadFont
	}
	ends := make([]int, nc)
	prev := -1
	for c := 0; c < nc; c++ {
		e := int(be16(d, i+2*c))
		if e <= prev {
			return nil, nil, errBadFont
		}
		ends[c], prev = e, e
	}
	i += 2 * nc
	np := prev + 1
	i += 2 + int(be16(d, i)) // skip instructions
	if i > len(d) {
		return nil, nil, errBadFont
	}
	flags := make([]byte, 0, np)
	for len(flags) < np {
		if i >= len(d) {
			return nil, nil, errBadFont
		}
		fl := d[i]
		i++
		flags = append(flags, fl)
		if fl&8 != 0 { // repeat
			if i >= len(d) {
				return nil, nil, errBadFont
			}
			for r := int(d[i]); r > 0 && len(flags) < np; r-- {
				flags = append(flags, fl)
			}
			i++
		}
	}
	pts := make([]ttPoint, np)
	var err error
	if i, err = decodeCoords(d, i, flags, pts, true); err != nil {
		return nil, nil, err
	}
	if _, err = decodeCoords(d, i, flags, pts, false); err != nil {
		return nil, nil, err
	}
	for k, fl := range flags {
		pts[k].on = fl&1 != 0
	}
	return pts, ends, nil
}

func decodeCoords(d []byte, i int, flags []byte, pts []ttPoint, isX bool) (int, error) {
	short, same := byte(2), byte(16)
	if !isX {
		short, same = 4, 32
	}
	var v int32
	for k, fl := range flags {
		switch {
		case fl&short != 0:
			if i >= len(d) {
				return 0, errBadFont
			}
			if fl&same != 0 {
				v += int32(d[i])
			} else {
				v -= int32(d[i])
			}
			i++
		case fl&same == 0:
			if i+2 > len(d) {
				return 0, errBadFont
			}
			v += int32(int16(be16(d, i)))
			i += 2
		}
		if isX {
			pts[k].x = float32(v)
		} else {
			pts[k].y = float32(v)
		}
	}
	return i, nil
}

// emitContour converts TrueType on/off-curve points into move/line/quad
// segments; consecutive off-curve points imply an on-curve midpoint.
func emitContour(pts []ttPoint, sink pathSink) {
	n := len(pts)
	if n == 0 {
		return
	}
	mid := func(p, q ttPoint) ttPoint { return ttPoint{x: (p.x + q.x) / 2, y: (p.y + q.y) / 2, on: true} }
	var start ttPoint
	switch {
	case pts[0].on:
		start, pts = pts[0], pts[1:]
	case pts[n-1].on:
		start, pts = pts[n-1], pts[:n-1]
	default:
		start = mid(pts[0], pts[n-1])
	}
	sink.moveTo(start.x, start.y)
	var ctrl ttPoint
	have := false
	for _, p := range pts {
		switch {
		case p.on && have:
			sink.quadTo(ctrl.x, ctrl.y, p.x, p.y)
			have = false
		case p.on:
			sink.lineTo(p.x, p.y)
		default:
			if have {
				m := mid(ctrl, p)
				sink.quadTo(ctrl.x, ctrl.y, m.x, m.y)
			}
			ctrl, have = p, true
		}
	}
	if have {
		sink.quadTo(ctrl.x, ctrl.y, start.x, start.y)
	} else {
		sink.lineTo(start.x, start.y)
	}
}

// compound emits the components of a compound glyph. Point-matching
// placement (ARGS_ARE_XY_VALUES clear) is not supported and skipped.
func (f *ttf) compound(d []byte, t affine, sink pathSink, depth int) error {
	const (
		argWords  = 1
		argsXY    = 2
		haveScale = 8
		more      = 32
		xyScale   = 64
		twoByTwo  = 128
	)
	f2dot14 := func(i int) float32 { return float32(int16(be16(d, i))) / 16384 }
	for comps := 0; comps < 64; comps++ {
		if len(d) < 4 {
			return errBadFont
		}
		flags, gi := be16(d, 0), be16(d, 2)
		var dx, dy float32
		if flags&argWords != 0 {
			if len(d) < 8 {
				return errBadFont
			}
			dx, dy = float32(int16(be16(d, 4))), float32(int16(be16(d, 6)))
			d = d[8:]
		} else {
			if len(d) < 6 {
				return errBadFont
			}
			dx, dy = float32(int8(d[4])), float32(int8(d[5]))
			d = d[6:]
		}
		m := affine{a: 1, d: 1}
		switch {
		case flags&haveScale != 0:
			if len(d) < 2 {
				return errBadFont
			}
			m.a = f2dot14(0)
			m.d = m.a
			d = d[2:]
		case flags&xyScale != 0:
			if len(d) < 4 {
				return errBadFont
			}
			m.a, m.d = f2dot14(0), f2dot14(2)
			d = d[4:]
		case flags&twoByTwo != 0:
			if len(d) < 8 {
				return errBadFont
			}
			// File order: xscale, scale01, scale10, yscale;
			// x' = xscale*x + scale10*y, y' = scale01*x + yscale*y.
			m.a, m.b, m.c, m.d = f2dot14(0), f2dot14(2), f2dot14(4), f2dot14(6)
			d = d[8:]
		}
		if flags&argsXY != 0 {
			m.e, m.f = dx, dy
			if err := f.outline(gi, t.then(m), sink, depth+1); err != nil {
				return err
			}
		}
		if flags&more == 0 {
			return nil
		}
	}
	return fmt.Errorf("%w: too many components", errBadFont)
}
