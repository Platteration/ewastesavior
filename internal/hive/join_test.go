package hive

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

func TestJoinHandshake(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	hl := h.hello()
	if hl.HiveID != h.s.HiveID() || hl.APIVersion != proto.APIVersion || len(hl.Nonce) != 64 || hl.Time.IsZero() {
		t.Fatalf("hello: %+v", hl)
	}
	n := h.newNode(func(r *proto.RegisterRequest) { r.Name = "Lab-PC" })
	resp := n.register() // also verifies hive_proof against our fingerprint
	if resp.NodeID != n.req.NodeID || resp.Name != "lab-pc" || resp.ShortCode != proto.ShortCode(n.req.NodeID) ||
		len(resp.Token) != 64 || resp.Pending || resp.HeartbeatIntervalS != 5 || resp.Directives.Name != "lab-pc" {
		t.Fatalf("register response: %+v", resp)
	}
	hb := n.heartbeat()
	if hb.Time.IsZero() || hb.Directives.Pending {
		t.Fatalf("heartbeat: %+v", hb)
	}
	v := h.nodeView(n.req.NodeID)
	if v.Liveness != proto.NodeOnline || !v.Approved || !v.FullIsolation || v.Name != "lab-pc" {
		t.Fatalf("node view: %+v", v)
	}
	// The same view by name.
	if h.nodeView("lab-pc").ID != n.req.NodeID {
		t.Fatal("lookup by name")
	}
	// A bogus token is 401.
	n.token = "nope"
	if code, _ := n.api("POST", "heartbeat", proto.HeartbeatRequest{}, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
}

func TestWrongKeyRejectedAndRateLimited(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	wrong := auth.NewSwarmSecret("some-other-swarm-key-000000")
	n := h.newNode(nil)
	for i := 0; i < 5; i++ {
		st, raw, _ := n.tryRegister(wrong, "", false)
		if st != http.StatusForbidden {
			t.Fatalf("attempt %d: %d %s", i, st, raw)
		}
		if bytes.Contains(raw, []byte("hive_proof")) {
			t.Fatal("a rejected join must not carry a hive proof")
		}
	}
	// The source is now locked out, even with the right key.
	if st, _, _ := n.tryRegister(swarmSecret(), "", false); st != http.StatusTooManyRequests {
		t.Fatalf("after 5 failures: %d", st)
	}
	// Other buckets are unaffected.
	if st := h.admin("GET", "info", nil, nil); st != 200 {
		t.Fatalf("admin bucket affected: %d", st)
	}
}

// TestMITMNeverGetsValidProof puts a TLS-terminating proxy with its own
// certificate between node and hive. The node binds its proof to the
// certificate it sees (the proxy's), so the hive rejects it and the proxy
// never holds a proof it could use against the real hive.
func TestMITMNeverGetsValidProof(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	target, _ := url.Parse(h.url)
	var mu sync.Mutex
	var captured []byte
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/register" {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			captured = b
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		rp.ServeHTTP(w, r)
	}))
	proxy.Config.ErrorLog = log.New(io.Discard, "", 0)
	proxy.StartTLS() // its own certificate
	defer proxy.Close()

	// The node sees the proxy's certificate on /hello and pins it.
	var fpSeen string
	tofu := &http.Client{Transport: &http.Transport{TLSClientConfig: auth.ClientTLSConfig("", func(fp string) { fpSeen = fp })}}
	var hl proto.Hello
	if st, _ := do(t, tofu, "GET", proxy.URL+"/api/v1/hello", "", nil, &hl); st != 200 {
		t.Fatalf("hello via proxy: %d", st)
	}
	if fpSeen == "" || fpSeen == h.s.Fingerprint() {
		t.Fatalf("expected the proxy certificate, saw %q", fpSeen)
	}
	pinned := pinnedClient(fpSeen)
	n := h.newNode(nil)
	req := n.req
	req.HiveNonce, req.NodeNonce = hl.Nonce, auth.NewNonce()
	req.Proof = swarmSecret().NodeProof(req.HiveNonce, req.NodeNonce, req.NodeID, fpSeen)
	st, raw := do(t, pinned, "POST", proxy.URL+"/api/v1/register", "", req, nil)
	if st != http.StatusForbidden || bytes.Contains(raw, []byte("token")) {
		t.Fatalf("proof bound to the proxy certificate accepted: %d %s", st, raw)
	}
	// Replaying the captured request directly to the hive (fresh nonce,
	// same proof) fails too: the proof is bound to the proxy's certificate.
	mu.Lock()
	var stolen proto.RegisterRequest
	json.Unmarshal(captured, &stolen)
	mu.Unlock()
	stolen.HiveNonce = h.hello().Nonce
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/register", "", stolen, nil); st != http.StatusForbidden {
		t.Fatalf("replayed MITM proof: %d", st)
	}
	// And the simple form: a proof over a different fingerprint is refused.
	if st, _, _ := n.tryRegister(swarmSecret(), "sha256:"+strings.Repeat("ab", 32), false); st != http.StatusForbidden {
		t.Fatalf("proof over another fingerprint: %d", st)
	}
	// A pinned client refuses the proxy outright.
	if _, err := h.hc.Get(proxy.URL + "/api/v1/hello"); err == nil {
		t.Fatal("pinned client talked to the proxy")
	}
}

