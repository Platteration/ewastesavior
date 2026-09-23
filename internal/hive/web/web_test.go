package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const wantCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// wantTypes is written out independently of contentTypes so a typo there
// can't make the test agree with itself.
var wantTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
}

// staticFiles returns every embedded dashboard file by its served path.
func staticFiles(t *testing.T) map[string][]byte {
	t.Helper()
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, p)
		files[p] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded files")
	}
	return files
}

func filesWithExt(t *testing.T, ext string) map[string][]byte {
	out := map[string][]byte{}
	for p, b := range staticFiles(t) {
		if path.Ext(p) == ext {
			out[p] = b
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s files embedded", ext)
	}
	return out
}

func do(t *testing.T, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec
}

func checkSecurityHeaders(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	h := rec.Header()
	if got := h.Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("%s: CSP = %q, want %q", what, got, wantCSP)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options = %q", what, got)
	}
	if got := h.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("%s: Referrer-Policy = %q", what, got)
	}
	if got := h.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("%s: X-Frame-Options = %q", what, got)
	}
	for _, k := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
		if h.Get(k) != "" {
			t.Errorf("%s: unexpected CORS header %s", what, k)
		}
	}
}

func TestCSPConstant(t *testing.T) {
	if ContentSecurityPolicy != wantCSP {
		t.Fatalf("ContentSecurityPolicy = %q, want %q", ContentSecurityPolicy, wantCSP)
	}
}

func TestServesIndex(t *testing.T) {
	index := staticFiles(t)["index.html"]
	for _, target := range []string{"/", "/index.html"} {
		rec := do(t, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != wantTypes[".html"] {
			t.Errorf("GET %s: Content-Type %q", target, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s: Cache-Control %q, want no-cache", target, cc)
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("GET %s: no ETag", target)
		}
		if !bytes.Equal(rec.Body.Bytes(), index) {
			t.Errorf("GET %s: body differs from index.html", target)
		}
		checkSecurityHeaders(t, "GET "+target, rec)
	}
}

func TestServesEveryAsset(t *testing.T) {
	for name, body := range staticFiles(t) {
		want, ok := wantTypes[path.Ext(name)]
		if !ok {
			t.Errorf("%s: unexpected file type embedded; add it to contentTypes and this test", name)
			continue
		}
		rec := do(t, http.MethodGet, "/"+name, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /%s: status %d", name, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != want {
			t.Errorf("GET /%s: Content-Type %q, want %q", name, ct, want)
		}
		if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
			t.Errorf("GET /%s: Content-Length %q, want %d", name, cl, len(body))
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET /%s: Cache-Control %q", name, cc)
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Errorf("GET /%s: body differs from the embedded file", name)
		}
		checkSecurityHeaders(t, "GET /"+name, rec)
	}
}

func TestAssetBudget(t *testing.T) {
	const budget = 150_000 // bytes, uncompressed
	total := 0
	for _, b := range staticFiles(t) {
		total += len(b)
	}
	t.Logf("dashboard assets: %d bytes", total)
	if total >= budget {
		t.Errorf("dashboard assets are %d bytes, budget is < %d", total, budget)
	}
}

func TestSPAFallback(t *testing.T) {
	index := staticFiles(t)["index.html"]
	for _, target := range []string{"/nodes", "/jobs/j0123456789abcdef", "/deep/route/here", "/js", "/js/", "/%E2%9C%93"} {
		rec := do(t, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), index) {
			t.Errorf("GET %s: status %d, want index.html", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != wantTypes[".html"] {
			t.Errorf("GET %s: Content-Type %q", target, ct)
		}
		checkSecurityHeaders(t, "GET "+target, rec)
	}
}

func TestMissingAssetIs404(t *testing.T) {
	for _, target := range []string{"/missing.js", "/js/nope.js", "/favicon.ico", "/x.png", "/static/index.html", "/web.go"} {
		rec := do(t, http.MethodGet, target, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("GET %s: Content-Type %q", target, ct)
		}
		checkSecurityHeaders(t, "GET "+target, rec)
	}
}

func TestAPIPathsNeverGetHTML(t *testing.T) {
	for _, target := range []string{"/api", "/api/", "/api/v1/nope", "/api/v2/admin/info", "/js/../api/v1/x", "/api/v1/app.js"} {
		rec := do(t, http.MethodGet, target, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: Content-Type %q", target, ct)
		}
		if !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("GET %s: body %q lacks an error field", target, rec.Body.String())
		}
		checkSecurityHeaders(t, "GET "+target, rec)
	}
}

func TestPathTraversal(t *testing.T) {
	index := staticFiles(t)["index.html"]
	src, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{
		"/../web.go", "/..%2fweb.go", "/%2e%2e/web.go", "/%2e%2e%2fweb.go", "/static/../web.go", "/js/../../web.go",
		"/..%5cweb.go", "/....//web.go", "/./web.go", "/js/..%2f..%2fweb.go", "/../../../../etc/passwd",
		"/%2e%2e%2f%2e%2e%2fetc%2fpasswd", "//etc/passwd", "/js/%2e%2e/%2e%2e/web_test.go",
	}
	check := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		body := rec.Body.Bytes()
		if bytes.Contains(body, []byte("package web")) || bytes.Contains(body, []byte("root:")) || bytes.Equal(body, src) {
			t.Errorf("%s: leaked a file outside the embedded assets", what)
		}
		if rec.Code != http.StatusNotFound && !(rec.Code == http.StatusOK && bytes.Equal(body, index)) {
			t.Errorf("%s: status %d with unexpected body", what, rec.Code)
		}
		checkSecurityHeaders(t, what, rec)
	}
	for _, target := range targets {
		check("GET "+target, do(t, http.MethodGet, target, nil))
	}
	// Paths that never come out of net/http's parser, as a handler mounted
	// behind another router might see them.
	for _, p := range []string{"", "web.go", "../web.go", "..", "../../static/index.html", `..\web.go`, "\x00"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = p
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, req)
		check("raw path "+strconv.Quote(p), rec)
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		in   string
		name string
		api  bool
	}{
		{"/", "index.html", false},
		{"", "index.html", false},
		{"/js/app.js", "js/app.js", false},
		{"js/app.js", "js/app.js", false},
		{"/../../js/app.js", "js/app.js", false},
		{"/a/b/../../app.css", "app.css", false},
		{"/js/./app.js", "js/app.js", false},
		{"/api", "", true},
		{"/api/v1/hello", "", true},
		{"/x/../api/v1", "", true},
		{"/apis", "apis", false},
	}
	for _, c := range cases {
		name, api := resolve(c.in)
		if name != c.name || api != c.api {
			t.Errorf("resolve(%q) = %q, %v; want %q, %v", c.in, name, api, c.name, c.api)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, "TRACE"} {
		rec := do(t, m, "/", nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /: status %d", m, rec.Code)
		}
		if a := rec.Header().Get("Allow"); a != "GET, HEAD" {
			t.Errorf("%s /: Allow %q", m, a)
		}
		checkSecurityHeaders(t, m+" /", rec)
	}
}

