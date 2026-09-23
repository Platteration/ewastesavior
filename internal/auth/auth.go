// Package auth implements the swarm-key join proofs, tokens, and the hive's
// self-signed TLS identity with fingerprint pinning. See docs/DESIGN.md
// section 6.
package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	nodeLabel  = "savior-node-v1"
	hiveLabel  = "savior-hive-v1"
	adminLabel = "savior-admin-v1"
	hintLabel  = "savior-swarm-hint-v2"

	// KDFIterations is the PBKDF2-SHA256 cost of stretching a swarm key or
	// admin token. About 1-2 s once per process on a Pentium M.
	KDFIterations = 200000
)

// Secret is a stretched shared secret (swarm key or admin token). Proofs
// and hints are computed from the stretched key, so an observed proof or
// beacon only allows slow offline guessing.
type Secret struct {
	k []byte
}

// NewSwarmSecret stretches a swarm key.
func NewSwarmSecret(swarmKey string) Secret { return stretch(swarmKey, "savior-swarm-v1") }

// NewAdminSecret stretches an admin token.
func NewAdminSecret(adminToken string) Secret { return stretch(adminToken, "savior-admin-v1") }

func stretch(pass, salt string) Secret {
	k, err := pbkdf2.Key(sha256.New, pass, []byte(salt), KDFIterations, 32)
	if err != nil {
		panic("auth: pbkdf2: " + err.Error())
	}
	return Secret{k: k}
}

// IsZero reports whether s was never initialized.
func (s Secret) IsZero() bool { return len(s.k) == 0 }

func (s Secret) mac(label string, parts ...string) string {
	h := hmac.New(sha256.New, s.k)
	h.Write([]byte(label))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NodeProof is the node's proof of swarm-key possession, bound to the hive
// nonce, its own nonce, its node ID and the TLS fingerprint it observed.
func (s Secret) NodeProof(hiveNonce, nodeNonce, nodeID, fingerprint string) string {
	return s.mac(nodeLabel, hiveNonce, nodeNonce, nodeID, fingerprint)
}

// HiveProof is the hive's proof of swarm-key possession, bound to the same
// values and the hive's own certificate fingerprint.
func (s Secret) HiveProof(hiveNonce, nodeNonce, nodeID, fingerprint string) string {
	return s.mac(hiveLabel, hiveNonce, nodeNonce, nodeID, fingerprint)
}

// AdminProof is `savior ctl`'s login proof (s = NewAdminSecret(token)); the
// hive answers with HiveProof computed from the same admin secret.
func (s Secret) AdminProof(hiveNonce, clientNonce, fingerprint string) string {
	return s.mac(adminLabel, hiveNonce, clientNonce, fingerprint)
}

// SwarmHint is the 8-hex-char (32-bit) swarm identifier broadcast in
// beacons: enough to tell swarms apart on a LAN, useless on its own for
// confirming a key guess.
func (s Secret) SwarmHint() string {
	return s.mac(hintLabel)[:8]
}

// VerifyProof compares two hex proofs in constant time.
func VerifyProof(expected, got string) bool {
	return len(expected) == len(got) && subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}

// NonceIssuer makes stateless hive nonces: hex(unix time (8 bytes) ||
// random (16) || HMAC(process key, time||random)[:8]). Check verifies the
// MAC and age; single use is enforced separately by the caller.
type NonceIssuer struct{ key []byte }

// NewNonceIssuer returns an issuer with a fresh random key.
func NewNonceIssuer() *NonceIssuer {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return &NonceIssuer{key: k}
}

// Issue returns a new nonce stamped with now.
func (n *NonceIssuer) Issue(now time.Time) string {
	b := make([]byte, 32)
	binary.BigEndian.PutUint64(b[:8], uint64(now.Unix()))
	if _, err := rand.Read(b[8:24]); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	h := hmac.New(sha256.New, n.key)
	h.Write(b[:24])
	copy(b[24:], h.Sum(nil)[:8])
	return hex.EncodeToString(b)
}

// Check reports whether nonce was issued by n and is at most ttl old.
func (n *NonceIssuer) Check(nonce string, now time.Time, ttl time.Duration) bool {
	b, err := hex.DecodeString(nonce)
	if err != nil || len(b) != 32 {
		return false
	}
	h := hmac.New(sha256.New, n.key)
	h.Write(b[:24])
	if !hmac.Equal(b[24:], h.Sum(nil)[:8]) {
		return false
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(b[:8])), 0)
	age := now.Sub(issued)
	return age >= -5*time.Second && age <= ttl
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewToken returns 32 random bytes as hex.
func NewToken() string { return randHex(32) }

// NewNonce returns 32 random bytes as hex.
func NewNonce() string { return randHex(32) }

// NewID returns n random bytes as hex (for hive/job/task IDs).
func NewID(n int) string { return randHex(n) }

// HashToken returns the hex SHA-256 of a token (tokens are stored hashed).
func HashToken(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

// TokenEqual compares a presented token with a stored hash in constant time.
func TokenEqual(storedHash, presented string) bool {
	return VerifyProof(storedHash, HashToken(presented))
}

// Fingerprint returns "sha256:<hex>" of a DER certificate.
func Fingerprint(der []byte) string {
	s := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(s[:])
}

// BearerToken extracts the token from an "Authorization: Bearer x" header value.
func BearerToken(header string) (string, bool) {
	const p = "bearer "
	if len(header) > len(p) && strings.EqualFold(header[:len(p)], p) {
		t := strings.TrimSpace(header[len(p):])
		return t, t != ""
	}
	return "", false
}

// LoadOrCreateCert loads cert.pem/key.pem from dir, creating a fresh ECDSA
// P-256 self-signed certificate (20-year validity) if they don't exist.
func LoadOrCreateCert(dir string) (tls.Certificate, string, error) {
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return c, Fingerprint(c.Certificate[0]), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(certPath); statErr == nil {
			return tls.Certificate{}, "", fmt.Errorf("load hive certificate: %w", err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM, keyPEM, err := GenerateCert("savior-hive")
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return c, Fingerprint(c.Certificate[0]), nil
}

// GenerateCert returns PEM-encoded certificate and private key.
func GenerateCert(cn string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"SaviorOS"}},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

// ErrFingerprintMismatch is returned (wrapped) when a pinned connection sees
// a different certificate.
var ErrFingerprintMismatch = errors.New("hive certificate fingerprint mismatch")

// ClientTLSConfig returns a client config that never uses CA verification
// or certificate dates. With pin != "" the handshake fails unless the leaf
// certificate's fingerprint equals pin. With pin == "" any certificate is
// accepted (trust on first use) and seen is called with its fingerprint.
func ClientTLSConfig(pin string, seen func(fp string)) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // verification is done by fingerprint below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("hive presented no certificate")
			}
			fp := Fingerprint(cs.PeerCertificates[0].Raw)
			if pin != "" && !VerifyProof(pin, fp) {
				return fmt.Errorf("%w: expected %s, got %s", ErrFingerprintMismatch, pin, fp)
			}
			if seen != nil {
				seen(fp)
			}
			return nil
		},
	}
}

// ServerTLSConfig returns the hive's server config.
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
