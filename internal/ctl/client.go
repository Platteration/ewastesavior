package ctl

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/version"
)

// HTTP header names shared with the hive.
const (
	ClientTimeHeader = "X-Savior-Client-Time" // the operator's clock (DESIGN 9)
	LogOffsetHeader  = "X-Savior-Log-Offset"  // next offset of a task log read
)

const (
	defaultTimeout   = 30 * time.Second
	maxJSONResponse  = 64 << 20
	maxErrorBody     = 64 << 10
	maxHelloBody     = 64 << 10
	maxLogResponse   = 4 << 20
	maxNodeConfig    = 64 << 10
	sessionMargin    = time.Minute
	maxSessionLength = 512
)

// Errors returned by Client. They are wrapped with context; test with errors.Is.
var (
	// ErrNotLoggedIn means there is no usable session and no admin token.
	ErrNotLoggedIn = errors.New("not logged in to the hive: run 'savior ctl login' or set SAVIOR_ADMIN_TOKEN")
	// ErrSessionExpired means the hive refused the stored session and no
	// admin token is available to log in again.
	ErrSessionExpired = errors.New("the admin session expired or was revoked: run 'savior ctl login'")
	// ErrNoFingerprint means no fingerprint is pinned and trust on first
	// use is disabled.
	ErrNoFingerprint = errors.New("no certificate fingerprint is pinned for this hive and trust on first use is off: pass --fingerprint sha256:... (shown on the hive's screen)")
	// ErrHiveProof means the hive accepted the login but could not prove
	// it knows the admin token: it is not the real hive.
	ErrHiveProof = errors.New("the hive could not prove that it knows the admin token; it may be an impostor")
)

var fingerprintRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// NormalizeFingerprint returns fp in canonical "sha256:<64 hex>" form, or an
// error if it is not a SHA-256 certificate fingerprint. Colons and case are
// ignored, and the "sha256:" prefix is optional.
func NormalizeFingerprint(fp string) (string, error) {
	n := config.NormalizeFingerprint(fp)
	if !fingerprintRE.MatchString(n) {
		return "", fmt.Errorf("invalid fingerprint %q: want sha256: followed by 64 hex digits", truncate(sanitizeCell(fp), 80))
	}
	return n, nil
}

