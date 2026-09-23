package display

import (
	"errors"
	"fmt"
	"image"
	"math"

	"github.com/platteration/ewastesavior/internal/proto"
)

// tileMap maps the wall canvas to this screen: the tile rectangle
// (X, Y, W, H) fills the whole screen.
func tileMap(t *proto.WallTile, w, h int) canvasMap {
	return canvasMap{
		ox: float64(t.X), oy: float64(t.Y),
		sx: float64(w) / float64(t.W), sy: float64(h) / float64(t.H),
	}
}

// wall renders this node's part of a video wall. Content is laid out on
// the canvas (in canvas units) and only this tile's rectangle is drawn, so
// the full canvas is never allocated.
func (s *scene) wall(spec proto.DisplaySpec) {
	t := spec.Wall
	if t == nil || t.Content == nil || t.W <= 0 || t.H <= 0 || t.CanvasW <= 0 || t.CanvasH <= 0 {
		s.res.err = errors.New("wall mode without a valid wall tile")
		return
	}
	c := *t.Content
	mp := tileMap(t, s.w, s.h)
	geo := fmt.Sprintf("%dx%d|%d,%d,%d,%d,%d,%d", s.w, s.h, t.CanvasW, t.CanvasH, t.X, t.Y, t.W, t.H)
	switch c.Mode {
	case proto.DisplayColor:
		bg := specColor(c.BG, colBlack)
		if !s.unchanged("wall-color|" + geo + fmt.Sprint(bg)) {
			fill(s.dst, s.dst.Rect, bg)
		}
	case proto.DisplayText:
		fg, bg := specColor(c.FG, colWhite), specColor(c.BG, colBlack)
		title := cleanText(c.Title, 256, false)
		body := cleanText(c.Text, proto.MaxTextLen, true)
		if s.unchanged(fmt.Sprintf("wall-text|%s|%v|%v|%q|%q", geo, fg, bg, title, body)) {
			return
		}
		fill(s.dst, s.dst.Rect, bg)
		drawTextLines(s.dst, layoutText(title, body, t.CanvasW, t.CanvasH), mp, fg)
	case proto.DisplayTest:
		if s.unchanged(fmt.Sprintf("wall-test|%s|%d,%d|%q", geo, t.Row, t.Col, t.Label)) {
			return
		}
		drawWallCalibration(s.dst, t, mp)
	case proto.DisplayImage, proto.DisplaySlideshow:
		bg := specColor(c.BG, colBlack)
		m, ok := s.currentMedia(c)
		if !ok {
			s.res.err = errors.New("wall content has no image")
			return
		}
		req := FetchRequest{CanvasW: t.CanvasW, CanvasH: t.CanvasH,
			Rect:   image.Rect(t.X, t.Y, t.X+t.W, t.Y+t.H),
			PixelW: s.w, PixelH: s.h, Fit: fitOf(c.Fit)}
		img, err := s.fetch(m, req)
		if s.unchanged(fmt.Sprintf("wall-media|%s|%v|%s|%p|%v", geo, bg, m.Key(), img, err)) {
			return
		}
		fill(s.dst, s.dst.Rect, bg)
		if img != nil {
			drawFetched(s.dst, s.dst.Rect, img, req)
		} else {
			mediaPlaceholder(s.dst, s.dst.Rect, m, err, bg)
		}
	default:
		s.res.err = fmt.Errorf("unsupported wall content mode %q", proto.Sanitize(c.Mode, 32, false))
	}
}

// drawWallCalibration draws the canvas-wide calibration pattern: a grid
// every 100 canvas units (bolder every 500), both canvas diagonals, circles
// centered on the canvas, the tile's edge, and the tile label.
func drawWallCalibration(dst *image.RGBA, t *proto.WallTile, mp canvasMap) {
	W, H := dst.Rect.Dx(), dst.Rect.Dy()
	fill(dst, dst.Rect, rgb(0x18, 0x18, 0x18))
	px := func(cx float64) float64 { return (cx - mp.ox) * mp.sx }
	py := func(cy float64) float64 { return (cy - mp.oy) * mp.sy }
	thin := max(1, min(W, H)/400)
	grid := rgb(0x60, 0x60, 0x60)
	if 100*mp.sx >= 4 {
		for gx := 0; gx <= t.CanvasW; gx += 100 {
			x := px(float64(gx))
			if x < -8 || x > float64(W)+8 {
				continue
			}
			w, c := thin, grid
			if gx%500 == 0 {
				w, c = thin*3, rgb(0xa0, 0xa0, 0xa0)
			}
			line(dst, dst.Rect, x, 0, x, float64(H), w, c)
		}
	}
	if 100*mp.sy >= 4 {
		for gy := 0; gy <= t.CanvasH; gy += 100 {
			y := py(float64(gy))
			if y < -8 || y > float64(H)+8 {
				continue
			}
			w, c := thin, grid
			if gy%500 == 0 {
				w, c = thin*3, rgb(0xa0, 0xa0, 0xa0)
			}
			line(dst, dst.Rect, 0, y, float64(W), y, w, c)
		}
	}
	cw, ch := float64(t.CanvasW), float64(t.CanvasH)
	diag := rgb(0xff, 0xd4, 0x00)
	line(dst, dst.Rect, px(0), py(0), px(cw), py(ch), thin*3, diag)
	line(dst, dst.Rect, px(cw), py(0), px(0), py(ch), thin*3, diag)
	r := math.Min(cw, ch) / 2
	for _, f := range []float64{1, 0.5} {
		ellipse(dst, dst.Rect, px(cw/2), py(ch/2), r*f*mp.sx, r*f*mp.sy, thin*3, rgb(0x00, 0xc8, 0xff))
	}
	strokeRect(dst, dst.Rect, max(2, thin*2), rgb(0xff, 0x40, 0x40))

	label := cleanText(t.Label, 64, false)
	if label == "" {
		label = fmt.Sprintf("R%d C%d", t.Row+1, t.Col+1)
	}
	box := image.Rect(W/8, H*3/10, W*7/8, H*7/10)
	blend(dst, box, colBlack, 0xc0)
	drawCentered(dst, image.Rect(box.Min.X, box.Min.Y, box.Max.X, box.Min.Y+box.Dy()*2/3), styleBold, 0, label, colWhite)
	info := fmt.Sprintf("row %d · col %d · %d,%d %d×%d mm", t.Row+1, t.Col+1, t.X, t.Y, t.W, t.H)
	drawCentered(dst, image.Rect(box.Min.X, box.Min.Y+box.Dy()*2/3, box.Max.X, box.Max.Y), styleRegular, 0, info, colMuted)
}
