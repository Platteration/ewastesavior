package ctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/discovery"
	"github.com/platteration/ewastesavior/internal/proto"
)

// fakeHive implements the parts of the hive API that ctl uses, including
// the proof-based login exactly as DESIGN 6.3 describes it.
type fakeHive struct {
	t      *testing.T
	srv    *httptest.Server
	url    string
	token  string
	secret auth.Secret
	nonces *auth.NonceIssuer
	hiveID string
	cert   atomic.Pointer[tls.Certificate]
	fp     atomic.Pointer[string]

	mu            sync.Mutex
	usedNonces    map[string]bool
	sessions      map[string]time.Time
	logins        int
	requests      []recordedRequest
	badHiveProof  bool
	hiveClockSkew time.Duration

	nodes      map[string]proto.NodeView
	patches    []proto.NodePatch
	actions    []proto.NodeAction
	identifies []proto.IdentifyRequest
	deleted    []string

	jobs       map[string]*fakeJob
	submitted  []proto.JobSpec
	jobFinal   proto.JobState
	pollsToEnd int
	failedTask proto.TaskView

	blobs      map[string][]byte
	blobPuts   map[string]int // bodies actually read per sha
	corrupt    map[string]bool
	walls      map[string]proto.WallSpec
	logs       map[string][]byte
	logChunk   int
	logReqs    []logReq
	taskState  map[string]proto.TaskState
	outputs    map[string][]proto.OutputEntry
	anyOutputs []proto.OutputEntry // for jobs without their own entry
	nodeConf   string
}

type recordedRequest struct {
	Method, Path, Query string
	Header              http.Header
	Body                []byte
}

type logReq struct {
	offset int64
	waitS  string
}

type fakeJob struct {
	detail proto.JobDetail
	polls  int
}

func newFakeHive(t *testing.T) *fakeHive {
	t.Helper()
	h := &fakeHive{
		t:          t,
		token:      auth.NewToken(),
		nonces:     auth.NewNonceIssuer(),
		hiveID:     "hive-" + auth.NewID(4),
		usedNonces: map[string]bool{},
		sessions:   map[string]time.Time{},
		nodes:      map[string]proto.NodeView{},
		jobs:       map[string]*fakeJob{},
		jobFinal:   proto.JobSucceeded,
		pollsToEnd: 2,
		blobs:      map[string][]byte{},
		blobPuts:   map[string]int{},
		corrupt:    map[string]bool{},
		walls:      map[string]proto.WallSpec{},
		logs:       map[string][]byte{},
		taskState:  map[string]proto.TaskState{},
		outputs:    map[string][]proto.OutputEntry{},
	}
	h.secret = auth.NewAdminSecret(h.token)
	h.setNewCert()
	h.nodes["n0123456789ab"] = proto.NodeView{
		ID: "n0123456789ab", Name: "lobby-1", ShortCode: "7KQ", Liveness: proto.NodeOnline, Approved: true,
		Roles: []proto.Role{proto.RoleCompute, proto.RoleDisplay}, AdminLabels: map[string]string{"a": "1", "b": "2"},
	}
	srv := httptest.NewUnstartedServer(h.handler())
	// Refused handshakes (pinning tests) are expected; keep the log quiet.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*h.cert.Load()}}, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	h.srv, h.url = srv, srv.URL
	return h
}

// setNewCert makes the hive present a fresh certificate (a new identity).
func (h *fakeHive) setNewCert() {
	certPEM, keyPEM, err := auth.GenerateCert("savior-hive")
	if err != nil {
		h.t.Fatal(err)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		h.t.Fatal(err)
	}
	fp := auth.Fingerprint(c.Certificate[0])
	h.cert.Store(&c)
	h.fp.Store(&fp)
	if h.srv != nil {
		h.srv.CloseClientConnections()
	}
}

func (h *fakeHive) fingerprint() string { return *h.fp.Load() }

func (h *fakeHive) requestCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func (h *fakeHive) allRequests() []recordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedRequest(nil), h.requests...)
}

func (h *fakeHive) loginCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logins
}

func (h *fakeHive) revokeSessions() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = map[string]time.Time{}
}

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErrResp(w http.ResponseWriter, status int, msg string) {
	writeJSONResp(w, status, proto.ErrorResponse{Error: msg})
}

