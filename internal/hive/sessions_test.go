package hive

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

func TestAdminLoginProof(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	login := func(sec auth.Secret, fp string) (int, proto.SessionResponse, proto.LoginRequest) {
		hl := h.hello()
		req := proto.LoginRequest{HiveNonce: hl.Nonce, ClientNonce: auth.NewNonce()}
		req.Proof = sec.AdminProof(req.HiveNonce, req.ClientNonce, fp)
		var resp proto.SessionResponse
		st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/login", "", req, &resp)
		return st, resp, req
	}
	st, resp, req := login(adminSecret(), h.s.Fingerprint())
	if st != 200 || resp.Session == "" || resp.ExpiresAt.Before(time.Now().Add(11*time.Hour)) {
		t.Fatalf("login: %d %+v", st, resp)
	}
	if want := adminSecret().HiveProof(req.HiveNonce, req.ClientNonce, "admin", h.s.Fingerprint()); !auth.VerifyProof(want, resp.HiveProof) {
		t.Fatal("hive proof does not verify")
	}
	if st, _ := do(t, h.hc, "GET", h.url+"/api/v1/admin/info", resp.Session, nil, nil); st != 200 {
		t.Fatalf("session bearer: %d", st)
	}
	// Replaying the login request fails (single-use nonce).
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/login", "", req, nil); st != http.StatusForbidden {
		t.Fatalf("login replay: %d", st)
	}
	// A proof over another certificate (MITM) or with the swarm key fails.
	if st, _, _ := login(adminSecret(), "sha256:"+strings.Repeat("00", 32)); st != http.StatusForbidden {
		t.Fatalf("login via other cert: %d", st)
	}
	if st, _, _ := login(swarmSecret(), h.s.Fingerprint()); st != http.StatusForbidden {
		t.Fatalf("login with the swarm key: %d", st)
	}
	// Logout revokes the session.
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/logout", resp.Session, nil, nil); st != 200 {
		t.Fatalf("logout: %d", st)
	}
	if st, _ := do(t, h.hc, "GET", h.url+"/api/v1/admin/info", resp.Session, nil, nil); st != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", st)
	}
}

func TestPairingCodeCookieAndCSRF(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	pc := h.s.PairCode()
	if len(pc.Code) != 8 || strings.Trim(pc.Code, crockfordUpper) != "" || h.s.PairCode().Code != pc.Code {
		t.Fatalf("screen code: %+v", pc)
	}
	// Codes are case-insensitive and dashes are ignored.
	typed := strings.ToLower(pc.Code[:4] + "-" + pc.Code[4:])
	req, _ := http.NewRequest("POST", h.url+"/api/v1/admin/session", strings.NewReader(`{"pair_code":"`+typed+`"}`))
	resp, err := h.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if resp.StatusCode != 200 || cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Fatalf("session cookie: %d %+v", resp.StatusCode, cookie)
	}
	// Single use; the screen shows a new code afterwards.
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/session", "", proto.SessionRequest{PairCode: pc.Code}, nil); st != http.StatusForbidden {
		t.Fatalf("reused pairing code: %d", st)
	}
	if h.s.PairCode().Code == pc.Code {
		t.Fatal("screen code not rotated after use")
	}

	withCookie := func(method, path string, hdr ...string) int {
		req, _ := http.NewRequest(method, h.url+"/api/v1/admin/"+path, strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie.Value})
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := h.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	host := strings.TrimPrefix(h.url, "https://")
	if st := withCookie("GET", "info"); st != 200 {
		t.Fatalf("cookie GET: %d", st)
	}
	if st := withCookie("POST", "identify"); st != http.StatusForbidden {
		t.Fatalf("cookie POST without X-Savior: %d", st)
	}
	if st := withCookie("POST", "identify", "X-Savior", "1", "Origin", "https://evil.example"); st != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d", st)
	}
	if st := withCookie("POST", "identify", "X-Savior", "1", "Origin", "https://"+host); st != 200 {
		t.Fatalf("same-origin POST: %d", st)
	}
	if st := withCookie("POST", "identify", "X-Savior", "1"); st != 200 {
		t.Fatalf("POST without Origin: %d", st)
	}
	// Admin-made pairing codes work once, too.
	var extra proto.PairCode
	h.mustAdmin("POST", "pair", nil, &extra)
	var sr proto.SessionResponse
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/session", "", proto.SessionRequest{PairCode: extra.Code}, &sr); st != 200 || sr.Session == "" {
		t.Fatalf("admin pairing code: %d", st)
	}
	// Revoke all sessions.
	if st := withCookie("POST", "sessions/revoke", "X-Savior", "1"); st != 200 {
		t.Fatalf("revoke: %d", st)
	}
	if st := withCookie("GET", "info"); st != http.StatusUnauthorized {
		t.Fatalf("cookie after revoke: %d", st)
	}
	if st, _ := do(t, h.hc, "GET", h.url+"/api/v1/admin/info", sr.Session, nil, nil); st != http.StatusUnauthorized {
		t.Fatalf("bearer session after revoke: %d", st)
	}
}

