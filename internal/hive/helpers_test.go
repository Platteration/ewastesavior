package hive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

const (
	testKey   = "test-swarm-key-0123456789abcdef"
	testAdmin = "test-admin-token-0123456789abcdef-0123"
)

var (
	secretOnce sync.Once
	testSecret auth.Secret
	adminOnce  sync.Once
	adminSec   auth.Secret
)

func swarmSecret() auth.Secret {
	secretOnce.Do(func() { testSecret = auth.NewSwarmSecret(testKey) })
	return testSecret
}

func adminSecret() auth.Secret {
	adminOnce.Do(func() { adminSec = auth.NewAdminSecret(testAdmin) })
	return adminSec
}

type testHive struct {
	t   *testing.T
	s   *Server
	ts  *httptest.Server
	url string
	dir string
	hc  *http.Client // pinned admin/node client
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testConfig returns a config with a fast loop and default protocol timings.
func testConfig(dir string) Config {
	cfg := Config{DataDir: dir, SwarmKey: testKey, AdminToken: testAdmin, Log: quietLog(), StatusFile: "-"}
	cfg.tune.loopInterval = 20 * time.Millisecond
	cfg.tune.minPersist = 10 * time.Millisecond
	return cfg
}

func newHive(t *testing.T, mod func(*Config)) *testHive {
	t.Helper()
	return startHive(t, t.TempDir(), mod)
}

func startHive(t *testing.T, dir string, mod func(*Config)) *testHive {
	t.Helper()
	cfg := testConfig(dir)
	if mod != nil {
		mod(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.TLS = s.TLSConfig()
	ts.StartTLS()
	h := &testHive{t: t, s: s, ts: ts, url: ts.URL, dir: dir, hc: pinnedClient(s.Fingerprint())}
	t.Cleanup(h.stop)
	return h
}

// stop shuts the hive down (idempotent) so a new one can load its state.
func (h *testHive) stop() {
	h.ts.Close()
	h.s.Close()
}

func pinnedClient(fp string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: auth.ClientTLSConfig(fp, nil)},
		Timeout:   60 * time.Second,
	}
}

// do sends a JSON request and decodes a JSON response into out (if non-nil
// and 2xx). It returns the status and raw body.
func do(t *testing.T, c *http.Client, method, url, bearer string, in, out any, hdr ...string) (int, []byte) {
	t.Helper()
	var body io.Reader
	switch v := in.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(v)
	case string:
		body = strings.NewReader(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, url, raw, err)
		}
	}
	return resp.StatusCode, raw
}

// admin performs an admin request with the admin token as bearer.
func (h *testHive) admin(method, path string, in, out any) int {
	h.t.Helper()
	st, _ := do(h.t, h.hc, method, h.url+"/api/v1/admin/"+path, testAdmin, in, out)
	return st
}

func (h *testHive) mustAdmin(method, path string, in, out any) {
	h.t.Helper()
	st, raw := do(h.t, h.hc, method, h.url+"/api/v1/admin/"+path, testAdmin, in, out)
	if st != http.StatusOK {
		h.t.Fatalf("%s %s: %d %s", method, path, st, raw)
	}
}

func (h *testHive) hello() proto.Hello {
	h.t.Helper()
	var hl proto.Hello
	if st, raw := do(h.t, h.hc, "GET", h.url+"/api/v1/hello", "", nil, &hl); st != 200 {
		h.t.Fatalf("hello: %d %s", st, raw)
	}
	return hl
}

// testNode is a scripted node agent.
type testNode struct {
	t     *testing.T
	h     *testHive
	req   proto.RegisterRequest
	token string
	resp  proto.RegisterResponse
}

var nodeSeq struct {
	sync.Mutex
	n int
}

// newNode makes a compute+display node with 4 cores, 8 GiB and full
// isolation. mod may adjust the registration request.
func (h *testHive) newNode(mod func(*proto.RegisterRequest)) *testNode {
	nodeSeq.Lock()
	nodeSeq.n++
	i := nodeSeq.n
	nodeSeq.Unlock()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", h.t.Name(), i)))
	id := "n" + hex.EncodeToString(sum[:6])
	req := proto.RegisterRequest{
		NodeID:  id,
		HWIDs:   []string{"mac:" + hex.EncodeToString(sum[6:12])},
		BootID:  "boot-" + id,
		Roles:   []proto.Role{proto.RoleCompute, proto.RoleDisplay},
		Version: "test",
		Inventory: proto.Inventory{Arch: "amd64", Cores: 4, MemTotalMB: 8192, CPUFlags: []string{"sse2", "lm"},
			Connectors: []proto.Connector{{Name: "HDMI-A-1", Status: "connected", WidthMM: 520, HeightMM: 290}}},
		Total:       proto.Resources{Cores: 4, MemMB: 4096, DiskMB: 10000},
		Sandbox:     "strict",
		SandboxCaps: append([]string(nil), fullIsolationCaps...),
	}
	if mod != nil {
		mod(&req)
	}
	return &testNode{t: h.t, h: h, req: req}
}