func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (h *fakeHive) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/hello", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		writeJSONResp(w, 200, proto.Hello{HiveID: h.hiveID, APIVersion: proto.APIVersion, Version: "test",
			Nonce: h.nonces.Issue(now), Time: now.Add(h.hiveClockSkew)})
	})
	mux.HandleFunc("POST /api/v1/admin/login", h.handleLogin)
	admin := func(pattern string, fn http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			tok, ok := auth.BearerToken(r.Header.Get("Authorization"))
			h.mu.Lock()
			exp, valid := h.sessions[tok]
			h.mu.Unlock()
			if !ok || !valid || time.Now().After(exp) {
				writeErrResp(w, http.StatusUnauthorized, "invalid or expired admin credentials")
				return
			}
			fn(w, r)
		})
	}
	a := "/api/v1/admin/"
	admin("POST "+a+"logout", func(w http.ResponseWriter, r *http.Request) {
		tok, _ := auth.BearerToken(r.Header.Get("Authorization"))
		h.mu.Lock()
		delete(h.sessions, tok)
		h.mu.Unlock()
		writeJSONResp(w, 200, struct{}{})
	})
	admin("POST "+a+"sessions/revoke", func(w http.ResponseWriter, r *http.Request) {
		h.revokeSessions()
		writeJSONResp(w, 200, struct{}{})
	})
	admin("POST "+a+"pair", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, proto.PairCode{Code: "ABCD2345", ExpiresAt: time.Now().Add(10 * time.Minute)})
	})
	admin("GET "+a+"info", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, proto.HiveInfo{HiveID: h.hiveID, Version: "test", APIVersion: proto.APIVersion,
			Fingerprint: h.fingerprint(), TimeSynced: true, TimeSource: proto.TimeNTP, Persistent: true})
	})
	admin("GET "+a+"node-config", func(w http.ResponseWriter, r *http.Request) {
		conf := h.nodeConf
		if conf == "" {
			conf = fmt.Sprintf("swarm_key = K3Y0123456789ABCDEFGHJKMNPQRSTVW\nhive_fingerprint = %s\nhive = %s\n",
				h.fingerprint(), r.URL.Query().Get("hive"))
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, conf)
	})
	admin("GET "+a+"nodes", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		out := make([]proto.NodeView, 0, len(h.nodes))
		for _, n := range h.nodes {
			out = append(out, n)
		}
		h.mu.Unlock()
		writeJSONResp(w, 200, out)
	})
	findNode := func(ref string) (proto.NodeView, bool) {
		for _, n := range h.nodes {
			if n.ID == ref || n.Name == ref {
				return n, true
			}
		}
		return proto.NodeView{}, false
	}
	admin("GET "+a+"nodes/{ref}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		n, ok := findNode(r.PathValue("ref"))
		h.mu.Unlock()
		if !ok {
			writeErrResp(w, 404, "no such node")
			return
		}
		writeJSONResp(w, 200, n)
	})
	admin("PATCH "+a+"nodes/{ref}", func(w http.ResponseWriter, r *http.Request) {
		var p proto.NodePatch
		if err := decodeStrict(r, &p); err != nil {
			writeErrResp(w, 400, err.Error())
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		n, ok := findNode(r.PathValue("ref"))
		if !ok {
			writeErrResp(w, 404, "no such node")
			return
		}
		h.patches = append(h.patches, p)
		if p.Display != nil {
			if err := proto.ValidateDisplaySpec(p.Display); err != nil {
				writeErrResp(w, 400, err.Error())
				return
			}
			d := *p.Display
			d.Rev = 7
			n.Display = &d
		}
		if p.Labels != nil {
			n.AdminLabels = *p.Labels
			n.Labels = *p.Labels
		}
		if p.Name != nil {
			n.Name = *p.Name
		}
		if p.DisplayRotate != nil {
			n.DisplayRotate = *p.DisplayRotate
		}
		if p.Drain != nil {
			n.Drain = *p.Drain
		}
		h.nodes[n.ID] = n
		writeJSONResp(w, 200, n)
	})
	admin("DELETE "+a+"nodes/{ref}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.deleted = append(h.deleted, r.PathValue("ref"))
		h.mu.Unlock()
		writeJSONResp(w, 200, struct{}{})
	})
	admin("POST "+a+"nodes/{ref}/action", func(w http.ResponseWriter, r *http.Request) {
		var act proto.NodeAction
		if err := decodeStrict(r, &act); err != nil {
			writeErrResp(w, 400, err.Error())
			return
		}
		h.mu.Lock()
		h.actions = append(h.actions, act)
		h.mu.Unlock()
		writeJSONResp(w, 200, struct{}{})
	})
	admin("POST "+a+"identify", func(w http.ResponseWriter, r *http.Request) {
		var req proto.IdentifyRequest
		if err := decodeStrict(r, &req); err != nil {
			writeErrResp(w, 400, err.Error())
			return
		}
		h.mu.Lock()
		h.identifies = append(h.identifies, req)
		h.mu.Unlock()
		writeJSONResp(w, 200, struct{}{})
	})
	admin("POST "+a+"jobs", h.handleSubmit)
	admin("GET "+a+"jobs", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		var out []proto.JobView
		for _, j := range h.jobs {
			out = append(out, j.detail.JobView)
		}
		h.mu.Unlock()
		writeJSONResp(w, 200, out)
	})
	admin("GET "+a+"jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		j := h.jobs[r.PathValue("id")]
		if j == nil {
			writeErrResp(w, 404, "no such job")
			return
		}
		j.polls++
		if j.polls >= h.pollsToEnd && !j.detail.State.Terminal() {
			j.detail.State = h.jobFinal
			switch h.jobFinal {
			case proto.JobSucceeded:
				j.detail.Counts = proto.TaskCounts{Succeeded: j.detail.Count}
			case proto.JobFailed:
				j.detail.Counts = proto.TaskCounts{Succeeded: j.detail.Count - 1, Failed: 1}
			}
		} else if !j.detail.State.Terminal() {
			j.detail.State = proto.JobRunning
			j.detail.Counts = proto.TaskCounts{Running: 1, Pending: j.detail.Count - 1}
		}
		writeJSONResp(w, 200, j.detail)
	})
	admin("GET "+a+"jobs/{id}/tasks", func(w http.ResponseWriter, r *http.Request) {
		page := proto.TaskPage{Tasks: []proto.TaskView{}}
		h.mu.Lock()
		if st := r.URL.Query().Get("state"); st == string(proto.TaskFailed) && h.failedTask.ID != "" {
			page.Tasks = append(page.Tasks, h.failedTask)
			page.Total = 1
		}
		h.mu.Unlock()
		writeJSONResp(w, 200, page)
	})
	admin("GET "+a+"jobs/{id}/outputs", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		out, ok := h.outputs[r.PathValue("id")]
		if !ok {
			out = h.anyOutputs
		}
		h.mu.Unlock()
		if out == nil {
			out = []proto.OutputEntry{}
		}
		writeJSONResp(w, 200, out)
	})
	admin("GET "+a+"jobs/{id}/outputs.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = io.WriteString(w, "PK-fake-zip-"+r.PathValue("id"))
	})
	admin("POST "+a+"jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, proto.JobView{ID: r.PathValue("id"), State: proto.JobCanceled})
	})
	admin("GET "+a+"tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		st, ok := h.taskState[r.PathValue("id")]
		h.mu.Unlock()
		if !ok {
			writeErrResp(w, 404, "no such task")
			return
		}
		writeJSONResp(w, 200, proto.TaskView{ID: r.PathValue("id"), State: st})
	})
	admin("GET "+a+"tasks/{id}/log", h.handleLog)
	admin("POST "+a+"walls", func(w http.ResponseWriter, r *http.Request) {
		var ws proto.WallSpec
		if err := decodeStrict(r, &ws); err != nil {
			writeErrResp(w, 400, err.Error())
			return
		}
		if err := proto.ValidateWallSpec(&ws); err != nil {
			writeErrResp(w, 400, err.Error())
			return
		}
		h.mu.Lock()
		ws.ID = "w" + strconv.Itoa(len(h.walls)+1)
		h.walls[ws.ID] = ws
		h.mu.Unlock()
		writeJSONResp(w, 200, ws)
	})
	admin("GET "+a+"walls", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		out := []proto.WallSpec{}
		for _, ws := range h.walls {
			out = append(out, ws)
		}
		h.mu.Unlock()
		writeJSONResp(w, 200, out)
	})
	admin("GET "+a+"blobs", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		out := []proto.BlobInfo{}
		for sha, b := range h.blobs {
			out = append(out, proto.BlobInfo{SHA256: sha, Size: int64(len(b))})
		}
		h.mu.Unlock()
		writeJSONResp(w, 200, out)
	})
	admin("GET "+a+"walls/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		ws, ok := h.walls[r.PathValue("id")]
		h.mu.Unlock()
		if !ok {
			writeErrResp(w, 404, "no such wall")
			return
		}
		writeJSONResp(w, 200, ws)
	})
	admin("PUT "+a+"walls/{id}", func(w http.ResponseWriter, r *http.Request) {
		var ws proto.WallSpec
		if err := decodeStrict(r, &ws); err != nil {
			writeErrResp(w, 400, err.Error())
			return
		}
		h.mu.Lock()
		ws.ID = r.PathValue("id")
		h.walls[ws.ID] = ws
		h.mu.Unlock()
		writeJSONResp(w, 200, ws)
	})
	admin("GET /api/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, proto.SwarmStats{NodesOnline: 1, Cores: 4})
	})
	admin("PUT /api/v1/blobs/{sha}", h.handleBlobPut)
	admin("GET /api/v1/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("sha")
		h.mu.Lock()
		b, ok := h.blobs[sha]
		bad := h.corrupt[sha]
		h.mu.Unlock()
		if !ok {
			writeErrResp(w, 404, "no such blob")
			return
		}
		if bad {
			b = append([]byte("tampered:"), b...)[:len(b)]
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
	})
	return h.record(mux)
}

