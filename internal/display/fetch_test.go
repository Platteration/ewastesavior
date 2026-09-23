package display

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/imaging"
	"github.com/platteration/ewastesavior/internal/proto"
)

// pngBytes encodes a w×h image, left half red and right half blue.
func pngBytes(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fill(img, image.Rect(0, 0, w/2, h), rgb(255, 0, 0))
	fill(img, image.Rect(w/2, 0, w, h), rgb(0, 0, 255))
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngBomb is a tiny PNG whose header claims w×h pixels.
func pngBomb(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(typ string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(typ)
		b.Write(data)
		crc := crc32.NewIEEE()
		crc.Write([]byte(typ))
		crc.Write(data)
		binary.Write(&b, binary.BigEndian, crc.Sum32())
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk("IHDR", ihdr)
	chunk("IDAT", []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})
	chunk("IEND", nil)
	return b.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func screenReq(w, h int) FetchRequest {
	return FetchRequest{CanvasW: w, CanvasH: h, Rect: image.Rect(0, 0, w, h), PixelW: w, PixelH: h, Fit: "stretch"}
}

func newTestFetcher() *URLFetcher {
	f := NewURLFetcher()
	f.MemAvailable = func() int64 { return 1 << 30 }
	f.Cache = NewFrameCache(16 << 20)
	return f
}

func TestURLFetcherDecodesAndScales(t *testing.T) {
	data := pngBytes(t, 200, 100)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("credentials sent: %v", r.Header)
		}
		w.Write(data)
	}))
	defer srv.Close()
	f := newTestFetcher()
	m := proto.Media{URL: srv.URL + "/a.png", SHA256: sha(data)}
	img, err := f.Fetch(context.Background(), m, screenReq(40, 20))
	if err != nil {
		t.Fatal(err)
	}
	rgba := img.(*image.RGBA)
	if b := rgba.Bounds(); b.Dx() != 40 || b.Dy() != 20 {
		t.Fatalf("scaled to %v", b)
	}
	if rgba.RGBAAt(5, 10) != rgb(255, 0, 0) || rgba.RGBAAt(35, 10) != rgb(0, 0, 255) {
		t.Fatalf("pixels %v %v", rgba.RGBAAt(5, 10), rgba.RGBAAt(35, 10))
	}
	// Second fetch of the same frame comes from the cache.
	if _, err := f.Fetch(context.Background(), m, screenReq(40, 20)); err != nil || hits.Load() != 1 {
		t.Fatalf("cache miss: hits %d err %v", hits.Load(), err)
	}
	// A different geometry is a different frame (a wall tile: right half).
	tile := FetchRequest{CanvasW: 200, CanvasH: 100, Rect: image.Rect(100, 0, 200, 100), PixelW: 10, PixelH: 10, Fit: "contain"}
	img, err = f.Fetch(context.Background(), m, tile)
	if err != nil || img.(*image.RGBA).RGBAAt(5, 5) != rgb(0, 0, 255) || hits.Load() != 2 {
		t.Fatalf("tile: %v hits %d", err, hits.Load())
	}
}

