// Package web serves the hive's embedded dashboard: a static single-page app
// (plain HTML, CSS and JavaScript modules, no build step, no external
// resources) that talks to the admin API under /api/v1. See docs/DESIGN.md
// sections 6.3 and 7.2.
//
// The hive mounts Handler at "/" and serves the API itself. Every response
// from this package carries the dashboard's security headers. Files are only
// ever read from the embedded tree, which is loaded into memory once, so
// request paths never touch a real filesystem.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// ContentSecurityPolicy is sent with every dashboard response (DESIGN.md 6.3).
// It forbids inline scripts and styles, so the UI builds its DOM from
// JavaScript and inserts untrusted strings only as text.
const ContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'; " +
	"base-uri 'none'; form-action 'none'"

//go:embed static
var embedded embed.FS

// contentTypes maps the extensions the dashboard uses to media types. It is
// explicit rather than mime.TypeByExtension because that consults the OS, and
// a Windows registry that maps .js to text/plain breaks module scripts.
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".ico":  "image/x-icon",
	".txt":  "text/plain; charset=utf-8",
}

// gzipMinSize is the smallest asset worth compressing.
const gzipMinSize = 512

type asset struct {
	ctype  string
	body   []byte
	etag   string
	gz     []byte // gzip-compressed body; nil when compression doesn't pay off
	gzEtag string
}

type handler struct {
	assets map[string]*asset // keyed by slash path relative to the root, e.g. "js/app.js"
	index  *asset
}

var (
	loadOnce sync.Once
	loaded   http.Handler
)

// Handler returns the dashboard handler. It serves the embedded files for
// GET and HEAD, falls back to index.html for extension-less paths (the app
// uses hash routing, so these are only typed or stale URLs), answers unknown
// /api/ paths with a JSON 404, and refuses other methods.
func Handler() http.Handler {
	loadOnce.Do(func() {
		h, err := load()
		if err != nil {
			// Only reachable if the binary was built without the assets.
			slog.Error("web: dashboard assets unavailable", "err", err)
			loaded = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				setSecurityHeaders(w.Header())
				plainError(w, http.StatusInternalServerError, "dashboard assets unavailable")
			})
			return
		}
		loaded = h
	})
	return loaded
}

func load() (*handler, error) {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		return nil, fmt.Errorf("open embedded assets: %w", err)
	}
	return newHandler(sub)
}

// newHandler reads every regular file of fsys into memory.
func newHandler(fsys fs.FS) (*handler, error) {
	h := &handler{assets: map[string]*asset{}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		a, err := newAsset(name, body)
		if err != nil {
			return err
		}
		h.assets[name] = a
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load dashboard assets: %w", err)
	}
	h.index = h.assets["index.html"]
	if h.index == nil {
		return nil, errors.New("load dashboard assets: index.html missing")
	}
	return h, nil
}

func newAsset(name string, body []byte) (*asset, error) {
	sum := sha256.Sum256(body)
	tag := hex.EncodeToString(sum[:10])
	ctype, ok := contentTypes[path.Ext(name)]
	if !ok {
		ctype = "application/octet-stream"
	}
	a := &asset{ctype: ctype, body: body, etag: `"` + tag + `"`}
	if len(body) >= gzipMinSize {
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil, fmt.Errorf("compress %s: %w", name, err)
		}
		if _, err := zw.Write(body); err != nil {
			return nil, fmt.Errorf("compress %s: %w", name, err)
		}
		if err := zw.Close(); err != nil {
			return nil, fmt.Errorf("compress %s: %w", name, err)
		}
		if buf.Len() < len(body)*9/10 {
			a.gz = buf.Bytes()
			a.gzEtag = `"` + tag + `-gz"`
		}
	}
	return a, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		plainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name, api := resolve(r.URL.Path)
	if api {
		// Keep API clients from ever receiving the HTML shell.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}` + "\n"))
		return
	}
	a := h.assets[name]
	if a == nil {
		if path.Ext(name) != "" {
			plainError(w, http.StatusNotFound, "not found")
			return
		}
		a = h.index
	}
	serveAsset(w, r, a)
}

// resolve maps a request path to an asset name. Cleaning a rooted path
// removes every "." and ".." element, so the result can't leave the root;
// assets are looked up in a map anyway, never on disk.
func resolve(urlPath string) (name string, api bool) {
	p := path.Clean("/" + urlPath)
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		return "", true
	}
	name = strings.TrimPrefix(p, "/")
	if name == "" {
		name = "index.html"
	}
	return name, false
}

func serveAsset(w http.ResponseWriter, r *http.Request, a *asset) {
	hdr := w.Header()
	body, etag := a.body, a.etag
	if a.gz != nil {
		hdr.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Values("Accept-Encoding")) {
			body, etag = a.gz, a.gzEtag
			hdr.Set("Content-Encoding", "gzip")
		}
	}
	// No versioned file names, so browsers revalidate everything; the ETag
	// makes that a cheap 304 until the hive binary changes.
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("ETag", etag)
	if etagMatches(r.Header.Values("If-None-Match"), etag) {
		hdr.Del("Content-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hdr.Set("Content-Type", a.ctype)
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", ContentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	// Legacy equivalent of frame-ancestors 'none' for old browsers.
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

func plainError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg + "\n"))
}

// acceptsGzip reports whether the Accept-Encoding values allow gzip,
// honoring q=0 exclusions and the "*" wildcard.
func acceptsGzip(values []string) bool {
	gzipQ, starQ := -1.0, -1.0
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			coding, params, _ := strings.Cut(part, ";")
			q := 1.0
			for _, p := range strings.Split(params, ";") {
				k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
					f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
					if err != nil || f < 0 {
						f = 0
					}
					q = f
				}
			}
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip", "x-gzip":
				gzipQ = q
			case "*":
				starQ = q
			}
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return starQ > 0
}

// etagMatches implements the weak comparison If-None-Match uses.
func etagMatches(values []string, etag string) bool {
	for _, v := range values {
		for _, t := range strings.Split(v, ",") {
			t = strings.TrimSpace(t)
			if t == "*" || strings.TrimPrefix(t, "W/") == etag {
				return true
			}
		}
	}
	return false
}
