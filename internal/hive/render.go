package hive

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/platteration/ewastesavior/internal/imaging"
	"github.com/platteration/ewastesavior/internal/proto"
)

const (
	maxRenderPixels = 4096    // pw, ph limit
	maxCanvasUnits  = 1000000 // cw, ch, x, y, w, h limit (mm or px)
	renderWorkers   = 1       // concurrent decodes; each may use 64 MiB+
	renderCacheExt  = ".png"
)

// renderParams are the query parameters of the render endpoint (DESIGN 7.1).
type renderParams struct {
	cw, ch, x, y, w, h, pw, ph int
	fit                        string
	bg                         color.RGBA
	bgHex                      string
}

func parseRenderParams(q map[string][]string) (renderParams, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var p renderParams
	ints := []struct {
		name     string
		dst      *int
		min, max int
	}{
		{"cw", &p.cw, 1, maxCanvasUnits}, {"ch", &p.ch, 1, maxCanvasUnits},
		{"x", &p.x, 0, maxCanvasUnits}, {"y", &p.y, 0, maxCanvasUnits},
		{"w", &p.w, 1, maxCanvasUnits}, {"h", &p.h, 1, maxCanvasUnits},
		{"pw", &p.pw, 1, maxRenderPixels}, {"ph", &p.ph, 1, maxRenderPixels},
	}
	for _, f := range ints {
		v := get(f.name)
		if v == "" && (f.name == "x" || f.name == "y") {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < f.min || n > f.max {
			return p, fmt.Errorf("%s must be an integer in %d..%d", f.name, f.min, f.max)
		}
		*f.dst = n
	}
	switch p.fit = get("fit"); p.fit {
	case "":
		p.fit = "contain"
	case "contain", "cover", "stretch":
	default:
		return p, fmt.Errorf("fit must be contain, cover or stretch")
	}
	p.bg = color.RGBA{A: 255}
	if v := get("bg"); v != "" {
		r, g, b, ok := proto.ParseColor("#" + v)
		if !ok {
			r, g, b, ok = proto.ParseColor(v)
		}
		if !ok {
			return p, fmt.Errorf("bg must be rrggbb")
		}
		p.bg = color.RGBA{R: r, G: g, B: b, A: 255}
		p.bgHex = fmt.Sprintf("%02x%02x%02x", r, g, b)
	}
	return p, nil
}

func (p renderParams) key(sha string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d|%d|%d|%d|%d|%d|%s|%s", sha, p.cw, p.ch, p.x, p.y, p.w, p.h, p.pw, p.ph, p.fit, p.bgHex)))
	return hex.EncodeToString(h[:])
}

func (s *Server) handleRender(w http.ResponseWriter, r *http.Request, who requester) {
	sha := r.PathValue("sha")
	if !proto.ValidSHA256(sha) {
		writeErr(w, http.StatusBadRequest, "invalid blob hash")
		return
	}
	p, err := parseRenderParams(r.URL.Query())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	s.mu.Lock()
	if status, msg := s.blobReadAllowedLocked(who, sha); status != 0 {
		s.mu.Unlock()
		writeErr(w, status, "%s", msg)
		return
	}
	b := s.blobMeta[sha]
	if b == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "blob not found")
		return
	}
	s.touchLocked(b)
	s.mu.Unlock()

	setDeadlines(w, jsonDeadline*2)
	key := p.key(sha)
	data, ok := s.renders.get(key)
	if !ok {
		data, err = s.renders.do(r.Context(), key, func() ([]byte, error) { return s.renderBlob(sha, p) })
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, "cannot render this blob: %s", proto.Sanitize(err.Error(), 200, false))
			return
		}
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Security-Policy", blobCSP)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// renderBlob decodes a blob with size limits and renders the requested
// canvas region (DESIGN 11.5).
func (s *Server) renderBlob(sha string, p renderParams) ([]byte, error) {
	f, err := os.Open(s.blobs.path(sha))
	if err != nil {
		return nil, fmt.Errorf("blob not found")
	}
	defer f.Close()
	img, _, err := imaging.DecodeLimited(f, imaging.DefaultLimits)
	if err != nil {
		return nil, err
	}
	out := imaging.RenderRegion(img, p.cw, p.ch, p.fit, image.Rect(p.x, p.y, p.x+p.w, p.y+p.h), p.pw, p.ph, p.bg)
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderCache is an on-disk LRU of rendered PNGs with single-flight
// rendering and a bounded number of concurrent decodes.
type renderCache struct {
	dir    string
	max    int64
	mu     sync.Mutex
	lru    *list.List // of *cacheEntry, front = most recent
	idx    map[string]*list.Element
	size   int64
	sem    chan struct{}
	flight map[string]*renderFlight
}

type cacheEntry struct {
	key  string
	size int64
}

type renderFlight struct {
	done chan struct{}
	data []byte
	err  error
}

// newRenderCache starts empty: cached renders are derived data.
func newRenderCache(dir string, max int64) (*renderCache, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clean render cache: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create render cache: %w", err)
	}
	return &renderCache{dir: dir, max: max, lru: list.New(), idx: map[string]*list.Element{},
		sem: make(chan struct{}, renderWorkers), flight: map[string]*renderFlight{}}, nil
}

func (c *renderCache) path(key string) string { return filepath.Join(c.dir, key+renderCacheExt) }

func (c *renderCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	e, ok := c.idx[key]
	if ok {
		c.lru.MoveToFront(e)
	}
	c.mu.Unlock()
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		c.mu.Lock()
		if e, ok := c.idx[key]; ok {
			c.removeLocked(e)
		}
		c.mu.Unlock()
		return nil, false
	}
	return data, true
}

// do renders key once even with concurrent requests and caches the result.
func (c *renderCache) do(ctx context.Context, key string, fn func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if f, ok := c.flight[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.data, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &renderFlight{done: make(chan struct{})}
	c.flight[key] = f
	c.mu.Unlock()

	select {
	case c.sem <- struct{}{}:
		f.data, f.err = fn()
		<-c.sem
	case <-ctx.Done():
		f.err = ctx.Err()
	}
	if f.err == nil {
		c.store(key, f.data)
	}
	c.mu.Lock()
	delete(c.flight, key)
	c.mu.Unlock()
	close(f.done)
	return f.data, f.err
}

func (c *renderCache) store(key string, data []byte) {
	if int64(len(data)) > c.max {
		return
	}
	if err := writeFileAtomic(c.path(key), data, 0o600); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.idx[key]; ok {
		c.removeLocked(e)
	}
	c.idx[key] = c.lru.PushFront(&cacheEntry{key: key, size: int64(len(data))})
	c.size += int64(len(data))
	for c.size > c.max && c.lru.Len() > 0 {
		old := c.lru.Back()
		c.removeLocked(old)
		os.Remove(c.path(old.Value.(*cacheEntry).key))
	}
}

func (c *renderCache) removeLocked(e *list.Element) {
	ce := e.Value.(*cacheEntry)
	c.lru.Remove(e)
	delete(c.idx, ce.key)
	c.size -= ce.size
}