// record keeps every request (except blob upload bodies, which the
// handler accounts for) so tests can check what ctl sent.
func (h *fakeHive) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone()}
		if !(r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/blobs/")) {
			b, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
			rec.Body = b
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		h.mu.Lock()
		h.requests = append(h.requests, rec)
		h.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (h *fakeHive) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req proto.LoginRequest
	if err := decodeStrict(r, &req); err != nil {
		writeErrResp(w, 400, err.Error())
		return
	}
	if req.ClientNonce == "" || len(req.ClientNonce) > 128 {
		writeErrResp(w, 400, "invalid client_nonce")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.nonces.Check(req.HiveNonce, time.Now(), time.Minute) || h.usedNonces[req.HiveNonce] {
		writeErrResp(w, http.StatusForbidden, "invalid, expired or already used hive nonce")
		return
	}
	h.usedNonces[req.HiveNonce] = true
	fpOwn := h.fingerprint()
	if !auth.VerifyProof(h.secret.AdminProof(req.HiveNonce, req.ClientNonce, fpOwn), req.Proof) {
		writeErrResp(w, http.StatusForbidden, "login rejected: wrong admin token or certificate mismatch")
		return
	}
	h.logins++
	sess := auth.NewToken()
	h.sessions[sess] = time.Now().Add(12 * time.Hour)
	proof := h.secret.HiveProof(req.HiveNonce, req.ClientNonce, "admin", fpOwn)
	if h.badHiveProof {
		proof = auth.NewAdminSecret("not-the-token").HiveProof(req.HiveNonce, req.ClientNonce, "admin", fpOwn)
	}
	writeJSONResp(w, 200, proto.SessionResponse{Session: sess, ExpiresAt: time.Now().Add(h.hiveClockSkew + 12*time.Hour), HiveProof: proof})
}

func (h *fakeHive) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var spec proto.JobSpec
	if err := decodeStrict(r, &spec); err != nil {
		writeErrResp(w, 400, err.Error())
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.submitted = append(h.submitted, spec)
	norm := spec
	proto.ApplyJobDefaults(&norm)
	if err := proto.ValidateJobSpec(&norm); err != nil {
		writeErrResp(w, 400, err.Error())
		return
	}
	for _, in := range norm.Inputs {
		if in.Blob != "" {
			if _, ok := h.blobs[in.Blob]; !ok {
				writeErrResp(w, 400, "input "+in.Name+" refers to a missing blob")
				return
			}
		}
	}
	id := "j" + auth.NewID(8)
	jd := proto.JobDetail{JobView: proto.JobView{ID: id, Seq: uint64(len(h.jobs) + 1), Name: norm.Name,
		Count: norm.Count, State: proto.JobQueued, Counts: proto.TaskCounts{Pending: norm.Count}, CreatedAt: time.Now()}, Spec: norm}
	h.jobs[id] = &fakeJob{detail: jd}
	writeJSONResp(w, 200, jd)
}

func (h *fakeHive) handleLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	off, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || off < 0 {
		writeErrResp(w, 400, "invalid offset")
		return
	}
	h.mu.Lock()
	h.logReqs = append(h.logReqs, logReq{offset: off, waitS: r.URL.Query().Get("wait_s")})
	data := h.logs[id]
	chunk := h.logChunk
	end := int64(len(data))
	if off > end {
		off = end
	}
	part := data[off:]
	if chunk > 0 && len(part) > chunk {
		part = part[:chunk]
	}
	next := off + int64(len(part))
	if next == end {
		// Everything was served: the task finishes.
		if st, ok := h.taskState[id]; ok && !st.Terminal() {
			h.taskState[id] = proto.TaskSucceeded
		}
	}
	h.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(LogOffsetHeader, strconv.FormatInt(next, 10))
	_, _ = w.Write(part)
}

