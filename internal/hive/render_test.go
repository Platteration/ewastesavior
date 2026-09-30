package hive

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"testing"
)

// Without bg the render endpoint leaves uncovered canvas transparent, so a
// node compositing the frame over its spec's bg gets that colour in the
// letterbox, as with URL media (DISPLAY-HW-1). An explicit bg is opaque.
func TestRenderLetterboxDefaultsToTransparent(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	src := image.NewRGBA(image.Rect(0, 0, 400, 100))
	for i := range src.Pix {
		src.Pix[i] = 0xff // opaque white
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	sum := h.putBlob(buf.Bytes())
	for _, tc := range []struct {
		bg   string
		want color.NRGBA
	}{
		{"", color.NRGBA{}},
		{"102030", color.NRGBA{0x10, 0x20, 0x30, 0xff}},
	} {
		q := url.Values{"cw": {"200"}, "ch": {"200"}, "x": {"0"}, "y": {"0"}, "w": {"200"}, "h": {"200"},
			"pw": {"200"}, "ph": {"200"}, "fit": {"contain"}}
		if tc.bg != "" {
			q.Set("bg", tc.bg)
		}
		resp, err := adminGet(h, "/api/v1/blobs/"+sum+"/render?"+q.Encode())
		if err != nil || resp.code != 200 {
			t.Fatalf("bg=%q: %v %d %s", tc.bg, err, resp.code, resp.body)
		}
		img, err := png.Decode(bytes.NewReader(resp.body))
		if err != nil {
			t.Fatal(err)
		}
		// The image covers rows 75..125; row 10 is letterbox.
		if got := color.NRGBAModel.Convert(img.At(100, 10)).(color.NRGBA); got != tc.want {
			t.Errorf("bg=%q: letterbox %v, want %v", tc.bg, got, tc.want)
		}
		if got := color.NRGBAModel.Convert(img.At(100, 100)).(color.NRGBA); got != (color.NRGBA{0xff, 0xff, 0xff, 0xff}) {
			t.Errorf("bg=%q: image pixel %v", tc.bg, got)
		}
	}
	if p, err := parseRenderParams(url.Values{"cw": {"1"}, "ch": {"1"}, "w": {"1"}, "h": {"1"}, "pw": {"1"}, "ph": {"1"}}); err != nil || p.bg != (color.RGBA{}) || p.bgHex != "" {
		t.Fatalf("default bg %v %q %v", p.bg, p.bgHex, err)
	}
}
