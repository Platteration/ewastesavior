package auth

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	secretA = NewSwarmSecret("0123456789abcdef-key")
	secretB = NewSwarmSecret("another-key-000000")
)

func TestProofsDiffer(t *testing.T) {
	n := secretA.NodeProof("hn", "nn", "id", "fp")
	h := secretA.HiveProof("hn", "nn", "id", "fp")
	if n == h {
		t.Fatal("node and hive proofs must differ (reflection)")
	}
	if !VerifyProof(n, secretA.NodeProof("hn", "nn", "id", "fp")) {
		t.Fatal("deterministic proof mismatch")
	}
	if secretA.NodeProof("a", "bc", "id", "fp") == secretA.NodeProof("ab", "c", "id", "fp") {
		t.Fatal("concatenation ambiguity")
	}
	if VerifyProof(n, secretA.NodeProof("hn", "nn", "id", "other-fp")) {
		t.Fatal("proof not bound to fingerprint")
	}
	if VerifyProof(n, secretB.NodeProof("hn", "nn", "id", "fp")) {
		t.Fatal("proof not bound to key")
	}
	if VerifyProof(n, n[:10]) {
		t.Fatal("length mismatch accepted")
	}
	admin := NewAdminSecret("admin-token-0000000000")
	if admin.AdminProof("hn", "cn", "fp") == secretA.AdminProof("hn", "cn", "fp") {
		t.Fatal("admin proof not bound to token")
	}
}

func TestSwarmHint(t *testing.T) {
	a, b := secretA.SwarmHint(), secretB.SwarmHint()
	if len(a) != 8 || a == b {
		t.Fatalf("bad hints %q %q", a, b)
	}
	if (Secret{}).IsZero() != true || secretA.IsZero() {
		t.Fatal("IsZero")
	}
}

func TestNonces(t *testing.T) {
	n := NewNonceIssuer()
	now := time.Unix(1_800_000_000, 0)
	x := n.Issue(now)
	if !n.Check(x, now.Add(30*time.Second), time.Minute) {
		t.Fatal("fresh nonce rejected")
	}
	if n.Check(x, now.Add(2*time.Minute), time.Minute) {
		t.Fatal("expired nonce accepted")
	}
	if NewNonceIssuer().Check(x, now, time.Minute) {
		t.Fatal("foreign nonce accepted")
	}
	bad := []byte(x)
	bad[20] ^= 1
	if n.Check(string(bad), now, time.Minute) {
		t.Fatal("tampered nonce accepted")
	}
	if n.Issue(now) == x {
		t.Fatal("nonces repeat")
	}
}

func TestTokens(t *testing.T) {
	tok := NewToken()
	if len(tok) != 64 || tok == NewToken() {
		t.Fatal("tokens not random/64 hex")
	}
	h := HashToken(tok)
	if !TokenEqual(h, tok) || TokenEqual(h, tok+"x") {
		t.Fatal("TokenEqual wrong")
	}
	if v, ok := BearerToken("Bearer abc"); !ok || v != "abc" {
		t.Fatal("bearer parse")
	}
	if _, ok := BearerToken("Basic abc"); ok {
		t.Fatal("basic accepted")
	}
	if _, ok := BearerToken("Bearer "); ok {
		t.Fatal("empty bearer accepted")
	}
}

func TestCertPersistenceAndPinning(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	cert, fp, err := LoadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert2, fp2, err := LoadOrCreateCert(dir)
	if err != nil || fp2 != fp || len(cert2.Certificate) == 0 {
		t.Fatalf("reload changed cert: %v %s %s", err, fp, fp2)
	}
	if st, _ := os.Stat(filepath.Join(dir, "key.pem")); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	srv.TLS = ServerTLSConfig(cert)
	srv.StartTLS()
	defer srv.Close()

	get := func(cfg *tls.Config) error {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := c.Get(srv.URL)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	var seen string
	if err := get(ClientTLSConfig("", func(f string) { seen = f })); err != nil || seen != fp {
		t.Fatalf("TOFU: %v seen=%s want %s", err, seen, fp)
	}
	if err := get(ClientTLSConfig(fp, nil)); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	err = get(ClientTLSConfig("sha256:"+strings.Repeat("0", 64), nil))
	if err == nil || !errors.Is(err, ErrFingerprintMismatch) && !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("wrong pin accepted: %v", err)
	}
}

func TestCorruptCertIsAnError(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("garbage"), 0o644)
	os.WriteFile(filepath.Join(dir, "key.pem"), []byte("garbage"), 0o600)
	if _, _, err := LoadOrCreateCert(dir); err == nil {
		t.Fatal("corrupt cert silently replaced")
	}
}
