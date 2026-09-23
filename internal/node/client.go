package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// HTTPError is a non-2xx answer from the hive.
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("hive: %d %s", e.Status, e.Msg)
	}
	return fmt.Sprintf("hive: HTTP %d", e.Status)
}

// statusOf returns the HTTP status of err, or 0.
func statusOf(err error) int {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// hiveClient talks to one hive over connections pinned to one certificate
// fingerprint. A proof or token is never sent over any other connection.
type hiveClient struct {
	base string // https://host:port
	fp   string
	hc   *http.Client

	mu    sync.Mutex
	token string
}

func newTransport(tlsPin string, seen func(string)) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext:           d.DialContext,
		TLSClientConfig:       auth.ClientTLSConfig(tlsPin, seen),
		TLSHandshakeTimeout:   20 * time.Second, // ECDSA on a Pentium III is slow
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // per-request contexts bound long polls
		ForceAttemptHTTP2:     false,
	}
}

// probeHello fetches /hello with trust-on-first-use (or the configured pin)
// and returns the fingerprint actually presented.
func probeHello(ctx context.Context, base, pin string) (proto.Hello, string, error) {
	var mu sync.Mutex
	seen := ""
	tr := newTransport(pin, func(fp string) {
		mu.Lock()
		seen = fp
		mu.Unlock()
	})
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	var h proto.Hello
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/hello", nil)
	if err != nil {
		return h, "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return h, "", err
	}
	defer resp.Body.Close()
	if err := decodeResponse(resp, &h); err != nil {
		return h, "", err
	}
	mu.Lock()
	defer mu.Unlock()
	if seen == "" {
		return h, "", errors.New("no certificate seen")
	}
	return h, seen, nil
}

func newHiveClient(base, fp string) *hiveClient {
	return &hiveClient{base: base, fp: fp, hc: &http.Client{Transport: newTransport(fp, nil)}}
}

func (c *hiveClient) setToken(t string) {
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
}

func (c *hiveClient) close() {
	if tr, ok := c.hc.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (c *hiveClient) do(ctx context.Context, method, path string, body io.Reader, contentType string, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return c.hc.Do(req)
}

// postJSON POSTs in as JSON and decodes the reply into out (may be nil).
func (c *hiveClient) postJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(b), "application/json", int64(len(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, out)
}

func (c *hiveClient) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil, "", -1)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, out)
}

func decodeResponse(resp *http.Response, out any) error {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e proto.ErrorResponse
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &HTTPError{Status: resp.StatusCode, Msg: proto.Sanitize(e.Error, 200, false)}
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("hive: bad response: %w", err)
	}
	return nil
}

// Hive API calls.

func (c *hiveClient) register(ctx context.Context, req *proto.RegisterRequest) (proto.RegisterResponse, error) {
	var resp proto.RegisterResponse
	err := c.postJSON(ctx, "/api/v1/register", req, &resp)
	return resp, err
}

func (c *hiveClient) heartbeat(ctx context.Context, req *proto.HeartbeatRequest) (proto.HeartbeatResponse, error) {
	var resp proto.HeartbeatResponse
	err := c.postJSON(ctx, "/api/v1/heartbeat", req, &resp)
	return resp, err
}

func (c *hiveClient) claim(ctx context.Context, req *proto.ClaimRequest) (proto.ClaimResponse, error) {
	var resp proto.ClaimResponse
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.WaitS+15)*time.Second)
	defer cancel()
	err := c.postJSON(ctx, "/api/v1/claim", req, &resp)
	return resp, err
}

func (c *hiveClient) report(ctx context.Context, taskID string, r *proto.TaskReport) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.postJSON(ctx, "/api/v1/tasks/"+url.PathEscape(taskID)+"/report", r, nil)
}

// postLog uploads a log chunk that starts at offset; it returns the hive's
// next expected offset.
func (c *hiveClient) postLog(ctx context.Context, taskID, lease string, offset int64, chunk []byte) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	path := "/api/v1/tasks/" + url.PathEscape(taskID) + "/log?lease=" + url.QueryEscape(lease) + "&offset=" + strconv.FormatInt(offset, 10)
	resp, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(chunk), "text/plain; charset=utf-8", int64(len(chunk)))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Next int64 `json:"next"`
	}
	if err := decodeResponse(resp, &out); err != nil {
		return 0, err
	}
	return out.Next, nil
}

func (c *hiveClient) stats(ctx context.Context) (*proto.SwarmStats, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var s proto.SwarmStats
	if err := c.getJSON(ctx, "/api/v1/stats", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// FetchBlob streams a blob into w and verifies its hash (runner.Transfer).
func (c *hiveClient) FetchBlob(ctx context.Context, sha string, w io.Writer) (int64, error) {
	if !proto.ValidSHA256(sha) {
		return 0, fmt.Errorf("invalid blob hash")
	}
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/blobs/"+sha, nil, "", -1)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, decodeResponse(resp, nil)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, proto.MaxBlobBytes+1))
	if err != nil {
		return n, err
	}
	if n > proto.MaxBlobBytes {
		return n, fmt.Errorf("blob larger than %d bytes", int64(proto.MaxBlobBytes))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return n, fmt.Errorf("blob hash mismatch: got %s", got)
	}
	return n, nil
}

// UploadBlob PUTs size bytes from r as blob sha256 (runner.Transfer).
func (c *hiveClient) UploadBlob(ctx context.Context, sha string, size int64, r io.Reader) error {
	resp, err := c.do(ctx, http.MethodPut, "/api/v1/blobs/"+sha, io.NopCloser(r), "application/octet-stream", size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, nil)
}

// renderBlob asks the hive to fit, crop and scale a blob image for us.
func (c *hiveClient) renderBlob(ctx context.Context, sha string, q url.Values) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/blobs/"+sha+"/render?"+q.Encode(), nil, "", -1)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decodeResponse(resp, nil)
	}
	return resp.Body, nil
}

// getBlob returns the raw blob body (caller closes).
func (c *hiveClient) getBlob(ctx context.Context, sha string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/blobs/"+sha, nil, "", -1)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decodeResponse(resp, nil)
	}
	return resp.Body, nil
}

// urlClient fetches task inputs and media from arbitrary URLs: no hive
// credentials, normal CA verification, at most 3 redirects, http(s) only.
var urlClient = &http.Client{
	Timeout: 30 * time.Minute,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout: 20 * time.Second,
		MaxIdleConns:        4,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("redirect to non-http(s) URL")
		}
		return nil
	},
}

// fetchURL implements runner.Transfer.FetchURL.
func fetchURL(ctx context.Context, rawURL string, maxBytes int64, w io.Writer) (int64, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return 0, fmt.Errorf("unsupported URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := urlClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET %s: HTTP %d", u.Redacted(), resp.StatusCode)
	}
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return 0, fmt.Errorf("download is %d bytes, more than the declared %d", resp.ContentLength, maxBytes)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return n, err
	}
	if n > maxBytes {
		return n, fmt.Errorf("download exceeds the declared size of %d bytes", maxBytes)
	}
	return n, nil
}