func TestHead(t *testing.T) {
	index := staticFiles(t)["index.html"]
	rec := do(t, http.MethodHead, "/", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD /: status %d, body %d bytes", rec.Code, rec.Body.Len())
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(index)) {
		t.Errorf("HEAD /: Content-Length %q, want %d", cl, len(index))
	}
	checkSecurityHeaders(t, "HEAD /", rec)
}

func TestConditionalGet(t *testing.T) {
	first := do(t, http.MethodGet, "/js/app.js", nil)
	etag := first.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag %q is not a quoted strong validator", etag)
	}
	for _, inm := range []string{etag, "W/" + etag, "*", `"other", ` + etag} {
		rec := do(t, http.MethodGet, "/js/app.js", map[string]string{"If-None-Match": inm})
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %s: status %d, body %d bytes", inm, rec.Code, rec.Body.Len())
		}
		if rec.Header().Get("ETag") != etag {
			t.Errorf("If-None-Match %s: 304 without the ETag", inm)
		}
		checkSecurityHeaders(t, "304", rec)
	}
	rec := do(t, http.MethodGet, "/js/app.js", map[string]string{"If-None-Match": `"stale"`})
	if rec.Code != http.StatusOK {
		t.Errorf("stale If-None-Match: status %d", rec.Code)
	}
	other := do(t, http.MethodGet, "/app.css", nil).Header().Get("ETag")
	if other == etag {
		t.Error("different files share an ETag")
	}
}