func TestNonceReplayAndForgery(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	_, _, hl := n.tryRegister(swarmSecret(), "", false)
	// Same hive nonce again (new node nonce, valid proof): replay refused.
	req := n.req
	req.HiveNonce, req.NodeNonce = hl.Nonce, auth.NewNonce()
	req.Proof = swarmSecret().NodeProof(req.HiveNonce, req.NodeNonce, req.NodeID, h.s.Fingerprint())
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/register", "", req, nil); st != http.StatusForbidden {
		t.Fatalf("nonce replay: %d", st)
	}
	// Expired and foreign nonces.
	for _, nonce := range []string{
		h.s.nonces.Issue(time.Now().Add(-2 * time.Minute)),
		auth.NewNonceIssuer().Issue(time.Now()),
		strings.Repeat("0", 64),
	} {
		req.HiveNonce, req.NodeNonce = nonce, auth.NewNonce()
		req.Proof = swarmSecret().NodeProof(req.HiveNonce, req.NodeNonce, req.NodeID, h.s.Fingerprint())
		if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/register", "", req, nil); st != http.StatusForbidden {
			t.Fatalf("bad nonce accepted: %d", st)
		}
	}
}

func TestRegisterValidation(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(func(r *proto.RegisterRequest) { r.NodeID = "BAD ID" })
	if st, _, _ := n.tryRegister(swarmSecret(), "", false); st != http.StatusBadRequest {
		t.Fatalf("invalid node id: %d", st)
	}
	n = h.newNode(nil)
	hl := h.hello()
	req := n.req
	req.HiveNonce, req.NodeNonce = hl.Nonce, auth.NewNonce()
	req.Proof = swarmSecret().NodeProof(req.HiveNonce, req.NodeNonce, req.NodeID, h.s.Fingerprint())
	st, _ := do(t, h.hc, "POST", h.url+"/api/v1/register", "", req, nil, APIVersionHeader, "2")
	if st != http.StatusUpgradeRequired {
		t.Fatalf("api version header mismatch: %d", st)
	}
	body := map[string]any{"node_id": req.NodeID, "hive_nonce": req.HiveNonce, "node_nonce": req.NodeNonce, "proof": req.Proof, "api_version": 9}
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/register", "", body, nil); st != http.StatusUpgradeRequired {
		t.Fatalf("api version field mismatch: %d", st)
	}
	// Oversized bodies are refused.
	big := `{"node_id":"` + strings.Repeat("a", 2<<20) + `"}`
	if st, _ := do(t, h.hc, "POST", h.url+"/api/v1/register", "", big, nil); st != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", st)
	}
}

func TestDuplicateNodeID(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.OfflineAfter = 300 * time.Millisecond })
	a := h.newNode(nil)
	a.register()
	dup := h.newNode(func(r *proto.RegisterRequest) { *r = a.req; r.BootID = "another-machine" })
	if st, raw, _ := dup.tryRegister(swarmSecret(), "", false); st != http.StatusConflict || !strings.Contains(string(raw), "duplicate node id") {
		t.Fatalf("duplicate id: %d %s", st, raw)
	}
	// The same session may re-register (e.g. after a lost response).
	a.register()
	// Once the first machine is offline the ID is free again.
	time.Sleep(400 * time.Millisecond)
	if st, raw, _ := dup.tryRegister(swarmSecret(), "", false); st != 200 {
		t.Fatalf("after offline: %d %s", st, raw)
	}
	// The old session's token no longer works.
	if code, _ := a.api("POST", "heartbeat", proto.HeartbeatRequest{}, nil); code != http.StatusUnauthorized {
		t.Fatalf("old token: %d", code)
	}
}

func TestKeylessPendingUntilApproved(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	resp := n.registerKeyless()
	if !resp.Pending || !resp.Directives.Pending || resp.HiveProof != "" {
		t.Fatalf("keyless join must be pending: %+v", resp)
	}
	job := h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Requirements.Isolation = proto.IsolationAny }))
	if tasks := n.claim(4); len(tasks) != 0 {
		t.Fatalf("pending node got tasks: %v", tasks)
	}
	// No blobs either, even display media.
	blob := h.putBlob(testPNG(t, 8, 8))
	if code, _ := n.api("GET", "blobs/"+blob, nil, nil); code != http.StatusForbidden {
		t.Fatalf("pending node read a blob: %d", code)
	}
	if hb := n.heartbeat(); !hb.Directives.Pending || hb.Directives.Display != nil {
		t.Fatalf("pending heartbeat: %+v", hb.Directives)
	}
	if code, _ := n.api("GET", "stats", nil, nil); code != http.StatusForbidden {
		t.Fatalf("pending node read stats: %d", code)
	}
	var v proto.NodeView
	h.mustAdmin("PATCH", "nodes/"+n.req.NodeID, proto.NodePatch{Approved: ptr(true)}, &v)
	if !v.Approved {
		t.Fatal("approve")
	}
	if hb := n.heartbeat(); hb.Directives.Pending {
		t.Fatal("still pending after approval")
	}
	tasks := n.claim(4)
	if len(tasks) != 1 || tasks[0].JobID != job.ID {
		t.Fatalf("approved node claim: %v", tasks)
	}

	// A keyless join whose HWID matches an approved record is re-approved.
	again := h.newNode(func(r *proto.RegisterRequest) { r.NodeID = "n00000000beef"; r.HWIDs = n.req.HWIDs; r.BootID = "b2" })
	if resp := again.registerKeyless(); resp.Pending {
		t.Fatal("HWID match of an approved record should approve")
	}
	// Unknown hardware stays pending.
	other := h.newNode(nil)
	if resp := other.registerKeyless(); !resp.Pending {
		t.Fatal("unknown keyless node approved")
	}
}