func (h *fakeHive) handleBlobPut(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	h.mu.Lock()
	_, exists := h.blobs[sha]
	h.mu.Unlock()
	now := time.Now()
	if exists {
		// Content-addressed: answer without reading the body.
		writeJSONResp(w, 200, proto.BlobInfo{SHA256: sha, CreatedAt: now, LastTouched: now})
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErrResp(w, 400, err.Error())
		return
	}
	if sha256Hex(b) != sha {
		writeErrResp(w, 400, "hash mismatch")
		return
	}
	h.mu.Lock()
	h.blobs[sha] = b
	h.blobPuts[sha]++
	h.requests = append(h.requests, recordedRequest{Method: "PUT-BODY", Path: r.URL.Path, Body: b})
	h.mu.Unlock()
	writeJSONResp(w, 200, proto.BlobInfo{SHA256: sha, Size: int64(len(b)), CreatedAt: now, LastTouched: now})
}

// ctlEnv runs `savior ctl` in-process with an injected environment.
type ctlEnv struct {
	t         *testing.T
	cfgPath   string
	env       map[string]string
	stdin     string
	stdinTTY  bool
	stdoutTTY bool
	secret    string
	discover  func(ctx context.Context, window time.Duration) ([]discovery.Candidate, error)
}

func newCtlEnv(t *testing.T) *ctlEnv {
	t.Helper()
	return &ctlEnv{t: t, cfgPath: filepath.Join(t.TempDir(), "cfg", "savior", "ctl.json"), env: map[string]string{}}
}

