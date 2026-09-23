package display

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// textLine is one laid-out line in some coordinate space: pixels for a
// plain screen, canvas units (millimeters) for a wall.
type textLine struct {
	st          fontStyle
	size        int // em size in the space's units
	s           string
	x, baseline int
}

// layoutText lays out an optional title and a body on a cw×ch area: the
// title fits a band at the top, the body is word-wrapped, sized to fill
// about 80% of the rest and centered.
func layoutText(title, body string, cw, ch int) []textLine {
	var out []textLine
	top := 0
	if title != "" {
		band := ch * 16 / 100
		size := fitLineSize(styleBold, title, cw*90/100, band*80/100)
		m := metricsFor(styleBold, size)
		w := textWidth(styleBold, size, title)
		out = append(out, textLine{st: styleBold, size: size, s: title,
			x: (cw - w) / 2, baseline: (band-(m.ascent+m.descent))/2 + m.ascent + band/10})
		top = band
	}
	bodyH := ch - top
	if body == "" || bodyH <= 0 {
		return out
	}
	size, lines := fitWrapped(styleRegular, body, cw*80/100, bodyH*80/100, 1, bodyH)
	m := metricsFor(styleRegular, size)
	blockH := len(lines)*m.height - (m.height - m.ascent - m.descent)
	y := top + (bodyH-blockH)/2 + m.ascent
	for _, l := range lines {
		w := textWidth(styleRegular, size, l)
		out = append(out, textLine{st: styleRegular, size: size, s: l, x: (cw - w) / 2, baseline: y})
		y += m.height
	}
	return out
}

// canvasMap maps canvas coordinates to screen pixels: px = (cx-ox)*sx.
type canvasMap struct {
	ox, oy float64
	sx, sy float64
}

var identityMap = canvasMap{sx: 1, sy: 1}

// drawTextLines draws laid-out lines through a canvas mapping. Glyph
// positions are computed in canvas space so text continues seamlessly
// across neighboring wall tiles; lines and glyphs off the screen are
// skipped, and huge glyphs are rasterized only where visible.
func drawTextLines(dst *image.RGBA, lines []textLine, mp canvasMap, c color.RGBA) {
	clip := dst.Rect
	src := uniform(c)
	for _, l := range lines {
		f := fontFor(l.st)
		if f == nil {
			continue
		}
		ps := int(math.Round(float64(l.size) * mp.sy))
		if ps < 1 {
			continue
		}
		m := metricsFor(l.st, ps)
		py := int(math.Round((float64(l.baseline) - mp.oy) * mp.sy))
		if py+m.descent < clip.Min.Y || py-m.ascent > clip.Max.Y {
			continue
		}
		fc := newFace(l.st, ps)
		unit := float64(l.size) / float64(f.upem)
		cx := float64(l.x)
		for _, r := range l.s {
			adv := float64(f.advance(f.glyphIndex(r))) * unit
			px := (cx - mp.ox) * mp.sx
			if px > float64(clip.Max.X) {
				break
			}
			if px+adv*mp.sx*1.5 >= float64(clip.Min.X) {
				fc.drawGlyph(dst, clip, r, int(math.Round(px)), py, src)
			}
			cx += adv
		}
	}
}

// text renders spec.Text (and Title) auto-sized and centered.
func (s *scene) text(spec proto.DisplaySpec) {
	fg, bg := specColor(spec.FG, colWhite), specColor(spec.BG, colBlack)
	title := cleanText(spec.Title, 256, false)
	body := cleanText(spec.Text, proto.MaxTextLen, true)
	if s.unchanged(fmt.Sprintf("text|%dx%d|%v|%v|%q|%q", s.w, s.h, fg, bg, title, body)) {
		return
	}
	fill(s.dst, s.dst.Rect, bg)
	drawTextLines(s.dst, layoutText(title, body, s.w, s.h), identityMap, fg)
}

var zoneCache sync.Map // name -> *time.Location