func TestPairAndTokenRateLimits(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	for i := 0; i < 5; i++ {
		if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/session", "", proto.SessionRequest{PairCode: "ZZZZZZZZ"}, nil); st != http.StatusForbidden {
			t.Fatalf("wrong code %d: %d", i, st)
		}
	}
	good := h.s.PairCode().Code
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/session", "", proto.SessionRequest{PairCode: good}, nil); st != http.StatusTooManyRequests {
		t.Fatalf("pair bucket not limited: %d", st)
	}
	// The token exchange uses the admin bucket, which is still open.
	var sr proto.SessionResponse
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/session", "", proto.SessionRequest{Token: testAdmin}, &sr); st != 200 {
		t.Fatalf("token exchange: %d", st)
	}
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/session", "", proto.SessionRequest{Token: testAdmin, PairCode: good}, nil); st != http.StatusBadRequest {
		t.Fatalf("both fields: %d", st)
	}
	for i := 0; i < 5; i++ {
		if st, _ := do(t, h.hc, "GET", h.url+"/api/v1/admin/nodes", "wrong-token", nil, nil); st != http.StatusUnauthorized {
			t.Fatalf("wrong bearer %d: %d", i, st)
		}
	}
	if st, _ := do(t, h.hc, "GET", h.url+"/api/v1/admin/nodes", testAdmin, nil, nil); st != http.StatusTooManyRequests {
		t.Fatalf("admin bucket not limited: %d", st)
	}
	// Missing credentials are 401 and don't count.
	h2 := newHive(t, nil)
	for i := 0; i < 8; i++ {
		if st, _ := do(t, h2.hc, "GET", h2.url+"/api/v1/admin/nodes", "", nil, nil); st != http.StatusUnauthorized {
			t.Fatalf("anonymous: %d", st)
		}
	}
	if st := h2.admin("GET", "nodes", nil, nil); st != 200 {
		t.Fatalf("anonymous requests counted as failures: %d", st)
	}
}

func TestSessionExpiry(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.tune.sessionTTL = 100 * time.Millisecond; c.tune.pairTTL = 100 * time.Millisecond })
	var sr proto.SessionResponse
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/session", "", proto.SessionRequest{Token: testAdmin}, &sr); st != 200 {
		t.Fatal(st)
	}
	pc := h.s.PairCode()
	time.Sleep(150 * time.Millisecond)
	if st, _ := do(t, h.hc, "GET", h.url+"/api/v1/admin/info", sr.Session, nil, nil); st != http.StatusUnauthorized {
		t.Fatalf("expired session: %d", st)
	}
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/admin/session", "", proto.SessionRequest{PairCode: pc.Code}, nil); st != http.StatusForbidden {
		t.Fatalf("expired pairing code: %d", st)
	}
	if h.s.PairCode().Code == pc.Code {
		t.Fatal("expired screen code not rotated")
	}
}
