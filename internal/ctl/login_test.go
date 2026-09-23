package ctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
)

func TestLoginPinsFingerprintAndStoresSession(t *testing.T) {
	h := newFakeHive(t)
	e := newCtlEnv(t)
	e.env[EnvAdminToken] = h.token
	code, stdout, stderr := e.run("login", "--hive", h.url)
	if code != 0 {
		t.Fatalf("login: code %d, stderr %s", code, stderr)
	}
	if !strings.Contains(stderr, "first contact") || !strings.Contains(stderr, h.fingerprint()) {
		t.Errorf("first contact warning missing the fingerprint:\n%s", stderr)
	}
	if !strings.Contains(stdout, h.fingerprint()) {
		t.Errorf("login output does not show the pinned fingerprint:\n%s", stdout)
	}
	fc := e.readConfig()
	want, _ := config.HiveURL(h.url)
	if fc.Hive != want || fc.Fingerprint != h.fingerprint() || fc.Session == "" {
		t.Fatalf("ctl.json = %+v, want hive %s and fingerprint %s with a session", fc, want, h.fingerprint())
	}
	if d := time.Until(fc.SessionExpires); d < 11*time.Hour || d > 13*time.Hour {
		t.Errorf("session expires in %v, want about 12h", d)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(e.cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("ctl.json mode %v, want 0600", st.Mode().Perm())
		}
		dst, err := os.Stat(filepath.Dir(e.cfgPath))
		if err != nil {
			t.Fatal(err)
		}
		if dst.Mode().Perm() != 0o700 {
			t.Errorf("config dir mode %v, want 0700", dst.Mode().Perm())
		}
	}

	// A later run without any token uses the stored session.
	delete(e.env, EnvAdminToken)
	before := h.requestCount()
	code, stdout, stderr = e.run("nodes")
	if code != 0 {
		t.Fatalf("nodes: code %d, stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "lobby-1") {
		t.Errorf("nodes output:\n%s", stdout)
	}
	if h.loginCount() != 1 {
		t.Errorf("logins = %d, want 1 (session reused)", h.loginCount())
	}
	reqs := h.allRequests()[before:]
	if len(reqs) != 1 || reqs[0].Path != "/api/v1/admin/nodes" {
		t.Fatalf("requests = %+v", reqs)
	}
	if got := reqs[0].Header.Get("Authorization"); got != "Bearer "+fc.Session {
		t.Errorf("Authorization = %q, want the stored session", got)
	}
	ct, err := time.Parse(time.RFC3339Nano, reqs[0].Header.Get(ClientTimeHeader))
	if err != nil || time.Since(ct).Abs() > time.Minute {
		t.Errorf("%s = %q (%v)", ClientTimeHeader, reqs[0].Header.Get(ClientTimeHeader), err)
	}
	assertTokenNeverSent(t, h)
}

func TestLoginRequestCarriesOnlyTheProof(t *testing.T) {
	h := newFakeHive(t)
	e := newCtlEnv(t)
	e.env[EnvAdminToken] = h.token
	if code, _, stderr := e.run("login", "--hive", h.url); code != 0 {
		t.Fatalf("login: %s", stderr)
	}
	var login *recordedRequest
	for _, r := range h.allRequests() {
		if r.Path == "/api/v1/admin/login" {
			login = &r
		}
		if (r.Path == "/api/v1/admin/login" || r.Path == "/api/v1/hello") && r.Header.Get("Authorization") != "" {
			t.Errorf("%s sent an Authorization header", r.Path)
		}
	}
	if login == nil {
		t.Fatal("no login request")
	}
	body := string(login.Body)
	for _, field := range []string{`"hive_nonce"`, `"client_nonce"`, `"proof"`} {
		if !strings.Contains(body, field) {
			t.Errorf("login body lacks %s: %s", field, body)
		}
	}
	if strings.Contains(body, h.token) {
		t.Fatal("login body contains the admin token")
	}
	assertTokenNeverSent(t, h)
}

func TestFingerprintChangeIsRefused(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	oldFP := h.fingerprint()
	h.setNewCert()
	newFP := h.fingerprint()
	before := h.requestCount()

	code, _, stderr := e.run("nodes")
	if code != 1 {
		t.Fatalf("code %d, want 1; stderr %s", code, stderr)
	}
	for _, want := range []string{oldFP, newFP, "does not match the pinned fingerprint", "--fingerprint " + newFP} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	// Even with the token available nothing may be sent: no hello, no proof.
	e.env[EnvAdminToken] = h.token
	if code, _, _ := e.run("login"); code != 1 {
		t.Fatalf("login to a changed certificate succeeded")
	}
	if n := h.requestCount(); n != before {
		t.Fatalf("%d requests reached the hive with a different certificate", n-before)
	}
	if fc := e.readConfig(); fc.Fingerprint != oldFP {
		t.Fatalf("pin changed to %s", fc.Fingerprint)
	}
	// The operator re-pins explicitly after checking the hive's screen.
	if code, _, stderr := e.run("login", "--fingerprint", newFP); code != 0 {
		t.Fatalf("explicit re-pin failed: %s", stderr)
	}
	if fc := e.readConfig(); fc.Fingerprint != newFP {
		t.Fatalf("pin = %s, want %s", fc.Fingerprint, newFP)
	}
	assertTokenNeverSent(t, h)
}

func TestClientFingerprintErrorType(t *testing.T) {
	h := newFakeHive(t)
	other := "sha256:" + strings.Repeat("ab", 32)
	c, err := NewClient(h.url, other, WithAdminToken(h.token))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Login(context.Background())
	var fe *FingerprintError
	if !errors.As(err, &fe) || !errors.Is(err, auth.ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want FingerprintError", err)
	}
	if fe.Pinned != other || fe.Seen != h.fingerprint() {
		t.Errorf("FingerprintError = %+v", fe)
	}
	if h.requestCount() != 0 {
		t.Fatal("a request reached the hive over a mismatched certificate")
	}
}

func TestLoginRejectsHiveWithoutProof(t *testing.T) {
	h := newFakeHive(t)
	h.badHiveProof = true
	e := newCtlEnv(t)
	e.env[EnvAdminToken] = h.token
	code, _, stderr := e.run("login", "--hive", h.url)
	if code != 1 || !strings.Contains(stderr, "could not prove") {
		t.Fatalf("code %d, stderr %s", code, stderr)
	}
	if _, err := os.Stat(e.cfgPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ctl.json was written after a failed hive proof (%v)", err)
	}
}

func TestLoginWrongToken(t *testing.T) {
	h := newFakeHive(t)
	e := newCtlEnv(t)
	e.env[EnvAdminToken] = "definitely-not-the-admin-token-0123456789"
	code, _, stderr := e.run("login", "--hive", h.url)
	if code != 1 || !strings.Contains(stderr, "login rejected") {
		t.Fatalf("code %d, stderr %s", code, stderr)
	}
	if _, err := os.Stat(e.cfgPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ctl.json written after a rejected login")
	}
}

func TestTOFUCanBeDisabled(t *testing.T) {
	h := newFakeHive(t)
	e := newCtlEnv(t)
	e.env[EnvAdminToken] = h.token
	code, _, stderr := e.run("--insecure-tofu=false", "login", "--hive", h.url)
	if code != 1 || !strings.Contains(stderr, "no certificate fingerprint is pinned") {
		t.Fatalf("code %d, stderr %s", code, stderr)
	}
	if h.requestCount() != 0 {
		t.Fatal("contacted the hive although trust on first use is off")
	}
	code, _, stderr = e.run("--insecure-tofu=false", "login", "--hive", h.url, "--fingerprint", strings.ToUpper(h.fingerprint()))
	if code != 0 {
		t.Fatalf("login with explicit pin: %s", stderr)
	}
	if strings.Contains(stderr, "first contact") {
		t.Error("warned about first contact although the fingerprint was given")
	}
}

func TestExplicitWrongFingerprintSendsNothing(t *testing.T) {
	h := newFakeHive(t)
	e := newCtlEnv(t)
	e.env[EnvAdminToken] = h.token
	e.env[EnvFingerprint] = "sha256:" + strings.Repeat("0", 64)
	code, _, stderr := e.run("login", "--hive", h.url)
	if code != 1 || !strings.Contains(stderr, "pinned") {
		t.Fatalf("code %d, stderr %s", code, stderr)
	}
	if h.requestCount() != 0 {
		t.Fatal("contacted a hive with the wrong certificate")
	}
}

func TestExpiredSessionLogsInAgainWithToken(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	h.revokeSessions()

	code, _, stderr := e.run("nodes")
	if code != 1 || !strings.Contains(stderr, "session expired or was revoked") || !strings.Contains(stderr, "savior ctl login") {
		t.Fatalf("without a token: code %d, stderr %s", code, stderr)
	}
	e.env[EnvAdminToken] = h.token
	code, _, stderr = e.run("nodes")
	if code != 0 {
		t.Fatalf("with a token: code %d, stderr %s", code, stderr)
	}
	if h.loginCount() != 2 {
		t.Errorf("logins = %d, want 2", h.loginCount())
	}
	fc := e.readConfig()
	h.mu.Lock()
	_, live := h.sessions[fc.Session]
	h.mu.Unlock()
	if !live {
		t.Error("the new session was not saved")
	}
	assertTokenNeverSent(t, h)
}

func TestLoginPromptsForToken(t *testing.T) {
	h := newFakeHive(t)
	e := newCtlEnv(t)
	e.stdinTTY = true
	e.secret = h.token + "\n"
	code, _, stderr := e.run("login", "--hive", h.url)
	if code != 0 || !strings.Contains(stderr, "Admin token for") {
		t.Fatalf("code %d, stderr %s", code, stderr)
	}

	// Not a terminal: login reads a line from stdin, with a notice.
	e2 := newCtlEnv(t)
	e2.stdin = h.token + "\r\n"
	code, _, stderr = e2.run("login", "--hive", h.url)
	if code != 0 || !strings.Contains(stderr, "standard input") {
		t.Fatalf("stdin login: code %d, stderr %s", code, stderr)
	}

	// Other commands never read a token from a non-terminal stdin.
	e3 := newCtlEnv(t)
	e3.stdin = h.token + "\n"
	code, _, stderr = e3.run("--hive", h.url, "nodes")
	if code != 1 || !strings.Contains(stderr, "not logged in") {
		t.Fatalf("nodes without login: code %d, stderr %s", code, stderr)
	}
	// ... unless asked to with --token -.
	code, _, stderr = e3.run("--hive", h.url, "--token", "-", "nodes")
	if code != 0 {
		t.Fatalf("--token -: code %d, stderr %s", code, stderr)
	}
	assertTokenNeverSent(t, h)
}

func TestOneOffHiveDoesNotReplaceRememberedHive(t *testing.T) {
	h1 := newFakeHive(t)
	h2 := newFakeHive(t)
	e := loggedIn(t, h1)
	first := e.readConfig()
	e.env[EnvAdminToken] = h2.token
	if code, _, stderr := e.run("--hive", h2.url, "nodes"); code != 0 {
		t.Fatalf("one-off hive: %s", stderr)
	}
	if got := e.readConfig(); got != first {
		t.Fatalf("ctl.json changed by a one-off --hive: %+v", got)
	}
	// The stored session of h1 must never be offered to h2.
	for _, r := range h2.allRequests() {
		if strings.Contains(r.Header.Get("Authorization"), first.Session) {
			t.Fatal("h1's session was sent to h2")
		}
	}
	// An explicit login switches the remembered hive.
	if code, _, stderr := e.run("login", "--hive", h2.url); code != 0 {
		t.Fatalf("login h2: %s", stderr)
	}
	if got := e.readConfig(); got.Fingerprint != h2.fingerprint() {
		t.Fatalf("after login to h2: %+v", got)
	}
}

func TestLogout(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	sess := e.readConfig().Session
	if code, _, stderr := e.run("logout"); code != 0 {
		t.Fatalf("logout: %s", stderr)
	}
	h.mu.Lock()
	_, live := h.sessions[sess]
	h.mu.Unlock()
	if live {
		t.Error("session still valid on the hive")
	}
	fc := e.readConfig()
	if fc.Session != "" || fc.Fingerprint != h.fingerprint() {
		t.Errorf("after logout ctl.json = %+v (want pin kept, session gone)", fc)
	}
	if code, stdout, _ := e.run("logout"); code != 0 || !strings.Contains(stdout, "Not logged in") {
		t.Errorf("second logout: %d %q", code, stdout)
	}
}

func TestLocalExpiryCorrectsHiveClock(t *testing.T) {
	local := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	hiveNow := time.Date(2003, 1, 1, 0, 0, 0, 0, time.UTC) // dead CMOS battery
	got := localExpiry(local, hiveNow, hiveNow.Add(12*time.Hour))
	if !got.Equal(local.Add(12 * time.Hour)) {
		t.Errorf("localExpiry = %v", got)
	}
	if got := localExpiry(local, time.Time{}, local.Add(time.Hour)); !got.Equal(local.Add(time.Hour)) {
		t.Errorf("without hello time: %v", got)
	}
	if got := localExpiry(local, hiveNow, time.Time{}); !got.IsZero() {
		t.Errorf("zero expiry: %v", got)
	}
}

func TestSkewedHiveClockSessionStillUsable(t *testing.T) {
	h := newFakeHive(t)
	h.hiveClockSkew = -20 * 365 * 24 * time.Hour
	e := loggedIn(t, h)
	fc := e.readConfig()
	if time.Until(fc.SessionExpires) < 11*time.Hour {
		t.Fatalf("session expiry %v is not in local time", fc.SessionExpires)
	}
	if code, _, stderr := e.run("nodes"); code != 0 || h.loginCount() != 1 {
		t.Fatalf("code %d logins %d: %s", code, h.loginCount(), stderr)
	}
}

func TestClientLibraryDefaultsToStrictPinning(t *testing.T) {
	h := newFakeHive(t)
	c, err := NewClient(h.url, "", WithAdminToken(h.token))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Nodes(context.Background()); !errors.Is(err, ErrNoFingerprint) {
		t.Fatalf("err = %v, want ErrNoFingerprint", err)
	}
	var seen string
	var saved LoginResult
	c, err = NewClient(h.url, "", WithAdminToken(h.token), WithTOFU(true),
		WithFirstContact(func(fp string) { seen = fp }), WithLoginCallback(func(r LoginResult) { saved = r }))
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := c.Nodes(context.Background())
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %v, %v", nodes, err)
	}
	if seen != h.fingerprint() || saved.Fingerprint != h.fingerprint() || !saved.FirstContact || c.Fingerprint() != h.fingerprint() {
		t.Errorf("seen %q, saved %+v, pinned %q", seen, saved, c.Fingerprint())
	}
	assertTokenNeverSent(t, h)
}

func TestLocallyExpiredSessionIsStillOffered(t *testing.T) {
	h := newFakeHive(t)
	e := loggedIn(t, h)
	fc := e.readConfig()
	fc.SessionExpires = time.Now().Add(-time.Hour) // our clock thinks it expired
	if err := saveFileConfig(e.cfgPath, fc); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := e.run("nodes"); code != 0 {
		t.Fatalf("the hive still accepts the session, but ctl gave up: %s", stderr)
	}
	if h.loginCount() != 1 {
		t.Errorf("logins = %d", h.loginCount())
	}
}

func TestConcurrentCallsShareOneLogin(t *testing.T) {
	h := newFakeHive(t)
	c, err := NewClient(h.url, h.fingerprint(), WithAdminToken(h.token))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := c.Nodes(context.Background())
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := h.loginCount(); n != 1 {
		t.Errorf("logins = %d, want 1", n)
	}
}