func TestGzip(t *testing.T) {
	const target = "/js/jobs.js"
	body := staticFiles(t)["js/jobs.js"]
	plain := do(t, http.MethodGet, target, nil)
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatal("compressed without Accept-Encoding")
	}
	if !strings.Contains(plain.Header().Get("Vary"), "Accept-Encoding") {
		t.Error("compressible asset lacks Vary: Accept-Encoding")
	}
	rec := do(t, http.MethodGet, target, map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	if rec.Header().Get("Content-Type") != wantTypes[".js"] {
		t.Errorf("gzip Content-Type %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Error("gzip and identity responses share an ETag")
	}
	raw := rec.Body.Bytes()
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(raw)) {
		t.Errorf("gzip Content-Length %q, body is %d bytes", cl, len(raw))
	}
	if len(raw) >= len(body) {
		t.Errorf("gzip body (%d bytes) is not smaller than the asset (%d)", len(raw), len(body))
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("gzip body does not decompress to the asset")
	}
	checkSecurityHeaders(t, "gzip", rec)
	// A cached identity copy must not validate against the gzip variant.
	rec = do(t, http.MethodGet, target, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": plain.Header().Get("ETag")})
	if rec.Code != http.StatusOK {
		t.Errorf("identity ETag matched the gzip variant: status %d", rec.Code)
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                              false,
		"gzip":                          true,
		"GZIP":                          true,
		"x-gzip":                        true,
		"deflate, br":                   false,
		"gzip;q=0":                      false,
		"gzip; q=0.000":                 false,
		"gzip;q=0.5, br":                true,
		"*":                             true,
		"*;q=0":                         false,
		"gzip;q=0, *":                   false,
		"identity, *;q=0.1":             true,
		"br;q=1.0, gzip;q=0.8, *;q=0.1": true,
		"gzip;q=bogus":                  false,
	}
	for in, want := range cases {
		if got := acceptsGzip([]string{in}); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", in, got, want)
		}
	}
	if !acceptsGzip([]string{"br", "gzip"}) {
		t.Error("multiple Accept-Encoding header lines not combined")
	}
}

