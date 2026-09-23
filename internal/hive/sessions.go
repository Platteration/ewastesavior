package hive

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// sessionValid reports whether a session hash is live.
func (s *Server) sessionValid(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[hash]
	return sess != nil && time.Now().Before(sess.expires)
}

// newSessionLocked creates a 12-hour admin session.
func (s *Server) newSessionLocked() proto.SessionResponse {
	tok := auth.NewToken()
	ttl := s.cfg.tune.sessionTTL
	sess := &session{expires: time.Now().Add(ttl), expiresWall: s.now().Add(ttl)}
	if len(s.sessions) >= maxSessions {
		s.expireSessionsLocked(time.Now())
		for len(s.sessions) >= maxSessions {
			var oldest string
			for k, v := range s.sessions {
				if oldest == "" || v.expires.Before(s.sessions[oldest].expires) {
					oldest = k
				}
			}
			delete(s.sessions, oldest)
		}
	}
	s.sessions[auth.HashToken(tok)] = sess
	return proto.SessionResponse{Session: tok, ExpiresAt: sess.expiresWall}
}

func (s *Server) expireSessionsLocked(now time.Time) {
	for k, v := range s.sessions {
		if !now.Before(v.expires) {
			delete(s.sessions, k)
		}
	}
}

// handleLogin is the proof-based login of `savior ctl` (DESIGN 6.3): the
// admin token itself is never sent.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	src := sourceKey(r.RemoteAddr)
	if s.limits.blocked(bucketAdmin, src) {
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts; try again in a minute")
		return
	}
	var req proto.LoginRequest
	if !decodeJSON(w, r, &req, true) {
		return
	}
	if req.ClientNonce == "" || len(req.ClientNonce) > 128 || len(req.Proof) > 128 {
		writeErr(w, http.StatusBadRequest, "invalid client_nonce or proof")
		return
	}
	if !s.consumeNonce(req.HiveNonce) {
		s.limits.fail(bucketAdmin, src)
		writeErr(w, http.StatusForbidden, "invalid, expired or already used hive nonce")
		return
	}
	want := s.admin.AdminProof(req.HiveNonce, req.ClientNonce, s.fp)
	if !auth.VerifyProof(want, req.Proof) {
		s.limits.fail(bucketAdmin, src)
		writeErr(w, http.StatusForbidden, "login rejected: wrong admin token or certificate mismatch")
		return
	}
	s.mu.Lock()
	resp := s.newSessionLocked()
	s.mu.Unlock()
	resp.HiveProof = s.admin.HiveProof(req.HiveNonce, req.ClientNonce, "admin", s.fp)
	writeJSON(w, http.StatusOK, resp)
}

// handleSession exchanges a pairing code or the admin token for a session
// and sets the browser cookie.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	src := sourceKey(r.RemoteAddr)
	var req proto.SessionRequest
	if !decodeJSON(w, r, &req, true) {
		return
	}
	if (req.PairCode == "") == (req.Token == "") {
		writeErr(w, http.StatusBadRequest, "send exactly one of pair_code or token")
		return
	}
	if req.PairCode != "" {
		if s.limits.blocked(bucketPair, src) {
			writeErr(w, http.StatusTooManyRequests, "too many wrong pairing codes; try again in a minute")
			return
		}
		if !s.usePairCode(req.PairCode) {
			s.limits.fail(bucketPair, src)
			writeErr(w, http.StatusForbidden, "wrong or expired pairing code")
			return
		}
	} else {
		if s.limits.blocked(bucketAdmin, src) {
			writeErr(w, http.StatusTooManyRequests, "too many failed attempts; try again in a minute")
			return
		}
		if !auth.TokenEqual(s.adminTokenHash, req.Token) {
			s.limits.fail(bucketAdmin, src)
			writeErr(w, http.StatusForbidden, "wrong admin token")
			return
		}
	}
	s.mu.Lock()
	resp := s.newSessionLocked()
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    resp.Session,
		Path:     "/",
		MaxAge:   int(s.cfg.tune.sessionTTL / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, a adminCtx) {
	if a.sessionHash != "" {
		s.mu.Lock()
		delete(s.sessions, a.sessionHash)
		s.mu.Unlock()
	}
	clearCookie(w)
	writeOK(w)
}

func (s *Server) handleRevokeAll(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	n := len(s.sessions)
	s.sessions = map[string]*session{}
	s.mu.Unlock()
	s.log.Info("all admin sessions revoked", "count", n)
	clearCookie(w)
	writeOK(w)
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	s.mu.Lock()
	pc := s.newPairCodeLocked(false)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, pc)
}

// newPairCodeLocked makes a single-use 8-character code valid 10 minutes.
// Only the screen code keeps its plaintext; others are stored hashed.
func (s *Server) newPairCodeLocked(screen bool) proto.PairCode {
	code := randomCode(8)
	ttl := s.cfg.tune.pairTTL
	pc := &pairCode{hash: auth.HashToken(code), expires: time.Now().Add(ttl), expiresWall: s.now().Add(ttl)}
	if screen {
		pc.code = code
		s.screenCode = pc
	}
	s.pairCodes = append(s.pairCodes, pc)
	if len(s.pairCodes) > maxPairCodes {
		drop := s.pairCodes[0]
		s.pairCodes = s.pairCodes[1:]
		if drop == s.screenCode {
			s.screenCode = nil
		}
	}
	return proto.PairCode{Code: code, ExpiresAt: pc.expiresWall}
}

// normalizePairCode applies Crockford decoding rules: case-insensitive,
// dashes and spaces ignored, O=0 and I/L=1.
func normalizePairCode(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	var b strings.Builder
	for _, r := range c {
		switch r {
		case '-', ' ':
		case 'O':
			b.WriteByte('0')
		case 'I', 'L':
			b.WriteByte('1')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// usePairCode consumes a matching, unexpired code (constant-time compare of
// hashes against every outstanding code).
func (s *Server) usePairCode(code string) bool {
	code = normalizePairCode(code)
	if len(code) != 8 {
		return false
	}
	h := auth.HashToken(code)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	match := -1
	for i, pc := range s.pairCodes {
		if subtle.ConstantTimeCompare([]byte(pc.hash), []byte(h)) == 1 && now.Before(pc.expires) {
			match = i
		}
	}
	if match < 0 {
		return false
	}
	pc := s.pairCodes[match]
	s.pairCodes = append(s.pairCodes[:match:match], s.pairCodes[match+1:]...)
	if pc == s.screenCode {
		s.screenCode = nil // rotated on next PairCode()
	}
	return true
}

// PairCode returns the pairing code shown on the hive's own screen. It is
// replaced once used or expired.
func (s *Server) PairCode() proto.PairCode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.screenPairCodeLocked()
}

func (s *Server) screenPairCodeLocked() proto.PairCode {
	if pc := s.screenCode; pc != nil && time.Now().Before(pc.expires) {
		return proto.PairCode{Code: pc.code, ExpiresAt: pc.expiresWall}
	}
	return s.newPairCodeLocked(true)
}

func (s *Server) expirePairCodesLocked(now time.Time) {
	kept := s.pairCodes[:0:0]
	for _, pc := range s.pairCodes {
		if now.Before(pc.expires) {
			kept = append(kept, pc)
		} else if pc == s.screenCode {
			s.screenCode = nil
		}
	}
	s.pairCodes = kept
}