func TestURLFetcherRejects(t *testing.T) {
	data := pngBytes(t, 20, 10)
	big := bytes.Repeat([]byte("x"), 5000)
	mux := http.NewServeMux()
	mux.HandleFunc("/ok.png", func(w http.ResponseWriter, r *http.Request) { w.Write(data) })
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) { w.Write(big) })
	mux.HandleFunc("/big-chunked", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		for i := 0; i < 10; i++ {
			w.Write(big[:600])
			w.(http.Flusher).Flush()
		}
	})
	// 15000² stays representable for image/png on 32-bit, so imaging's
	// limit is what rejects it; 100000² overflows image/png's own check.
	mux.HandleFunc("/bomb.png", func(w http.ResponseWriter, r *http.Request) { w.Write(pngBomb(15000, 15000)) })
	mux.HandleFunc("/hugebomb.png", func(w http.ResponseWriter, r *http.Request) { w.Write(pngBomb(100000, 100000)) })
	mux.HandleFunc("/wide.png", func(w http.ResponseWriter, r *http.Request) { w.Write(pngBomb(4000, 4000)) })
	mux.HandleFunc("/notimage", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>hello</html>")) })
	mux.HandleFunc("/redirect/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/redirect/"), "%d", &n)
		if n == 0 {
			w.Write(data)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/redirect/%d", n-1), http.StatusFound)
	})
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		name  string
		m     proto.Media
		setup func(f *URLFetcher)
		want  string // "" = success
	}{
		{"ok", proto.Media{URL: srv.URL + "/ok.png"}, nil, ""},
		{"sha match", proto.Media{URL: srv.URL + "/ok.png", SHA256: sha(data)}, nil, ""},
		{"sha mismatch", proto.Media{URL: srv.URL + "/ok.png", SHA256: strings.Repeat("0", 64)}, nil, "sha256 mismatch"},
		{"404", proto.Media{URL: srv.URL + "/missing"}, nil, "404"},
		{"size cap (content-length)", proto.Media{URL: srv.URL + "/big"}, func(f *URLFetcher) { f.MaxBytes = 4096 }, "too large"},
		{"size cap (chunked)", proto.Media{URL: srv.URL + "/big-chunked"}, func(f *URLFetcher) { f.MaxBytes = 4096 }, "too large"},
		{"png bomb", proto.Media{URL: srv.URL + "/bomb.png"}, nil, "too large"},
		{"huge png bomb", proto.Media{URL: srv.URL + "/hugebomb.png"}, nil, "decode"},
		{"memory guard", proto.Media{URL: srv.URL + "/wide.png"}, func(f *URLFetcher) { f.MemAvailable = func() int64 { return 256 << 20 } }, "only 256 MiB available"},
		{"not an image", proto.Media{URL: srv.URL + "/notimage"}, nil, "decode"},
		{"3 redirects ok", proto.Media{URL: srv.URL + "/redirect/3"}, nil, ""},
		{"4 redirects", proto.Media{URL: srv.URL + "/redirect/4"}, nil, "redirects"},
		{"redirect to file", proto.Media{URL: srv.URL + "/to-file"}, nil, "scheme"},
		{"blob", proto.Media{Blob: strings.Repeat("a", 64)}, nil, "hive"},
		{"ftp", proto.Media{URL: "ftp://example.com/x.png"}, nil, "http"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestFetcher()
			if tt.setup != nil {
				tt.setup(f)
			}
			_, err := f.Fetch(context.Background(), tt.m, screenReq(10, 10))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	f := newTestFetcher()
	if _, err := f.Fetch(context.Background(), proto.Media{URL: srv.URL + "/ok.png"}, FetchRequest{}); err == nil {
		t.Fatal("invalid request accepted")
	}
	_, err := newTestFetcher().Fetch(context.Background(), proto.Media{URL: srv.URL + "/bomb.png"}, screenReq(10, 10))
	if !errors.Is(err, imaging.ErrTooLarge) {
		t.Fatalf("bomb error does not wrap ErrTooLarge: %v", err)
	}
}

func TestURLFetcherCanceled(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := newTestFetcher().Fetch(ctx, proto.Media{URL: srv.URL}, screenReq(10, 10)); err == nil {
		t.Fatal("expected a timeout")
	}
}

func TestURLFetcherBytesCache(t *testing.T) {
	data := pngBytes(t, 64, 32)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(data)
	}))
	defer srv.Close()
	f := newTestFetcher()
	f.Cache = nil // force decoding every time
	f.BytesCacheDir = filepath.Join(t.TempDir(), "media")
	m := proto.Media{URL: srv.URL + "/p.png", SHA256: sha(data)}
	for i, size := range []int{10, 20, 30} {
		img, err := f.Fetch(context.Background(), m, screenReq(size, size))
		if err != nil || img.Bounds().Dx() != size {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("server hit %d times, want 1 (bytes cache)", hits.Load())
	}
	ents, _ := os.ReadDir(f.BytesCacheDir)
	if len(ents) != 1 {
		t.Fatalf("cache dir has %d entries", len(ents))
	}
	// A corrupt cache entry is replaced by a fresh download.
	os.WriteFile(filepath.Join(f.BytesCacheDir, ents[0].Name()), []byte("junk"), 0o600)
	if _, err := f.Fetch(context.Background(), m, screenReq(12, 12)); err != nil || hits.Load() != 2 {
		t.Fatalf("after corruption: %v hits %d", err, hits.Load())
	}
	// Pruning keeps the directory within its budget.
	f.BytesCacheMax = int64(len(data)) + 10
	other := proto.Media{URL: srv.URL + "/q.png"}
	if _, err := f.Fetch(context.Background(), other, screenReq(12, 12)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	ents, _ = os.ReadDir(f.BytesCacheDir)
	var total int64
	for _, e := range ents {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	if total > f.BytesCacheMax {
		t.Fatalf("cache dir holds %d bytes, max %d", total, f.BytesCacheMax)
	}
	// Mismatched content is never cached.
	f2 := newTestFetcher()
	f2.BytesCacheDir = filepath.Join(t.TempDir(), "m2")
	if _, err := f2.Fetch(context.Background(), proto.Media{URL: srv.URL + "/p.png", SHA256: strings.Repeat("1", 64)}, screenReq(5, 5)); err == nil {
		t.Fatal("sha mismatch accepted")
	}
	if ents, _ := os.ReadDir(f2.BytesCacheDir); len(ents) != 0 {
		t.Fatalf("mismatched download cached: %v", ents)
	}
}

func TestFrameCache(t *testing.T) {
	c := NewFrameCache(1000)
	img := func(n int) image.Image { return image.NewRGBA(image.Rect(0, 0, n, 1)) } // 4n bytes
	c.Add("a", img(100))                                                            // 400
	c.Add("b", img(100))                                                            // 800
	if c.Get("a") == nil {
		t.Fatal("a missing")
	}
	c.Add("c", img(100)) // evicts b (a was used more recently)
	if c.Get("b") != nil || c.Get("a") == nil || c.Get("c") == nil || c.Bytes() != 800 {
		t.Fatalf("LRU order wrong: len %d bytes %d", c.Len(), c.Bytes())
	}
	if c.AddIfRoom("d", img(100)) {
		t.Fatal("AddIfRoom evicted")
	}
	if !c.AddIfRoom("e", img(50)) || c.Bytes() != 1000 {
		t.Fatalf("AddIfRoom with room failed: %d", c.Bytes())
	}
	c.Add("huge", img(1000)) // larger than the budget: not cached, nothing evicted
	if c.Get("huge") != nil || c.Len() != 3 {
		t.Fatalf("huge image handling: len %d", c.Len())
	}
	c.Add("a", img(10))   // replace shrinks usage
	if c.Bytes() != 640 { // a (40) + c (400) + e (200)
		t.Fatalf("after replace %d bytes", c.Bytes())
	}
	var nilCache *FrameCache
	if nilCache.Get("x") != nil {
		t.Fatal("nil cache")
	}
	nilCache.Add("x", img(1))
	if n := imageBytes(image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)); n != 16+4+4 {
		t.Fatalf("YCbCr bytes %d", n)
	}
	if n := imageBytes(image.NewUniform(color.Black)); n <= 0 {
		t.Fatalf("uniform bytes %d", n)
	}
}

func TestReadMeminfo(t *testing.T) {
	old := meminfoPath
	defer func() { meminfoPath = old }()
	meminfoPath = filepath.Join(t.TempDir(), "meminfo")
	os.WriteFile(meminfoPath, []byte("MemTotal:        262144 kB\nMemFree:  1000 kB\nMemAvailable:    153600 kB\nbogus\n"), 0o644)
	total, avail := readMeminfo()
	if total != 256<<20 || avail != 150<<20 {
		t.Fatalf("total %d avail %d", total, avail)
	}
	if got := DefaultCacheBytes(); got != (256<<20)/10 {
		t.Fatalf("DefaultCacheBytes = %d, want 10%% of 256 MiB", got)
	}
	meminfoPath = filepath.Join(t.TempDir(), "missing")
	if got := DefaultCacheBytes(); got != 64<<20 {
		t.Fatalf("unknown memory: %d", got)
	}
}

func TestDecodeFrame(t *testing.T) {
	data := pngBytes(t, 30, 10)
	// A hive-rendered frame: stretched onto exactly the pixel size.
	img, err := DecodeFrame(bytes.NewReader(data), screenReq(60, 20), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 60 || b.Dy() != 20 {
		t.Fatalf("bounds %v", b)
	}
	if c := img.(*image.RGBA).RGBAAt(50, 10); c != rgb(0, 0, 255) {
		t.Fatalf("right side %v", c)
	}
	if _, err := DecodeFrame(bytes.NewReader(pngBomb(15000, 15000)), screenReq(10, 10), nil); !errors.Is(err, imaging.ErrTooLarge) {
		t.Fatalf("bomb: %v", err)
	}
	if _, err := DecodeFrame(bytes.NewReader(pngBomb(3000, 3000)), screenReq(10, 10), func() int64 { return 64 << 20 }); err == nil {
		t.Fatal("memory guard not applied")
	}
	if _, err := DecodeFrame(bytes.NewReader(data), FetchRequest{}, nil); err == nil {
		t.Fatal("invalid request accepted")
	}
}