// APIError is a non-2xx answer from the hive.
type APIError struct {
	Status  int    // HTTP status code
	Message string // the hive's error text, stripped of control characters
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("hive answered HTTP %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("hive: %s (HTTP %d)", e.Message, e.Status)
}

// IsNotFound reports whether err is a 404 from the hive.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// FingerprintError is returned when the hive presents a certificate other
// than the pinned one. It wraps auth.ErrFingerprintMismatch.
type FingerprintError struct {
	Hive   string // base URL
	Pinned string // expected fingerprint
	Seen   string // fingerprint the server presented
}

func (e *FingerprintError) Error() string {
	return fmt.Sprintf("%s presented certificate %s, but %s is pinned", e.Hive, e.Seen, e.Pinned)
}

// Unwrap lets errors.Is(err, auth.ErrFingerprintMismatch) match.
func (e *FingerprintError) Unwrap() error { return auth.ErrFingerprintMismatch }

// LoginResult describes a successful proof-based login.
type LoginResult struct {
	Hive         string    // base URL
	HiveID       string    // from /hello
	Version      string    // hive version from /hello
	Fingerprint  string    // verified certificate fingerprint
	Session      string    // bearer session
	Expires      time.Time // local clock
	FirstContact bool      // the fingerprint was learned by trust on first use
}

// Option configures a Client.
type Option func(*Client)

// WithAdminToken lets the client log in with the admin token when it has
// no valid session. The token itself is never sent to the hive.
func WithAdminToken(token string) Option {
	return func(c *Client) { c.token = strings.TrimSpace(token) }
}

// WithTokenSource is like WithAdminToken, but the token is requested only
// when a login is needed (for example by prompting). It should return an
// error wrapping ErrNotLoggedIn when no token is available.
func WithTokenSource(fn func() (string, error)) Option {
	return func(c *Client) { c.tokenFn = fn }
}

// WithSession starts the client with an existing session. expires is in
// local time; the zero time means unknown.
func WithSession(session string, expires time.Time) Option {
	return func(c *Client) { c.session, c.expires = session, expires }
}

// WithTOFU allows learning the fingerprint on first contact when none is
// pinned (default false). The learned fingerprint is only kept once the
// hive has proven that it knows the admin token.
func WithTOFU(allow bool) Option {
	return func(c *Client) { c.tofu = allow }
}

// WithTimeout sets the per-request timeout for API calls (default 30 s).
// Transfers of blobs and outputs fail only when no data moves for this long.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithFirstContact registers a callback that receives the fingerprint seen
// on first contact with an unpinned hive, before any login proof is sent.
func WithFirstContact(fn func(fingerprint string)) Option {
	return func(c *Client) { c.onFirstContact = fn }
}

// WithLoginCallback registers a callback run after every successful login,
// for example to persist the session and the verified fingerprint.
func WithLoginCallback(fn func(LoginResult)) Option {
	return func(c *Client) { c.onLogin = fn }
}

// WithLogger sets a logger for debug messages.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// Client talks to one hive's admin API. All methods are safe for
// concurrent use.
type Client struct {
	base           string
	timeout        time.Duration
	tofu           bool
	tokenFn        func() (string, error)
	onFirstContact func(string)
	onLogin        func(LoginResult)
	log            *slog.Logger
	now            func() time.Time

	loginMu sync.Mutex // serializes logins

	mu      sync.Mutex
	fp      string       // pinned fingerprint ("" = none yet)
	hc      *http.Client // pinned to fp
	session string
	expires time.Time
	token   string
	secret  auth.Secret // derived from token, lazily
}

// NewClient returns a client for the hive at base ("host", "host:port" or
// "https://host:port"). fingerprint pins the hive's certificate; if it is
// empty the client can only connect when WithTOFU(true) is given.
func NewClient(base, fingerprint string, opts ...Option) (*Client, error) {
	u, err := config.HiveURL(base)
	if err != nil {
		return nil, fmt.Errorf("invalid hive address %q: %w", truncate(sanitizeCell(base), 80), err)
	}
	c := &Client{
		base:    u,
		timeout: defaultTimeout,
		log:     slog.New(slog.DiscardHandler),
		now:     time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	if fingerprint != "" {
		fp, err := NormalizeFingerprint(fingerprint)
		if err != nil {
			return nil, err
		}
		c.fp = fp
		c.hc = c.pinnedHTTPClient(fp)
	}
	if c.session != "" && !validSession(c.session) {
		c.session, c.expires = "", time.Time{}
	}
	return c, nil
}

// Base returns the hive's base URL (https://host:port).
func (c *Client) Base() string { return c.base }

// Fingerprint returns the pinned certificate fingerprint, or "" if none is
// pinned yet.
func (c *Client) Fingerprint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fp
}

// Session returns the current session and its local expiry time.
func (c *Client) Session() (string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session, c.expires
}

// Close releases idle connections.
func (c *Client) Close() {
	c.mu.Lock()
	hc := c.hc
	c.mu.Unlock()
	if hc != nil {
		hc.CloseIdleConnections()
	}
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func newTransport(cfg *tls.Config) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		// The hive is on the LAN; a corporate proxy would only get in the way.
		Proxy:                 nil,
		DialContext:           d.DialContext,
		TLSClientConfig:       cfg,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
	}
}

// pinnedHTTPClient returns an HTTP client whose TLS handshake fails unless
// the server's certificate has fingerprint fp.
func (c *Client) pinnedHTTPClient(fp string) *http.Client {
	cfg := auth.ClientTLSConfig(fp, nil)
	inner := cfg.VerifyConnection
	base := c.base
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		err := inner(cs)
		if err != nil && errors.Is(err, auth.ErrFingerprintMismatch) && len(cs.PeerCertificates) > 0 {
			return &FingerprintError{Hive: base, Pinned: fp, Seen: auth.Fingerprint(cs.PeerCertificates[0].Raw)}
		}
		return err
	}
	return &http.Client{Transport: newTransport(cfg), CheckRedirect: noRedirects}
}

func validSession(s string) bool {
	if len(s) < 16 || len(s) > maxSessionLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// canLogin reports whether a login could be attempted.
func (c *Client) canLogin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token != "" || c.tokenFn != nil
}

// adminSecret returns the stretched admin secret, obtaining the token first
// if needed. Stretching is slow, so the result is cached.
func (c *Client) adminSecret() (auth.Secret, error) {
	c.mu.Lock()
	tok, fn, sec := c.token, c.tokenFn, c.secret
	c.mu.Unlock()
	if !sec.IsZero() {
		return sec, nil
	}
	if tok == "" {
		if fn == nil {
			return auth.Secret{}, ErrNotLoggedIn
		}
		t, err := fn()
		if err != nil {
			return auth.Secret{}, err
		}
		tok = strings.TrimSpace(t)
		if tok == "" {
			return auth.Secret{}, ErrNotLoggedIn
		}
	}
	sec = auth.NewAdminSecret(tok)
	c.mu.Lock()
	c.token, c.secret = tok, sec
	c.mu.Unlock()
	return sec, nil
}

