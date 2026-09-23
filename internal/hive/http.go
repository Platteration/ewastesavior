package hive

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/hive/web"
	"github.com/platteration/ewastesavior/internal/proto"
)

// Header values from DESIGN 6.3 "Web hardening".
const (
	htmlCSP     = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	apiCSP      = "default-src 'none'; frame-ancestors 'none'"
	blobCSP     = "sandbox"
	cookieName  = "savior_admin"
	csrfHeader  = "X-Savior"
	logOffsetHd = "X-Savior-Log-Offset"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Node API (DESIGN 7.1).
	mux.HandleFunc("GET /api/v1/hello", s.handleHello)
	mux.HandleFunc("POST /api/v1/register", s.handleRegister)
	mux.Handle("POST /api/v1/heartbeat", s.nodeOnly(s.handleHeartbeat))
	mux.Handle("POST /api/v1/claim", s.nodeOnly(s.handleClaim))
	mux.Handle("POST /api/v1/tasks/{id}/report", s.nodeOnly(s.handleReport))
	mux.Handle("POST /api/v1/tasks/{id}/log", s.nodeOnly(s.handleLogAppend))
	mux.Handle("GET /api/v1/blobs/{sha}", s.nodeOrAdmin(s.handleBlobGet))
	mux.Handle("GET /api/v1/blobs/{sha}/render", s.nodeOrAdmin(s.handleRender))
	mux.Handle("PUT /api/v1/blobs/{sha}", s.nodeOrAdmin(s.handleBlobPut))
	mux.Handle("GET /api/v1/stats", s.nodeOrAdmin(s.handleStats))

	// Admin API (DESIGN 7.2).
	const a = "/api/v1/admin/"
	mux.HandleFunc("POST "+a+"login", s.handleLogin)
	mux.HandleFunc("POST "+a+"session", s.handleSession)
	mux.Handle("POST "+a+"logout", s.adminOnly(s.handleLogout))
	mux.Handle("POST "+a+"sessions/revoke", s.adminOnly(s.handleRevokeAll))
	mux.Handle("POST "+a+"pair", s.adminOnly(s.handlePair))
	mux.Handle("GET "+a+"info", s.adminOnly(s.handleInfo))
	mux.Handle("GET "+a+"node-config", s.adminOnly(s.handleNodeConfig))
	mux.Handle("GET "+a+"nodes", s.adminOnly(s.handleNodes))
	mux.Handle("GET "+a+"nodes/{ref}", s.adminOnly(s.handleNodeGet))
	mux.Handle("PATCH "+a+"nodes/{ref}", s.adminOnly(s.handleNodePatch))
	mux.Handle("DELETE "+a+"nodes/{ref}", s.adminOnly(s.handleNodeDelete))
	mux.Handle("POST "+a+"nodes/{ref}/action", s.adminOnly(s.handleNodeAction))
	mux.Handle("POST "+a+"identify", s.adminOnly(s.handleIdentify))
	mux.Handle("GET "+a+"jobs", s.adminOnly(s.handleJobs))
	mux.Handle("POST "+a+"jobs", s.adminOnly(s.handleJobSubmit))
	mux.Handle("GET "+a+"jobs/{id}", s.adminOnly(s.handleJobGet))
	mux.Handle("GET "+a+"jobs/{id}/tasks", s.adminOnly(s.handleJobTasks))
	mux.Handle("GET "+a+"jobs/{id}/outputs", s.adminOnly(s.handleJobOutputs))
	mux.Handle("GET "+a+"jobs/{id}/outputs.zip", s.adminOnly(s.handleJobOutputsZip))
	mux.Handle("POST "+a+"jobs/{id}/cancel", s.adminOnly(s.handleJobCancel))
	mux.Handle("DELETE "+a+"jobs/{id}", s.adminOnly(s.handleJobDelete))
	mux.Handle("GET "+a+"tasks/{id}", s.adminOnly(s.handleTaskGet))
	mux.Handle("GET "+a+"tasks/{id}/log", s.adminOnly(s.handleTaskLog))
	mux.Handle("GET "+a+"tasks/{id}/outputs/{name...}", s.adminOnly(s.handleTaskOutput))
	mux.Handle("GET "+a+"walls", s.adminOnly(s.handleWalls))
	mux.Handle("POST "+a+"walls", s.adminOnly(s.handleWallCreate))
	mux.Handle("GET "+a+"walls/{id}", s.adminOnly(s.handleWallGet))
	mux.Handle("PUT "+a+"walls/{id}", s.adminOnly(s.handleWallPut))
	mux.Handle("DELETE "+a+"walls/{id}", s.adminOnly(s.handleWallDelete))
	mux.Handle("GET "+a+"blobs", s.adminOnly(s.handleBlobList))
	mux.Handle("POST "+a+"blobs", s.adminOnly(s.handleBlobPost))
	mux.Handle("DELETE "+a+"blobs/{sha}", s.adminOnly(s.handleBlobDelete))
	mux.Handle("POST "+a+"blobs/gc", s.adminOnly(s.handleBlobGC))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "no such endpoint")
	})
	mux.Handle("/", web.Handler())
	return s.secure(mux)
}

// secure adds the headers every response carries and a default deadline;
// handlers that stream or long-poll extend it.
func (s *Server) secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			hd.Set("Cache-Control", "no-store")
			hd.Set("Content-Security-Policy", apiCSP)
		} else {
			// The dashboard handler may set its own (equal or stricter) policy.
			hd.Set("Content-Security-Policy", htmlCSP)
			hd.Set("X-Frame-Options", "DENY")
		}
		setDeadlines(w, jsonDeadline)
		h.ServeHTTP(w, r)
	})
}

