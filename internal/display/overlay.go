package display

import (
	"image"
	"image/color"
)

// Identify overlay colors: black on signal yellow, then inverted.
var (
	identYellow = rgb(0xff, 0xd4, 0x00)
	identDark   = rgb(0x10, 0x10, 0x10)
)

// drawIdentify draws the identify overlay over whatever dst holds: a
// panel with the huge short code, the node name and the label (wall
// position), in yellow-on-black or black-on-yellow depending on phase.
func drawIdentify(dst *image.RGBA, code, name, label string, phase bool) {
	W, H := dst.Rect.Dx(), dst.Rect.Dy()
	bg, fg := identYellow, identDark
	if phase {
		bg, fg = fg, bg
	}
	inset := min(W, H) / 16
	panel := image.Rect(inset, inset, W-inset, H-inset)
	fill(dst, panel, bg)
	strokeRect(dst, panel, max(3, inset/4), fg)
	in := panel.Inset(inset / 2)
	h := in.Dy()
	code = cleanText(code, 8, false)
	name = cleanText(name, 64, false)
	label = cleanText(label, 64, false)
	rows := []struct {
		st   fontStyle
		frac int
		s    string
	}{{styleBold, 55, code}, {styleBold, 20, name}, {styleRegular, 14, label}}
	y := in.Min.Y
	for _, r := range rows {
		rh := h * r.frac / 100
		if r.s != "" {
			drawCentered(dst, image.Rect(in.Min.X, y, in.Max.X, y+rh), r.st, 0, r.s, fg)
		}
		y += rh + h*3/100
	}
}

// drawErrorScreen replaces the frame with an error message (a render
// failure or an unusable spec), so a broken display is obvious.
func drawErrorScreen(dst *image.RGBA, head, detail string) {
	W, H := dst.Rect.Dx(), dst.Rect.Dy()
	bg := rgb(0x5a, 0x10, 0x10)
	fill(dst, dst.Rect, bg)
	u := max(10, min(W/30, H/18))
	drawCentered(dst, image.Rect(u, H/4, W-u, H/4+u*3), styleBold, u*2, head, colWhite)
	y := H/4 + u*4
	for i, l := range wrapText(styleRegular, u, cleanText(detail, 600, false), W-2*u) {
		if i == 6 || y+u*3/2 > H {
			break
		}
		drawCentered(dst, image.Rect(u, y, W-u, y+u*3/2), styleRegular, u, l, color.RGBA{0xff, 0xd0, 0xd0, 0xff})
		y += u * 3 / 2
	}
}
