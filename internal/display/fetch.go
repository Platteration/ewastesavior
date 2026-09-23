package display

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"image"
	"image/color"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/imaging"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// maxRedirects bounds the redirects URLFetcher follows.
const maxRedirects = 3

// URLFetcher loads URL media for the display (DESIGN 11.5): its own HTTP
// client (no credentials, system CA verification, http/https only, at most
// 3 redirects, 64 MiB cap), optional SHA-256 pinning, guarded decoding,
// then crop+scale to the request with integer scaling. Scaled frames are
// cached; compressed bytes can be cached on tmpfs too.
type URLFetcher struct {
	Client   *http.Client
	MaxBytes int64          // download cap; default proto.MaxMediaBytes
	Limits   imaging.Limits // decode limits; default imaging.DefaultLimits
	// MemAvailable returns available memory in bytes (0 = unknown); an
	// image needing more than a quarter of it to decode (w*h*8) is refused.
	MemAvailable func() int64
	// Cache holds scaled frames; nil disables frame caching.
	Cache *FrameCache
	// BytesCacheDir, when set, caches downloaded bytes (tmpfs recommended)
	// up to BytesCacheMax bytes, so slideshows need not download again.
	BytesCacheDir string
	BytesCacheMax int64

	dirMu sync.Mutex
}

// NewURLFetcher returns a fetcher with DESIGN 11.5 defaults.
func NewURLFetcher() *URLFetcher {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       60 * time.Second,
	}
	return &URLFetcher{
		Client:        &http.Client{Transport: tr, Timeout: 3 * time.Minute, CheckRedirect: checkRedirect},
		MaxBytes:      proto.MaxMediaBytes,
		Limits:        imaging.DefaultLimits,
		MemAvailable:  memAvailableBytes,
		Cache:         NewFrameCache(DefaultCacheBytes()),
		BytesCacheMax: 32 << 20,
	}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
	}
	return nil
}

func (f *URLFetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return proto.MaxMediaBytes
}

func (f *URLFetcher) limits() imaging.Limits {
	if f.Limits.MaxBytes > 0 {
		return f.Limits
	}
	return imaging.DefaultLimits
}

// Fetch implements Env.Fetch for URL media.
func (f *URLFetcher) Fetch(ctx context.Context, m proto.Media, req FetchRequest) (image.Image, error) {
	if err := req.valid(); err != nil {
		return nil, err
	}
	if m.URL == "" {
		return nil, errors.New("blob media must be fetched from the hive")
	}
	if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
		return nil, errors.New("media url must be http or https")
	}
	key := m.Key() + "|" + req.key()
	if img := f.Cache.Get(key); img != nil {
		return img, nil
	}
	src, err := f.load(ctx, m)
	if err != nil {
		return nil, err
	}
	out := imaging.RenderRegion(src, req.CanvasW, req.CanvasH, fitOf(req.Fit), req.Rect, req.PixelW, req.PixelH, color.RGBA{})
	if imageBytes(src) > 8<<20 {
		// src is dead from here on: hand the decode buffer back to the OS
		// now, 256 MB machines can't wait for the scavenger.
		debug.FreeOSMemory()
	}
	f.Cache.Add(key, out)
	return out, nil
}

// load downloads and decodes the original image.
func (f *URLFetcher) load(ctx context.Context, m proto.Media) (image.Image, error) {
	if f.BytesCacheDir != "" {
		return f.loadCached(ctx, m)
	}
	body, err := f.get(ctx, m.URL)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	h := sha256.New()
	cr := &cappedReader{r: body, max: f.maxBytes()}
	tee := io.TeeReader(cr, h)
	img, derr := f.decode(tee)
	_, _ = io.Copy(io.Discard, tee) // hash the rest (bounded by the cap)
	if cr.over {
		return nil, fmt.Errorf("%w: more than %d MiB", imaging.ErrTooLarge, f.maxBytes()>>20)
	}
	if err := checkSum(m, h); err != nil {
		return nil, err
	}
	return img, derr
}

func checkSum(m proto.Media, h hash.Hash) error {
	if m.SHA256 == "" {
		return nil
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(m.SHA256) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got[:12], strings.ToLower(m.SHA256)[:min(12, len(m.SHA256))])
	}
	return nil
}

// get performs the GET and checks status and declared size.
func (f *URLFetcher) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "SaviorOS/"+version.Version)
	req.Header.Set("Accept", "image/*")
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute, CheckRedirect: checkRedirect}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	if resp.ContentLength > f.maxBytes() {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: %d bytes (max %d MiB)", imaging.ErrTooLarge, resp.ContentLength, f.maxBytes()>>20)
	}
	return resp.Body, nil
}

