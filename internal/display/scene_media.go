package display

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"

	"github.com/platteration/ewastesavior/internal/imaging"
	"github.com/platteration/ewastesavior/internal/proto"
)

// fitOf returns the effective fit mode.
func fitOf(f string) string {
	switch f {
	case "cover", "stretch":
		return f
	}
	return "contain"
}

// currentMedia picks the media item a spec shows at the scene's time and
// the time it changes (slideshows).
func (s *scene) currentMedia(spec proto.DisplaySpec) (m proto.Media, ok bool) {
	switch spec.Mode {
	case proto.DisplayImage:
		if spec.Image == nil {
			return m, false
		}
		return *spec.Image, true
	case proto.DisplaySlideshow:
		if len(spec.Images) == 0 {
			return m, false
		}
		idx, next := slideIndex(s.now, spec.IntervalS, len(spec.Images))
		s.res.next = next
		return spec.Images[idx], true
	}
	return m, false
}

// media renders image and slideshow modes: the image fitted to the screen
// over the background color.
func (s *scene) media(spec proto.DisplaySpec) {
	bg := specColor(spec.BG, colBlack)
	m, ok := s.currentMedia(spec)
	if !ok {
		s.res.mediaErrs = append(s.res.mediaErrs, &MediaError{Media: spec.Mode, Err: errors.New("no image in spec")})
		if !s.unchanged(fmt.Sprintf("media-none|%dx%d|%v", s.w, s.h, bg)) {
			fill(s.dst, s.dst.Rect, bg)
			placeholder(s.dst, s.dst.Rect, "No image configured", "", bg)
		}
		return
	}
	full := image.Rect(0, 0, s.w, s.h)
	req := FetchRequest{CanvasW: s.w, CanvasH: s.h, Rect: full, PixelW: s.w, PixelH: s.h, Fit: fitOf(spec.Fit)}
	img, err := s.fetch(m, req)
	if s.unchanged(fmt.Sprintf("media|%dx%d|%v|%s|%s|%p|%v", s.w, s.h, bg, m.Key(), req.key(), img, err)) {
		return
	}
	fill(s.dst, s.dst.Rect, bg)
	if img != nil {
		drawFetched(s.dst, full, img, req)
		return
	}
	mediaPlaceholder(s.dst, full, m, err, bg)
}

// drawFetched composites a fetched image over dst's rectangle r. A fetcher
// that returned the wrong size is corrected by scaling (integer only).
func drawFetched(dst *image.RGBA, r image.Rectangle, img image.Image, req FetchRequest) {
	b := img.Bounds()
	if b.Dx() != r.Dx() || b.Dy() != r.Dy() {
		img = imaging.RenderRegion(img, r.Dx(), r.Dy(), "stretch", image.Rect(0, 0, r.Dx(), r.Dy()), r.Dx(), r.Dy(), color.RGBA{})
		b = img.Bounds()
	}
	draw.Draw(dst, r, img, b.Min, draw.Over)
}

// mediaPlaceholder explains a missing image: loading, or the error.
func mediaPlaceholder(dst *image.RGBA, r image.Rectangle, m proto.Media, err error, bg color.RGBA) {
	if errors.Is(err, errPending) {
		placeholder(dst, r, "Loading image…", "", bg)
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
		var me *MediaError
		if errors.As(err, &me) {
			msg = me.Media + ": " + me.Err.Error()
		}
	}
	placeholder(dst, r, "Image unavailable", msg, bg)
}

// placeholder draws a short headline and an optional detail line in the
// lower third of r, in a color that contrasts with bg.
func placeholder(dst *image.RGBA, r image.Rectangle, head, detail string, bg color.RGBA) {
	fg := colText
	if int(bg.R)+int(bg.G)+int(bg.B) > 3*160 {
		fg = colBlack
	}
	u := max(10, min(r.Dx()/30, r.Dy()/18))
	y := r.Min.Y + r.Dy()*2/3
	drawCentered(dst, image.Rect(r.Min.X+u, y, r.Max.X-u, y+u*2), styleBold, u*3/2, head, fg)
	if detail != "" {
		detail = cleanText(detail, 300, false)
		for i, l := range wrapText(styleRegular, u, detail, r.Dx()-2*u) {
			if i == 3 {
				break
			}
			yy := y + u*2 + i*u*5/4
			drawCentered(dst, image.Rect(r.Min.X+u, yy, r.Max.X-u, yy+u*5/4), styleRegular, u, l, mix(fg, bg, 3, 4))
		}
	}
}