func TestNewHandler(t *testing.T) {
	if _, err := newHandler(fstest.MapFS{"app.js": {Data: []byte("x")}}); err == nil {
		t.Error("newHandler accepted a tree without index.html")
	}
	h, err := newHandler(fstest.MapFS{
		"index.html":    {Data: []byte("<!DOCTYPE html>")},
		"sub/deep.js":   {Data: bytes.Repeat([]byte("let a = 1;\n"), 200)},
		"odd.unknownxt": {Data: []byte("?")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for target, ct := range map[string]string{"/sub/deep.js": wantTypes[".js"], "/odd.unknownxt": "application/octet-stream"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ct {
			t.Errorf("GET %s: status %d, Content-Type %q", target, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if h.assets["sub/deep.js"].gz == nil {
		t.Error("compressible asset has no gzip variant")
	}
	if h.assets["index.html"].gz != nil {
		t.Error("tiny asset was compressed")
	}
}

func TestHandlerConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	hs := make([]http.Handler, 8)
	for i := range hs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hs[i] = Handler()
			rec := httptest.NewRecorder()
			hs[i].ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/js/app.js", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("status %d", rec.Code)
			}
		}(i)
	}
	wg.Wait()
	for _, h := range hs[1:] {
		if h != hs[0] {
			t.Fatal("Handler returned different handlers")
		}
	}
}

// --- Static content checks ---------------------------------------------

var (
	attrRefRE   = regexp.MustCompile(`(?i)\s(?:src|href)\s*=\s*"([^"]*)"`)
	scriptTagRE = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	handlerRE   = regexp.MustCompile(`(?i)<[^>]*\son[a-z]+\s*=`)
	styleAttrRE = regexp.MustCompile(`(?i)<[^>]*\sstyle\s*=`)
	styleTagRE  = regexp.MustCompile(`(?i)<style\b`)
	externalRE  = regexp.MustCompile(`(?i)(?:https?:)?//[a-z0-9-]+(?:\.[a-z0-9-]+)+|@import|url\(\s*['"]?(?:https?:|//)`)
	importRE    = regexp.MustCompile(`(?m)^\s*(?:import|export)\s[^'";]*?\bfrom\s*['"]([^'"]+)['"]|^\s*import\s*['"]([^'"]+)['"]|\bimport\s*\(\s*['"]([^'"]+)['"]\s*\)`)
)

func TestIndexReferencesOnlyEmbeddedAssets(t *testing.T) {
	files := staticFiles(t)
	index := string(files["index.html"])
	refs := attrRefRE.FindAllStringSubmatch(index, -1)
	if len(refs) == 0 {
		t.Fatal("index.html references nothing")
	}
	sawScript := false
	for _, m := range refs {
		ref := m[1]
		if strings.HasPrefix(ref, "#") {
			continue // hash route or in-page anchor
		}
		// Absolute same-origin paths keep working when index.html is served
		// for a deep link.
		if !strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "//") {
			t.Errorf("index.html: reference %q must be an absolute same-origin path", ref)
			continue
		}
		name := strings.TrimPrefix(ref, "/")
		if _, ok := files[name]; !ok {
			t.Errorf("index.html: reference %q is not an embedded file", ref)
		}
		if name == "js/app.js" {
			sawScript = true
		}
	}
	if !sawScript {
		t.Error("index.html does not load /js/app.js")
	}
}

func TestNoInlineCodeInMarkup(t *testing.T) {
	for _, ext := range []string{".html", ".svg"} {
		for name, b := range filesWithExt(t, ext) {
			s := string(b)
			for _, m := range scriptTagRE.FindAllStringSubmatch(s, -1) {
				if !regexp.MustCompile(`(?i)\ssrc\s*=`).MatchString(m[1]) {
					t.Errorf("%s: <script> without src", name)
				}
				if strings.TrimSpace(m[2]) != "" {
					t.Errorf("%s: <script> with inline content", name)
				}
			}
			if n := strings.Count(strings.ToLower(s), "<script"); n != len(scriptTagRE.FindAllString(s, -1)) {
				t.Errorf("%s: unterminated or odd <script> tag", name)
			}
			if handlerRE.MatchString(s) {
				t.Errorf("%s: inline event handler attribute", name)
			}
			if styleAttrRE.MatchString(s) {
				t.Errorf("%s: style attribute", name)
			}
			if styleTagRE.MatchString(s) {
				t.Errorf("%s: <style> element", name)
			}
			if strings.Contains(strings.ToLower(s), "javascript:") {
				t.Errorf("%s: javascript: URL", name)
			}
			if ext == ".svg" && strings.Contains(strings.ToLower(s), "<script") {
				t.Errorf("%s: script in SVG", name)
			}
		}
	}
}

func TestNoExternalResources(t *testing.T) {
	for _, ext := range []string{".html", ".css", ".svg"} {
		for name, b := range filesWithExt(t, ext) {
			s := string(b)
			if ext == ".svg" {
				s = strings.ReplaceAll(s, `xmlns="http://www.w3.org/2000/svg"`, "")
			}
			if m := externalRE.FindString(s); m != "" {
				t.Errorf("%s: external resource reference %q", name, m)
			}
		}
	}
	for name, b := range filesWithExt(t, ".js") {
		for _, m := range importRE.FindAllStringSubmatch(string(b), -1) {
			spec := m[1] + m[2] + m[3]
			if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
				t.Errorf("%s: import %q is not a relative module path", name, spec)
			}
		}
		if regexp.MustCompile(`fetch\(\s*['"](?:https?:)?//`).Match(b) {
			t.Errorf("%s: fetches another origin", name)
		}
	}
}

func TestJSImportsResolveAndAreReachable(t *testing.T) {
	files := staticFiles(t)
	js := filesWithExt(t, ".js")
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		b, ok := files[name]
		if !ok {
			t.Errorf("module %s is imported but not embedded", name)
			return
		}
		for _, m := range importRE.FindAllStringSubmatch(string(b), -1) {
			spec := m[1] + m[2] + m[3]
			walk(path.Join(path.Dir(name), spec))
		}
	}
	walk("js/app.js")
	var dead []string
	for name := range js {
		if !seen[name] {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("modules not reachable from js/app.js: %v", dead)
	}
}