// setDeadlines bounds the rest of a request (DESIGN 6.3: no global server
// timeouts, per-handler deadlines instead).
func setDeadlines(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	t := time.Now().Add(d)
	_ = rc.SetReadDeadline(t)
	_ = rc.SetWriteDeadline(t)
}

// blobDeadline allows for slow links: 1 min plus size at 128 KiB/s.
func blobDeadline(size int64) time.Duration {
	if size < 0 {
		size = 0
	}
	return time.Minute + time.Duration(size/blobRateFloor)*time.Second
}

// writeJSON encodes v before sending the status, so a value JSON can't
// encode (NaN, ±Inf) becomes a 500 instead of an empty 200.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b, _ = json.Marshal(proto.ErrorResponse{Error: "could not encode the response: " + err.Error()})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, proto.ErrorResponse{Error: fmt.Sprintf(format, args...)})
}

func writeOK(w http.ResponseWriter) { writeJSON(w, http.StatusOK, struct{}{}) }

// decodeJSON reads a JSON body capped at 1 MiB. strict rejects unknown
// fields (admin input, to catch typos); node input is lenient so newer
// agents can add fields.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, strict bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body larger than %d bytes", maxJSONBody)
			return false
		}
		writeErr(w, http.StatusBadRequest, "invalid JSON: %s", proto.Sanitize(err.Error(), 200, false))
		return false
	}
	return true
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// nodeOnly authenticates a node token; the handler gets the token hash and
// must look the node up again under the lock (it may re-register meanwhile).
func (s *Server) nodeOnly(h func(http.ResponseWriter, *http.Request, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash, ok := s.nodeToken(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unknown node token; register again")
			return
		}
		h(w, r, hash)
	})
}

// nodeToken returns the hash of a valid node bearer token.
func (s *Server) nodeToken(r *http.Request) (string, bool) {
	tok, ok := auth.BearerToken(r.Header.Get("Authorization"))
	if !ok {
		return "", false
	}
	hash := auth.HashToken(tok)
	s.mu.Lock()
	_, ok = s.tokens[hash]
	s.mu.Unlock()
	return hash, ok
}

// nodeByTokenLocked returns the node owning a token hash, or nil.
func (s *Server) nodeByTokenLocked(hash string) *node {
	id, ok := s.tokens[hash]
	if !ok {
		return nil
	}
	n := s.nodes[id]
	if n == nil || n.tokenHash != hash {
		return nil
	}
	return n
}

// requester identifies who made a request to a node-or-admin endpoint.
type requester struct {
	nodeToken string // hash; "" for admins
}

func (q requester) isAdmin() bool { return q.nodeToken == "" }

func (s *Server) nodeOrAdmin(h func(http.ResponseWriter, *http.Request, requester)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hash, ok := s.nodeToken(r); ok {
			h(w, r, requester{nodeToken: hash})
			return
		}
		if _, status, msg := s.authenticateAdmin(r); status != 0 {
			writeErr(w, status, "%s", msg)
			return
		}
		s.maybeAdoptClientTime(r)
		h(w, r, requester{})
	})
}

// adminCtx describes how an admin request authenticated.
type adminCtx struct {
	sessionHash string // "" when the raw admin token was used
	viaCookie   bool
}

func (s *Server) adminOnly(h func(http.ResponseWriter, *http.Request, adminCtx)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, status, msg := s.authenticateAdmin(r)
		if status != 0 {
			writeErr(w, status, "%s", msg)
			return
		}
		s.maybeAdoptClientTime(r)
		h(w, r, a)
	})
}

// authenticateAdmin checks a session (bearer or cookie) or the admin token
// (bearer). Wrong bearer credentials count toward the admin rate limit;
// cookie-authenticated state changes need X-Savior: 1 and a same-origin
// Origin (CSRF, DESIGN 6.3).
func (s *Server) authenticateAdmin(r *http.Request) (adminCtx, int, string) {
	src := sourceKey(r.RemoteAddr)
	if s.limits.blocked(bucketAdmin, src) {
		return adminCtx{}, http.StatusTooManyRequests, "too many failed attempts; try again in a minute"
	}
	if tok, ok := auth.BearerToken(r.Header.Get("Authorization")); ok {
		hash := auth.HashToken(tok)
		if s.sessionValid(hash) {
			return adminCtx{sessionHash: hash}, 0, ""
		}
		if auth.VerifyProof(s.adminTokenHash, hash) {
			return adminCtx{}, 0, ""
		}
		s.limits.fail(bucketAdmin, src)
		return adminCtx{}, http.StatusUnauthorized, "invalid or expired admin credentials"
	}
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		hash := auth.HashToken(c.Value)
		if !s.sessionValid(hash) {
			return adminCtx{}, http.StatusUnauthorized, "session expired; pair again"
		}
		if isStateChanging(r.Method) {
			if msg := csrfCheck(r); msg != "" {
				return adminCtx{}, http.StatusForbidden, msg
			}
		}
		return adminCtx{sessionHash: hash, viaCookie: true}, 0, ""
	}
	return adminCtx{}, http.StatusUnauthorized, "admin authentication required"
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// csrfCheck returns why a cookie-authenticated state change is refused.
func csrfCheck(r *http.Request) string {
	if r.Header.Get(csrfHeader) != "1" {
		return "missing X-Savior: 1 header"
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			return "cross-origin request refused"
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return "cross-site request refused"
	}
	return ""
}