// Hello fetches GET /api/v1/hello. It needs a pinned fingerprint, or trust
// on first use (in which case nothing is pinned by this call).
func (c *Client) Hello(ctx context.Context) (proto.Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	h, _, _, err := c.hello(ctx)
	return h, err
}

// hello returns the hive's hello, the fingerprint of the certificate it
// was received over, and whether that fingerprint was learned just now.
func (c *Client) hello(ctx context.Context) (proto.Hello, string, bool, error) {
	c.mu.Lock()
	fp, hc := c.fp, c.hc
	c.mu.Unlock()
	if fp != "" {
		h, err := c.getHello(ctx, hc)
		return h, fp, false, err
	}
	if !c.tofu {
		return proto.Hello{}, "", false, ErrNoFingerprint
	}
	var seenMu sync.Mutex
	var seen string
	tr := newTransport(auth.ClientTLSConfig("", func(f string) {
		seenMu.Lock()
		seen = f
		seenMu.Unlock()
	}))
	tr.DisableKeepAlives = true
	defer tr.CloseIdleConnections()
	h, err := c.getHello(ctx, &http.Client{Transport: tr, CheckRedirect: noRedirects})
	if err != nil {
		return proto.Hello{}, "", false, err
	}
	seenMu.Lock()
	fp = seen
	seenMu.Unlock()
	if fp == "" {
		return proto.Hello{}, "", false, errors.New("the hive presented no certificate")
	}
	if c.onFirstContact != nil {
		c.onFirstContact(fp)
	}
	return h, fp, true, nil
}

func (c *Client) getHello(ctx context.Context, hc *http.Client) (proto.Hello, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/hello", nil)
	if err != nil {
		return proto.Hello{}, err
	}
	setCommonHeaders(req)
	resp, err := hc.Do(req)
	if err != nil {
		return proto.Hello{}, c.transportError(err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return proto.Hello{}, readAPIError(resp)
	}
	var h proto.Hello
	if err := decodeJSONBody(resp.Body, maxHelloBody, &h); err != nil {
		return proto.Hello{}, fmt.Errorf("hello from %s: %w", c.base, err)
	}
	if h.APIVersion != proto.APIVersion {
		return proto.Hello{}, fmt.Errorf("the hive speaks API version %d but this savior speaks %d: use matching versions", h.APIVersion, proto.APIVersion)
	}
	if h.Nonce == "" || len(h.Nonce) > 256 {
		return proto.Hello{}, errors.New("the hive sent an invalid nonce")
	}
	return h, nil
}

// Login performs the proof-based admin login of DESIGN 6.3 and keeps the
// resulting session. The admin token is never sent: the hive gets a proof
// bound to its nonce, a fresh client nonce and the certificate fingerprint
// this client saw, and must answer with its own proof before the
// fingerprint and session are accepted.
func (c *Client) Login(ctx context.Context) (LoginResult, error) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.login(ctx)
}