// decode checks the declared dimensions against available memory, then
// decodes with imaging.DecodeLimited (dimension and byte limits).
func (f *URLFetcher) decode(r io.Reader) (image.Image, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(64 << 10)
	lim := f.limits()
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(head)); err == nil &&
		cfg.Width > 0 && cfg.Height > 0 && cfg.Width <= lim.MaxSide && cfg.Height <= lim.MaxSide {
		need := int64(cfg.Width) * int64(cfg.Height) * 8
		avail := int64(0)
		if f.MemAvailable != nil {
			avail = f.MemAvailable()
		}
		if avail > 0 && need > avail/4 {
			return nil, fmt.Errorf("%w: %dx%d needs %d MiB to decode, only %d MiB available",
				imaging.ErrTooLarge, cfg.Width, cfg.Height, need>>20, avail>>20)
		}
	}
	img, _, err := imaging.DecodeLimited(br, lim)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return img, nil
}

// DecodeFrame decodes an image from r with the DESIGN 11.5 guards
// (dimension, pixel and byte limits, and w*h*8 <= 25% of available memory)
// and renders it as req describes, with a transparent letterbox. The node
// uses it for PNGs from the hive's render endpoint (pass a request whose
// canvas and rect are the pixel size, fit "stretch") and for the local
// blob fallback. memAvailable nil means /proc/meminfo.
func DecodeFrame(r io.Reader, req FetchRequest, memAvailable func() int64) (image.Image, error) {
	if err := req.valid(); err != nil {
		return nil, err
	}
	if memAvailable == nil {
		memAvailable = memAvailableBytes
	}
	f := &URLFetcher{MemAvailable: memAvailable}
	src, err := f.decode(&cappedReader{r: r, max: f.maxBytes()})
	if err != nil {
		return nil, err
	}
	return imaging.RenderRegion(src, req.CanvasW, req.CanvasH, fitOf(req.Fit), req.Rect, req.PixelW, req.PixelH, color.RGBA{}), nil
}

// cappedReader reads at most max bytes and records whether more existed.
type cappedReader struct {
	r    io.Reader
	n    int64
	max  int64
	over bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n >= c.max {
		var one [1]byte
		if n, _ := c.r.Read(one[:]); n > 0 {
			c.over = true
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.max-c.n {
		p = p[:c.max-c.n]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// loadCached serves the compressed bytes from BytesCacheDir, downloading
// (and verifying) them first when missing.
func (f *URLFetcher) loadCached(ctx context.Context, m proto.Media) (image.Image, error) {
	sum := sha256.Sum256([]byte(m.Key()))
	name := filepath.Join(f.BytesCacheDir, hex.EncodeToString(sum[:16]))
	if file, err := os.Open(name); err == nil {
		img, derr := f.decode(file)
		file.Close()
		if derr == nil {
			now := time.Now()
			_ = os.Chtimes(name, now, now)
			return img, nil
		}
		_ = os.Remove(name) // corrupt cache entry: fetch again
	}
	if err := os.MkdirAll(f.BytesCacheDir, 0o700); err != nil {
		return nil, err
	}
	body, err := f.get(ctx, m.URL)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(f.BytesCacheDir, ".dl-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	cr := &cappedReader{r: body, max: f.maxBytes()}
	n, err := io.Copy(io.MultiWriter(tmp, h), cr)
	if err != nil {
		return nil, err
	}
	if cr.over {
		return nil, fmt.Errorf("%w: more than %d MiB", imaging.ErrTooLarge, f.maxBytes()>>20)
	}
	if err := checkSum(m, h); err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, err := f.decode(tmp)
	if err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil { // before rename (Windows)
		return img, nil
	}
	if n <= f.BytesCacheMax {
		f.dirMu.Lock()
		if os.Rename(tmp.Name(), name) == nil {
			f.pruneLocked(name)
		}
		f.dirMu.Unlock()
	}
	return img, nil
}

// pruneLocked deletes the oldest cached files until the directory fits
// BytesCacheMax, never deleting keep.
func (f *URLFetcher) pruneLocked(keep string) {
	ents, err := os.ReadDir(f.BytesCacheDir)
	if err != nil {
		return
	}
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, file{filepath.Join(f.BytesCacheDir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, fl := range files {
		if total <= f.BytesCacheMax {
			break
		}
		if fl.path == keep {
			continue
		}
		if os.Remove(fl.path) == nil {
			total -= fl.size
		}
	}
}
