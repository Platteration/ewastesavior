package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/platteration/ewastesavior/internal/display"
	"github.com/platteration/ewastesavior/internal/imaging"
	"github.com/platteration/ewastesavior/internal/proto"
)

// splitPNG encodes a w×h image, left half red and right half blue.
func splitPNG(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, image.Rect(0, 0, w/2, h), image.NewUniform(color.RGBA{255, 0, 0, 255}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(w/2, 0, w, h), image.NewUniform(color.RGBA{0, 0, 255, 255}), image.Point{}, draw.Src)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fetchMedia asks the hive to render blob media for exactly this screen or
// wall tile and takes the PNG pixel for pixel; when the hive can't render,
// it decodes the blob itself with the same result (SPEC-RUNTIME-10,
// DISPLAY-HW-5).
func TestFetchMediaRenderAndFallback(t *testing.T) {
	data := splitPNG(t, 400, 100)
	sum := sha256Hex(data)
	var (
		mu         sync.Mutex
		queries    []url.Values
		failRender atomic.Bool
		rawGets    atomic.Int32
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/blobs/{sha}/render", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		if failRender.Load() || r.PathValue("sha") != sum {
			http.Error(w, `{"error":"cannot render this blob"}`, http.StatusUnprocessableEntity)
			return
		}
		// What the hive's render endpoint does without a bg parameter.
		n := func(k string) int { v, _ := strconv.Atoi(q.Get(k)); return v }
		src, _, err := imaging.DecodeLimited(bytes.NewReader(data), imaging.DefaultLimits)
		if err != nil {
			t.Error(err)
			return
		}
		out := imaging.RenderRegion(src, n("cw"), n("ch"), q.Get("fit"),
			image.Rect(n("x"), n("y"), n("x")+n("w"), n("y")+n("h")), n("pw"), n("ph"), color.RGBA{})
		png.Encode(w, out)
	})
	mux.HandleFunc("GET /api/v1/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		rawGets.Add(1)
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := fakeAgent(nil, 0, nil)
	a.hc = newHiveClient(srv.URL, "")
	m := proto.Media{Blob: sum}
	// The right tile of a 2×1 wall: canvas 800×300 mm, tile 400×300 mm on
	// a 320×240 screen. contain puts the 4:1 image at y 50..250 mm.
	req := display.FetchRequest{CanvasW: 800, CanvasH: 300, Rect: image.Rect(400, 0, 800, 300), PixelW: 320, PixelH: 240}

	rendered, err := a.fetchMedia(context.Background(), m, req)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	q := queries[0]
	mu.Unlock()
	want := url.Values{"cw": {"800"}, "ch": {"300"}, "x": {"400"}, "y": {"0"}, "w": {"400"}, "h": {"300"},
		"pw": {"320"}, "ph": {"240"}, "fit": {"contain"}}
	if q.Encode() != want.Encode() {
		t.Fatalf("render query %s, want %s", q.Encode(), want.Encode())
	}
	if n := rawGets.Load(); n != 0 {
		t.Fatalf("fetched the raw blob %d times although the hive rendered it", n)
	}

	failRender.Store(true)
	local, err := a.fetchMedia(context.Background(), m, req)
	if err != nil {
		t.Fatalf("local fallback: %v", err)
	}
	if n := rawGets.Load(); n != 1 {
		t.Fatalf("raw blob fetched %d times by the fallback", n)
	}

	for name, img := range map[string]image.Image{"hive render": rendered, "local decode": local} {
		rgba, ok := img.(*image.RGBA)
		if !ok || rgba.Bounds() != image.Rect(0, 0, 320, 240) {
			t.Fatalf("%s: %T %v", name, img, img.Bounds())
		}
		if c := rgba.RGBAAt(160, 120); c != (color.RGBA{0, 0, 255, 255}) {
			t.Errorf("%s: tile centre %v, want blue", name, c)
		}
		// The letterbox is transparent, so the scene's bg shows through.
		if c := rgba.RGBAAt(160, 10); c.A != 0 {
			t.Errorf("%s: letterbox %v, want transparent", name, c)
		}
	}
	if !bytes.Equal(rendered.(*image.RGBA).Pix, local.(*image.RGBA).Pix) {
		t.Error("the hive render and the local fallback differ")
	}
}