func (c *Client) login(ctx context.Context) (LoginResult, error) {
	secret, err := c.adminSecret()
	if err != nil {
		return LoginResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	hello, fpSeen, first, err := c.hello(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	// Everything after /hello goes over a new client pinned to FP_seen, so a
	// proof never travels over a connection other than the one it is bound to.
	hc := c.pinnedHTTPClient(fpSeen)
	clientNonce := auth.NewNonce()
	body, err := json.Marshal(proto.LoginRequest{
		HiveNonce:   hello.Nonce,
		ClientNonce: clientNonce,
		Proof:       secret.AdminProof(hello.Nonce, clientNonce, fpSeen),
	})
	if err != nil {
		return LoginResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/admin/login", bytes.NewReader(body))
	if err != nil {
		return LoginResult{}, err
	}
	setCommonHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ClientTimeHeader, c.now().UTC().Format(time.RFC3339Nano))
	sent := c.now()
	resp, err := hc.Do(req)
	if err != nil {
		hc.CloseIdleConnections()
		return LoginResult{}, c.transportError(err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		hc.CloseIdleConnections()
		apiErr := readAPIError(resp)
		if resp.StatusCode == http.StatusForbidden {
			return LoginResult{}, fmt.Errorf("login rejected (wrong admin token?): %w", apiErr)
		}
		return LoginResult{}, fmt.Errorf("login: %w", apiErr)
	}
	var sr proto.SessionResponse
	if err := decodeJSONBody(resp.Body, maxHelloBody, &sr); err != nil {
		hc.CloseIdleConnections()
		return LoginResult{}, fmt.Errorf("login response: %w", err)
	}
	want := secret.HiveProof(hello.Nonce, clientNonce, "admin", fpSeen)
	if !auth.VerifyProof(want, sr.HiveProof) {
		hc.CloseIdleConnections()
		return LoginResult{}, ErrHiveProof
	}
	if !validSession(sr.Session) {
		hc.CloseIdleConnections()
		return LoginResult{}, errors.New("the hive returned an invalid session")
	}
	res := LoginResult{
		Hive:         c.base,
		HiveID:       sanitizeCell(hello.HiveID),
		Version:      sanitizeCell(hello.Version),
		Fingerprint:  fpSeen,
		Session:      sr.Session,
		Expires:      localExpiry(sent, hello.Time, sr.ExpiresAt),
		FirstContact: first,
	}
	c.mu.Lock()
	old := c.hc
	c.fp, c.hc = fpSeen, hc
	c.session, c.expires = res.Session, res.Expires
	c.mu.Unlock()
	if old != nil && old != hc {
		old.CloseIdleConnections()
	}
	c.log.Debug("logged in to the hive", "hive", c.base, "hive_id", res.HiveID, "fingerprint", fpSeen, "first_contact", first)
	if c.onLogin != nil {
		c.onLogin(res)
	}
	return res, nil
}

// localExpiry converts the hive's session expiry to the local clock. The
// hive's clock may be far off (dead CMOS battery), so the lifetime is
// measured against the hive's own hello time when that looks sane.
func localExpiry(localNow, hiveNow, hiveExpires time.Time) time.Time {
	const maxTTL = 7 * 24 * time.Hour
	if hiveExpires.IsZero() {
		return time.Time{}
	}
	if !hiveNow.IsZero() {
		if ttl := hiveExpires.Sub(hiveNow); ttl > 0 && ttl <= maxTTL {
			return localNow.Add(ttl)
		}
	}
	return hiveExpires
}

// authorization returns the session to send, logging in first when there
// is no usable one. fresh reports that the session was just obtained.
func (c *Client) authorization(ctx context.Context) (session string, fresh bool, err error) {
	usable := func() (string, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.session == "" {
			return "", false
		}
		return c.session, c.expires.IsZero() || c.now().Add(sessionMargin).Before(c.expires)
	}
	if s, ok := usable(); ok {
		return s, false, nil
	}
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	s, ok := usable()
	if ok {
		return s, false, nil
	}
	res, err := c.login(ctx)
	if err != nil {
		if s != "" && errors.Is(err, ErrNotLoggedIn) {
			// No token to log in again, and our idea of the expiry may be
			// off: let the hive judge the old session.
			return s, false, nil
		}
		return "", false, err
	}
	return res.Session, true, nil
}

// ensureSession logs in if needed before a timed request starts, so a
// token prompt or a slow key stretch does not eat into its timeout.
func (c *Client) ensureSession(ctx context.Context) error {
	_, _, err := c.authorization(ctx)
	return err
}

// dropSession forgets session if it is still the current one.
func (c *Client) dropSession(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == session {
		c.session, c.expires = "", time.Time{}
	}
}

func setCommonHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "savior-ctl/"+version.Version)
	req.Header.Set("Accept", "application/json")
}

// apiRequest describes one admin API call.
type apiRequest struct {
	method string
	path   string // escaped path starting with /api/v1/
	query  url.Values
	// body returns a fresh request body and its length (-1 = unknown) for
	// each attempt; nil means no body.
	body        func() (io.Reader, int64, error)
	contentType string
	expect100   bool // send Expect: 100-continue (uploads the hive may skip)
	noRelogin   bool // never log in for this call (logout)
}

// do sends an authenticated request and returns a 2xx response. A 401 for
// a stored session triggers one fresh login when a token is available.
func (c *Client) do(ctx context.Context, r apiRequest) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var session string
		var fresh bool
		var err error
		if r.noRelogin {
			c.mu.Lock()
			session = c.session
			c.mu.Unlock()
			if session == "" {
				return nil, ErrNotLoggedIn
			}
		} else if session, fresh, err = c.authorization(ctx); err != nil {
			if attempt > 0 && errors.Is(err, ErrNotLoggedIn) {
				return nil, ErrSessionExpired
			}
			return nil, err
		}
		c.mu.Lock()
		hc := c.hc
		c.mu.Unlock()
		if hc == nil {
			return nil, ErrNoFingerprint
		}
		var body io.Reader
		size := int64(0)
		if r.body != nil {
			if body, size, err = r.body(); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, r.method, c.base+r.path, body)
		if err != nil {
			return nil, err
		}
		if r.query != nil {
			req.URL.RawQuery = r.query.Encode()
		}
		if body != nil {
			req.ContentLength = size
			if size == 0 {
				req.Body = http.NoBody
			}
		}
		setCommonHeaders(req)
		req.Header.Set("Authorization", "Bearer "+session)
		req.Header.Set(ClientTimeHeader, c.now().UTC().Format(time.RFC3339Nano))
		if r.contentType != "" {
			req.Header.Set("Content-Type", r.contentType)
		}
		if r.expect100 && size != 0 {
			req.Header.Set("Expect", "100-continue")
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, c.transportError(err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && !fresh {
			drainClose(resp.Body)
			c.log.Debug("the hive refused the session", "path", r.path)
			c.dropSession(session)
			if !r.noRelogin && c.canLogin() {
				continue
			}
			return nil, ErrSessionExpired
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			defer drainClose(resp.Body)
			return nil, readAPIError(resp)
		}
		return resp, nil
	}
}

// transportError makes TLS pinning failures recognizable.
func (c *Client) transportError(err error) error {
	var fe *FingerprintError
	if errors.As(err, &fe) {
		return fe
	}
	return fmt.Errorf("cannot reach the hive at %s: %w", c.base, err)
}

func readAPIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := ""
	var er proto.ErrorResponse
	if json.Unmarshal(b, &er) == nil && er.Error != "" {
		msg = er.Error
	} else if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/plain") {
		msg = strings.TrimSpace(string(b))
	}
	return &APIError{Status: resp.StatusCode, Message: truncate(sanitizeCell(msg), 1024)}
}