func (e *ctlEnv) run(args ...string) (int, string, string) {
	e.t.Helper()
	var out, errb bytes.Buffer
	a := &app{
		stdin:        strings.NewReader(e.stdin),
		stdout:       &out,
		stderr:       &errb,
		getenv:       func(k string) string { return e.env[k] },
		configPath:   func() (string, error) { return e.cfgPath, nil },
		stdinTTY:     e.stdinTTY,
		stdoutTTY:    e.stdoutTTY,
		readSecret:   func(context.Context) (string, error) { return e.secret, nil },
		discover:     e.discover,
		now:          time.Now,
		pollInterval: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	code := a.run(ctx, args)
	return code, out.String(), errb.String()
}

func (e *ctlEnv) readConfig() fileConfig {
	e.t.Helper()
	b, err := os.ReadFile(e.cfgPath)
	if err != nil {
		e.t.Fatalf("read ctl.json: %v", err)
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		e.t.Fatalf("parse ctl.json: %v", err)
	}
	return fc
}

// loggedIn returns an environment already logged in to h (token in env).
func loggedIn(t *testing.T, h *fakeHive) *ctlEnv {
	t.Helper()
	e := newCtlEnv(t)
	e.env[EnvAdminToken] = h.token
	if code, _, stderr := e.run("login", "--hive", h.url); code != 0 {
		t.Fatalf("login failed (%d): %s", code, stderr)
	}
	delete(e.env, EnvAdminToken)
	return e
}

// assertTokenNeverSent checks that the admin token appears nowhere in
// anything ctl sent to the hive.
func assertTokenNeverSent(t *testing.T, h *fakeHive) {
	t.Helper()
	for _, r := range h.allRequests() {
		var all strings.Builder
		all.WriteString(r.Path + "?" + r.Query + "\n")
		for k, vs := range r.Header {
			all.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
		}
		all.Write(r.Body)
		if strings.Contains(all.String(), h.token) {
			t.Fatalf("admin token was sent in %s %s", r.Method, r.Path)
		}
	}
}