// clockLocation resolves spec.Timezone, else the configured location,
// else UTC.
func clockLocation(name string, fallback *time.Location) *time.Location {
	if name != "" {
		if l, ok := zoneCache.Load(name); ok {
			return l.(*time.Location)
		}
		if l, err := time.LoadLocation(name); err == nil {
			zoneCache.Store(name, l)
			return l
		}
	}
	if fallback != nil {
		return fallback
	}
	return time.UTC
}

// showsSeconds reports whether a Go time layout changes within a minute.
func showsSeconds(layout string) bool {
	t0 := time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)
	f := t0.Format(layout)
	return f != t0.Add(time.Second).Format(layout) || f != t0.Add(250*time.Millisecond).Format(layout)
}

var sampleCache = struct {
	sync.Mutex
	m map[string]string // "style|layout|zone" -> widest sample
}{m: map[string]string{}}

// widestSample returns the widest rendering of layout over sample dates
// covering every month × weekday with two-digit days and both 12/24-hour
// forms, so a clock's font size does not jump when the text changes.
func widestSample(st fontStyle, layout string, loc *time.Location) string {
	key := fmt.Sprintf("%d|%s|%s", st, layout, loc)
	sampleCache.Lock()
	defer sampleCache.Unlock()
	if v, ok := sampleCache.m[key]; ok {
		return v
	}
	best, bw := "", int64(-1)
	for m := time.January; m <= time.December; m++ {
		first := time.Date(2026, m, 22, 0, 0, 0, 0, loc).Weekday()
		for wd := 0; wd < 7; wd++ {
			day := 22 + (wd-int(first)+7)%7
			for _, hr := range []int{10, 22} {
				s := cleanText(time.Date(2026, m, day, hr, 48, 58, 0, loc).Format(layout), 64, false)
				if w := textUnits(st, s); w > bw {
					best, bw = s, w
				}
			}
		}
	}
	if len(sampleCache.m) >= 32 { // specs come from the hive; stay bounded
		clear(sampleCache.m)
	}
	sampleCache.m[key] = best
	return best
}

const dateLayout = "Monday 2 January 2006"

// clock renders the time in clock_format (default 15:04) and the date.
// It redraws at each minute boundary, or each second when the format
// shows seconds.
func (s *scene) clock(spec proto.DisplaySpec) {
	fg, bg := specColor(spec.FG, colWhite), specColor(spec.BG, colBlack)
	layout := spec.ClockFormat
	if layout == "" {
		layout = "15:04"
	}
	loc := clockLocation(spec.Timezone, s.env.Location)
	t := s.now.In(loc)
	if showsSeconds(layout) {
		s.res.next = s.now.Truncate(time.Second).Add(time.Second)
	} else {
		s.res.next = s.now.Truncate(time.Minute).Add(time.Minute)
	}
	ts := cleanText(t.Format(layout), 64, false)
	ds := t.Format(dateLayout)
	if s.unchanged(fmt.Sprintf("clock|%dx%d|%v|%v|%s|%s|%s|%s", s.w, s.h, fg, bg, layout, loc, ts, ds)) {
		return
	}
	fill(s.dst, s.dst.Rect, bg)
	tsize := fitLineSize(styleBold, widestSample(styleBold, layout, loc), s.w*90/100, s.h*45/100)
	dsize := min(fitLineSize(styleRegular, widestSample(styleRegular, dateLayout, loc), s.w*80/100, s.h*10/100), tsize/2)
	tm, dm := metricsFor(styleBold, tsize), metricsFor(styleRegular, dsize)
	gap := dm.height / 2
	blockH := tm.ascent + tm.descent + gap + dm.ascent + dm.descent
	y := (s.h-blockH)/2 + tm.ascent
	drawString(s.dst, s.dst.Rect, styleBold, tsize, (s.w-textWidth(styleBold, tsize, ts))/2, y, ts, fg)
	y += tm.descent + gap + dm.ascent
	dc := mix(fg, bg, 3, 4)
	drawString(s.dst, s.dst.Rect, styleRegular, dsize, (s.w-textWidth(styleRegular, dsize, ds))/2, y, ds, dc)
}