// tryRegister runs hello + register; proofFP overrides the fingerprint the
// proof is bound to ("" = the hive's real one); keyless sends no proof.
func (n *testNode) tryRegister(sec auth.Secret, proofFP string, keyless bool) (int, []byte, proto.Hello) {
	n.t.Helper()
	hl := n.h.hello()
	req := n.req
	req.HiveNonce = hl.Nonce
	req.NodeNonce = auth.NewNonce()
	if proofFP == "" {
		proofFP = n.h.s.Fingerprint()
	}
	if !keyless {
		req.Proof = sec.NodeProof(req.HiveNonce, req.NodeNonce, req.NodeID, proofFP)
	}
	var resp proto.RegisterResponse
	st, raw := do(n.t, n.h.hc, "POST", n.h.url+"/api/v1/register", "", req, &resp)
	if st == 200 {
		n.token = resp.Token
		n.resp = resp
		if !keyless {
			want := sec.HiveProof(req.HiveNonce, req.NodeNonce, req.NodeID, proofFP)
			if !auth.VerifyProof(want, resp.HiveProof) {
				n.t.Fatalf("hive_proof does not verify")
			}
		}
	}
	return st, raw, hl
}

func (n *testNode) register() proto.RegisterResponse {
	n.t.Helper()
	st, raw, _ := n.tryRegister(swarmSecret(), "", false)
	if st != 200 {
		n.t.Fatalf("register %s: %d %s", n.req.NodeID, st, raw)
	}
	return n.resp
}

func (n *testNode) registerKeyless() proto.RegisterResponse {
	n.t.Helper()
	st, raw, _ := n.tryRegister(auth.Secret{}, "", true)
	if st != 200 {
		n.t.Fatalf("keyless register: %d %s", st, raw)
	}
	return n.resp
}

func (n *testNode) api(method, path string, in, out any) (int, []byte) {
	n.t.Helper()
	return do(n.t, n.h.hc, method, n.h.url+"/api/v1/"+path, n.token, in, out)
}

func (n *testNode) heartbeat(running ...proto.RunningTask) proto.HeartbeatResponse {
	n.t.Helper()
	return n.heartbeatStatus(proto.NodeStatus{State: proto.NodeIdle, Total: n.req.Total, Free: n.req.Total, RunningTasks: running})
}

func (n *testNode) heartbeatStatus(st proto.NodeStatus) proto.HeartbeatResponse {
	n.t.Helper()
	if st.RunningTasks == nil {
		st.RunningTasks = []proto.RunningTask{}
	}
	var resp proto.HeartbeatResponse
	if code, raw := n.api("POST", "heartbeat", proto.HeartbeatRequest{Status: st}, &resp); code != 200 {
		n.t.Fatalf("heartbeat: %d %s", code, raw)
	}
	return resp
}

func (n *testNode) claimWith(req proto.ClaimRequest) []proto.Task {
	n.t.Helper()
	var resp proto.ClaimResponse
	if code, raw := n.api("POST", "claim", req, &resp); code != 200 {
		n.t.Fatalf("claim: %d %s", code, raw)
	}
	return resp.Tasks
}

// claim asks for up to max tasks with the node's full Total as free.
func (n *testNode) claim(max int) []proto.Task {
	n.t.Helper()
	return n.claimWith(proto.ClaimRequest{ClaimID: auth.NewID(8), Free: n.req.Total, Max: max})
}

func (n *testNode) report(t proto.Task, rep proto.TaskReport) int {
	n.t.Helper()
	if rep.Lease == "" {
		rep.Lease = t.Lease
	}
	code, _ := n.api("POST", "tasks/"+t.ID+"/report", rep, nil)
	return code
}

func (n *testNode) succeed(t proto.Task) {
	n.t.Helper()
	if code := n.report(t, proto.TaskReport{State: proto.TaskSucceeded}); code != 200 {
		n.t.Fatalf("succeed report: %d", code)
	}
}

func running(ts ...proto.Task) []proto.RunningTask {
	out := make([]proto.RunningTask, len(ts))
	for i, t := range ts {
		out[i] = proto.RunningTask{ID: t.ID, Lease: t.Lease, Phase: proto.PhaseRunning, RunS: 1}
	}
	return out
}

func (h *testHive) submit(spec proto.JobSpec) proto.JobDetail {
	h.t.Helper()
	var d proto.JobDetail
	h.mustAdmin("POST", "jobs", spec, &d)
	return d
}

func scriptJob(count int, mod func(*proto.JobSpec)) proto.JobSpec {
	spec := proto.JobSpec{Script: "echo {{index}}", Count: count, Resources: proto.Resources{Cores: 1, MemMB: 128, DiskMB: 64}}
	if mod != nil {
		mod(&spec)
	}
	return spec
}

func (h *testHive) job(id string) proto.JobDetail {
	h.t.Helper()
	var d proto.JobDetail
	h.mustAdmin("GET", "jobs/"+id, nil, &d)
	return d
}

func (h *testHive) task(id string) proto.TaskView {
	h.t.Helper()
	var v proto.TaskView
	h.mustAdmin("GET", "tasks/"+id, nil, &v)
	return v
}

func (h *testHive) nodeView(ref string) proto.NodeView {
	h.t.Helper()
	var v proto.NodeView
	h.mustAdmin("GET", "nodes/"+ref, nil, &v)
	return v
}

// eventually polls cond until it's true or 5 s pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sha(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// putBlob uploads data as admin and returns its hash.
func (h *testHive) putBlob(data []byte) string {
	h.t.Helper()
	sum := sha(data)
	st, raw := do(h.t, h.hc, "PUT", h.url+"/api/v1/blobs/"+sum, testAdmin, data, nil)
	if st != 200 {
		h.t.Fatalf("put blob: %d %s", st, raw)
	}
	return sum
}