func TestJoinPolicyApprove(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.JoinPolicy = "approve" })
	n := h.newNode(nil)
	if resp := n.register(); !resp.Pending {
		t.Fatal("approve policy: new node must be pending")
	}
	h.mustAdmin("PATCH", "nodes/"+n.req.NodeID, proto.NodePatch{Approved: ptr(true)}, nil)
	if resp := n.register(); resp.Pending {
		t.Fatal("approved node pending after re-register")
	}
	// Revoking approval sticks even under open-style re-joins.
	h.mustAdmin("PATCH", "nodes/"+n.req.NodeID, proto.NodePatch{Approved: ptr(false)}, nil)
	if resp := n.register(); !resp.Pending {
		t.Fatal("denied node re-approved")
	}
}

func TestNamesUniqueAndSanitized(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	a := h.newNode(func(r *proto.RegisterRequest) { r.Name = "kiosk" })
	b := h.newNode(func(r *proto.RegisterRequest) {
		r.Name = "kiosk"
		r.Labels = map[string]string{"room": "a\x1b[31m1", "BAD KEY": "x"}
		r.Inventory.CPUModel = "Intel\x00\x07 Pentium M"
	})
	a.register()
	resp := b.register()
	if !strings.HasPrefix(resp.Name, "savior-") || resp.Name != "savior-"+b.req.NodeID[len(b.req.NodeID)-6:] {
		t.Fatalf("taken name should fall back to the default name: %q", resp.Name)
	}
	v := h.nodeView(b.req.NodeID)
	if len(v.Warnings) == 0 || v.ConfigLabels["room"] != "a[31m1" || len(v.ConfigLabels) != 1 || v.Inventory.CPUModel != "Intel Pentium M" {
		t.Fatalf("sanitizing: %+v %+v %q", v.Warnings, v.ConfigLabels, v.Inventory.CPUModel)
	}
	// Admin rename wins; duplicates are 409.
	if st := h.admin("PATCH", "nodes/"+b.req.NodeID, proto.NodePatch{Name: ptr("kiosk")}, nil); st != http.StatusConflict {
		t.Fatalf("duplicate rename: %d", st)
	}
	if st := h.admin("PATCH", "nodes/"+b.req.NodeID, proto.NodePatch{Name: ptr("n0123456789ab")}, nil); st != http.StatusBadRequest {
		t.Fatalf("ID-shaped name: %d", st)
	}
	h.mustAdmin("PATCH", "nodes/"+b.req.NodeID, proto.NodePatch{Name: ptr("lobby")}, nil)
	if resp := b.register(); resp.Name != "lobby" {
		t.Fatalf("admin name lost on re-register: %q", resp.Name)
	}
	// More than 32 config labels are cut.
	many := map[string]string{}
	for i := 0; i < 40; i++ {
		many[strings.Repeat("k", 1)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	c := h.newNode(func(r *proto.RegisterRequest) { r.Labels = many })
	c.register()
	if v := h.nodeView(c.req.NodeID); len(v.ConfigLabels) != proto.MaxLabels {
		t.Fatalf("labels: %d", len(v.ConfigLabels))
	}
}

func TestHWIDTakeover(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.OfflineAfter = 200 * time.Millisecond })
	old := h.newNode(nil)
	old.register()
	h.mustAdmin("PATCH", "nodes/"+old.req.NodeID, proto.NodePatch{Name: ptr("front-desk"),
		Labels: &map[string]string{"floor": "2"}, Display: &proto.DisplaySpec{Mode: proto.DisplayClock}}, nil)
	time.Sleep(300 * time.Millisecond) // old record goes offline
	// A new NIC changed the node ID, but the DMI UUID is the same.
	nw := h.newNode(func(r *proto.RegisterRequest) { r.HWIDs = append([]string{"uuid:1234"}, old.req.HWIDs...) })
	resp := nw.register()
	if resp.Name != "front-desk" || resp.Directives.Labels["floor"] != "2" || resp.Directives.Display == nil ||
		resp.Directives.Display.Mode != proto.DisplayClock {
		t.Fatalf("takeover lost settings: %+v", resp)
	}
	if st := h.admin("GET", "nodes/"+old.req.NodeID, nil, nil); st != http.StatusNotFound {
		t.Fatalf("old record still present: %d", st)
	}
}

func ptr[T any](v T) *T { return &v }