// TestJSNoDangerousSinks enforces the rule that untrusted strings are only
// ever inserted as text: nothing in the dashboard parses markup or code.
func TestJSNoDangerousSinks(t *testing.T) {
	forbidden := []struct {
		re   *regexp.Regexp
		what string
	}{
		{regexp.MustCompile(`innerHTML`), "innerHTML"},
		{regexp.MustCompile(`outerHTML`), "outerHTML"},
		{regexp.MustCompile(`insertAdjacentHTML`), "insertAdjacentHTML"},
		{regexp.MustCompile(`document\s*\.\s*write`), "document.write"},
		{regexp.MustCompile(`\beval\s*\(`), "eval"},
		{regexp.MustCompile(`\bnew\s+Function\b|\bFunction\s*\(`), "Function constructor"},
		{regexp.MustCompile(`\bset(?:Timeout|Interval)\s*\(\s*['"\x60]`), "string timer"},
		{regexp.MustCompile(`srcdoc`), "srcdoc"},
		{regexp.MustCompile(`createContextualFragment`), "createContextualFragment"},
		{regexp.MustCompile(`DOMParser`), "DOMParser"},
		{regexp.MustCompile(`(?i)javascript:`), "javascript: URL"},
		{regexp.MustCompile(`setAttribute\(\s*['"](?:on[a-z]*|style|href|src|srcset|formaction|action)['"]`), "setAttribute of a URL, style or handler attribute"},
		{regexp.MustCompile(`\.(?:href|src|action)\s*=[^=]`), "direct URL property assignment (use h() so safeURL applies)"},
		{regexp.MustCompile(`\.on[a-z]+\s*=\s*['"\x60]`), "string event handler"},
		{regexp.MustCompile(`\bwindow\.open\s*\(`), "window.open"},
		{regexp.MustCompile(`localStorage|sessionStorage|document\.cookie`), "client-side storage of secrets"},
	}
	for name, b := range filesWithExt(t, ".js") {
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			for _, f := range forbidden {
				if f.re.MatchString(line) {
					t.Errorf("%s:%d: %s: %s", name, i+1, f.what, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestJSStaysES2017 catches newer syntax and APIs that old browsers lack
// (object spread and other syntax are also checked by the node/eslint runs).
func TestJSStaysES2017(t *testing.T) {
	forbidden := []struct {
		re   *regexp.Regexp
		what string
	}{
		{regexp.MustCompile(`\?\.[^0-9]`), "optional chaining"},
		{regexp.MustCompile(`\?\?`), "nullish coalescing"},
		{regexp.MustCompile(`\bcatch\s*\{`), "optional catch binding"},
		{regexp.MustCompile(`\.finally\s*\(`), "Promise.prototype.finally"},
		{regexp.MustCompile(`Object\.fromEntries|\.flatMap\(|\.flat\(|\.replaceAll\(|\.matchAll\(|Promise\.allSettled|Promise\.any|\bglobalThis\b|\.at\(-?\d`), "post-ES2017 API"},
		{regexp.MustCompile(`\bAbortSignal\.(?:any|timeout)\b|\bstructuredClone\b`), "recent web API"},
	}
	for name, b := range filesWithExt(t, ".js") {
		for i, line := range strings.Split(string(b), "\n") {
			for _, f := range forbidden {
				if f.re.MatchString(line) {
					t.Errorf("%s:%d: %s: %s", name, i+1, f.what, strings.TrimSpace(line))
				}
			}
		}
	}
}

// --- Node-based checks (skipped without Node.js 22+) ----------------------

func nodeBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("SAVIOR_NODE")}
	if p, err := exec.LookPath("node"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "/opt/node22/bin/node")
	for _, c := range candidates {
		if c == "" {
			continue
		}
		out, err := exec.Command(c, "--version").Output()
		if err != nil {
			continue
		}
		v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
		major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
		if major >= 22 {
			return c
		}
	}
	t.Skip("Node.js 22+ not found (set SAVIOR_NODE); skipping JavaScript checks")
	return ""
}

// TestJSSyntax parses every module with node. Plain `node --check file.js`
// silently accepts broken ES modules (module-type detection), so each file
// is fed on stdin with --input-type=module.
func TestJSSyntax(t *testing.T) {
	node := nodeBinary(t)
	for name, b := range filesWithExt(t, ".js") {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, node, "--input-type=module", "--check")
		cmd.Stdin = bytes.NewReader(b)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Errorf("%s: node --check: %v\n%s", name, err, out)
		}
	}
}

// TestJSUnit runs testdata/unit.mjs: the pure helpers (validation mirroring
// internal/proto, parsing, wall layout, formatting, API helpers) and the
// DOM builder against a stub document.
func TestJSUnit(t *testing.T) {
	node := nodeBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "testdata/unit.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("node testdata/unit.mjs: %v\n%s", err, out)
	}
	t.Logf("%s", bytes.TrimSpace(out))
}