func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 64<<10))
	_ = rc.Close()
}

// errTooLarge is returned when a response exceeds its size cap.
var errTooLarge = errors.New("response too large")

// capReader fails with errTooLarge instead of silently truncating.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, errTooLarge
	}
	return n, err
}

func decodeJSONBody(r io.Reader, max int64, out any) error {
	dec := json.NewDecoder(&capReader{r: r, left: max})
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid JSON from the hive: %w", err)
	}
	return nil
}

func jsonBody(v any) (func() (io.Reader, int64, error), error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return func() (io.Reader, int64, error) { return bytes.NewReader(b), int64(len(b)), nil }, nil
}

// callJSON sends in (if not nil) as JSON and decodes the answer into out
// (if not nil), within the client's request timeout.
func (c *Client) callJSON(ctx context.Context, method, path string, query url.Values, in, out any) error {
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	r := apiRequest{method: method, path: path, query: query}
	if in != nil {
		b, err := jsonBody(in)
		if err != nil {
			return err
		}
		r.body, r.contentType = b, "application/json"
	}
	resp, err := c.do(ctx, r)
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)
	if out == nil {
		return nil
	}
	return decodeJSONBody(resp.Body, maxJSONResponse, out)
}

// errStalled is the cancel cause of a transfer that made no progress.
var errStalled = errors.New("transfer stalled")

// stallContext returns a context that is canceled when kick is not called
// for the client's timeout, so large transfers have no overall deadline
// but still fail when they stop moving.
func (c *Client) stallContext(parent context.Context) (ctx context.Context, kick func(), stop func()) {
	ctx, cancel := context.WithCancelCause(parent)
	t := time.AfterFunc(c.timeout, func() { cancel(fmt.Errorf("%w: no data for %s", errStalled, c.timeout)) })
	return ctx, func() { t.Reset(c.timeout) }, func() { t.Stop(); cancel(nil) }
}

// stallErr prefers the stall cause over a bare "context canceled".
func stallErr(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil && errors.Is(cause, errStalled) {
		return cause
	}
	return err
}

// kickReader calls kick whenever data moves.
type kickReader struct {
	r    io.Reader
	kick func()
}

func (k *kickReader) Read(p []byte) (int, error) {
	n, err := k.r.Read(p)
	if n > 0 {
		k.kick()
	}
	return n, err
}

// escapePath joins escaped path segments under /api/v1/.
func escapePath(prefix string, segs ...string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, s := range segs {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

func adminPath(segs ...string) string { return escapePath("/api/v1/admin", segs...) }

// checkRef validates a user-supplied node ref, job, task or wall ID before
// it goes into a URL path.
func checkRef(kind, s string) error {
	if s == "" || s == "." || s == ".." || len(s) > 128 {
		return fmt.Errorf("invalid %s %q", kind, truncate(sanitizeCell(s), 80))
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || r == '/' || r == '\\' || r == '?' || r == '#' || r == '%' {
			return fmt.Errorf("invalid %s %q", kind, truncate(sanitizeCell(s), 80))
		}
	}
	return nil
}
