package display

import (
	"fmt"
	"image"
	"image/color"

	"github.com/platteration/ewastesavior/internal/proto"
)

// testBars are the full-intensity color bars of the test pattern.
var testBars = []color.RGBA{
	rgb(0xff, 0xff, 0xff), rgb(0xff, 0xff, 0x00), rgb(0x00, 0xff, 0xff), rgb(0x00, 0xff, 0x00),
	rgb(0xff, 0x00, 0xff), rgb(0xff, 0x00, 0x00), rgb(0x00, 0x00, 0xff), rgb(0x00, 0x00, 0x00),
}

// testPattern renders color bars, gradients, a 1-pixel border and the
// resolution and pixel format.
func (s *scene) testPattern() {
	var info string
	if s.env.device != nil {
		d := s.env.device()
		info = describeDevice(d)
	}
	if s.unchanged(fmt.Sprintf("test|%dx%d|%s", s.w, s.h, info)) {
		return
	}
	drawTestPattern(s.dst, info)
}

// describeDevice formats "XRGB8888 · i915drmfb · rotated 90°".
func describeDevice(d proto.DisplayState) string {
	s := d.Format
	if d.Driver != "" {
		s += " · " + cleanText(d.Driver, 40, false)
	}
	if d.Rotate != 0 {
		s += fmt.Sprintf(" · rotated %d°", d.Rotate)
	}
	return s
}

func drawTestPattern(dst *image.RGBA, info string) {
	W, H := dst.Rect.Dx(), dst.Rect.Dy()
	fill(dst, dst.Rect, colBlack)
	barsH := H * 55 / 100
	for i, c := range testBars {
		x0, x1 := i*W/len(testBars), (i+1)*W/len(testBars)
		fill(dst, image.Rect(x0, 0, x1, barsH), c)
	}
	ramps := []func(v uint8) color.RGBA{
		func(v uint8) color.RGBA { return rgb(v, v, v) },
		func(v uint8) color.RGBA { return rgb(v, 0, 0) },
		func(v uint8) color.RGBA { return rgb(0, v, 0) },
		func(v uint8) color.RGBA { return rgb(0, 0, v) },
	}
	gTop, gH := barsH, H*28/100
	for i, ramp := range ramps {
		y0, y1 := gTop+i*gH/len(ramps), gTop+(i+1)*gH/len(ramps)
		for x := 0; x < W; x++ {
			v := uint8(x * 255 / max(1, W-1))
			fill(dst, image.Rect(x, y0, x+1, y1), ramp(v))
		}
	}
	text := fmt.Sprintf("%d×%d", W, H)
	if info != "" {
		text += "  " + info
	}
	txt := image.Rect(W/20, gTop+gH, W-W/20, H-2)
	drawCentered(dst, txt, styleBold, 0, text, colWhite)
	strokeRect(dst, dst.Rect, 1, colWhite)
}

// solidTestFrames are the full-screen colors `savior display test` cycles
// through for dead-pixel checks.
var solidTestFrames = []color.RGBA{
	rgb(0xff, 0, 0), rgb(0, 0xff, 0), rgb(0, 0, 0xff), rgb(0xff, 0xff, 0xff), rgb(0, 0, 0),
}
