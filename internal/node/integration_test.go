//go:build linux

package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/display"
	"github.com/platteration/ewastesavior/internal/hive"
	"github.com/platteration/ewastesavior/internal/proto"
)

// These tests run a real hive over TLS on loopback and real node agents
// (with fixture hardware, the sandbox off and memory display devices).

const testKey = "integration-test-swarm-key-0123456789"

type testHive struct {
	t      *testing.T
	srv    *hive.Server
	addr   string
	dir    string
	cancel context.CancelFunc
	done   chan struct{}
	admin  *http.Client
}

func hiveConfig(dir string, ln net.Listener) hive.Config {
	return hive.Config{
		DataDir:           dir,
		SwarmKey:          testKey,
		AdminToken:        strings.Repeat("a", 40),
		Listener:          ln,
		StatusFile:        "-",
		HeartbeatInterval: time.Second,
		OfflineAfter:      4 * time.Second,
		LostAfter:         2 * time.Second,
		MissingAfter:      6 * time.Second,
		RecoveryWindow:    8 * time.Second,
		Log:               testLogger(),
	}
}

func testLogger() *slog.Logger {
	if os.Getenv("SAVIOR_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startHive(t *testing.T, dir, addr string) *testHive {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the old port may linger briefly after a restart
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	srv, err := hive.New(hiveConfig(dir, ln))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &testHive{t: t, srv: srv, addr: ln.Addr().String(), dir: dir, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		srv.Run(ctx)
	}()
	h.admin = &http.Client{Transport: &http.Transport{TLSClientConfig: auth.ClientTLSConfig(srv.Fingerprint(), nil)}, Timeout: 30 * time.Second}
	return h
}

func (h *testHive) stop() {
	h.cancel()
	<-h.done
}

func (h *testHive) url() string { return "https://" + h.addr }

// api calls the admin API with the admin token.
func (h *testHive) api(method, path string, in, out any) int {
	h.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.url()+path, body)
	req.Header.Set("Authorization", "Bearer "+h.srv.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.admin.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(b, out); err != nil {
			h.t.Fatalf("%s %s: decode %v: %s", method, path, err, b)
		}
	}
	if resp.StatusCode >= 300 {
		h.t.Logf("%s %s -> %d %s", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode
}

func (h *testHive) get(path string) []byte {
	req, _ := http.NewRequest(http.MethodGet, h.url()+path, nil)
	req.Header.Set("Authorization", "Bearer "+h.srv.AdminToken())
	resp, err := h.admin.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

type testNode struct {
	agent  *Agent
	cancel context.CancelFunc
	done   chan struct{}
	disp   *display.MemDevice
}

func startNode(t *testing.T, h *testHive, id string, mutate func(*config.Config, *Options)) *testNode {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = id
	cfg.Name = id
	cfg.SwarmKey = testKey
	cfg.Hive = h.url()
	cfg.Sandbox = "none"
	cfg.Roles = []string{"compute", "display"}
	dir := searchableTempDir(t)
	mem := display.NewMemDevice(320, 240, 32, "", 0, 0)
	opt := Options{
		Config:        cfg,
		Log:           testLogger().With("node", id),
		SysRoot:       "../hwinfo/testdata/qemu",
		WorkRoot:      filepath.Join(dir, "work"),
		CacheDir:      filepath.Join(dir, "cache"),
		CgroupRoot:    filepath.Join(dir, "no-cgroup"),
		StatusFile:    filepath.Join(dir, "status.json"),
		OpenDisplay:   func() (display.Device, error) { return mem, nil },
		SkipBenchmark: true,
		MinHeartbeat:  time.Second,
	}
	if mutate != nil {
		mutate(&opt.Config, &opt)
	}
	a, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &testNode{agent: a, cancel: cancel, done: make(chan struct{}), disp: mem}
	go func() {
		defer close(n.done)
		a.Run(ctx)
	}()
	t.Cleanup(n.stop)
	return n
}

// searchableTempDir returns a temp dir that task users (dropped uids) can
// traverse: with sandbox=none there's no private root to bind it into.
func searchableTempDir(t *testing.T) string {
	dir := t.TempDir()
	for d := dir; d != os.TempDir() && d != "/"; d = filepath.Dir(d) {
		os.Chmod(d, 0o755)
	}
	return dir
}

func (n *testNode) stop() {
	n.cancel()
	select {
	case <-n.done:
	case <-time.After(30 * time.Second):
	}
}

func (n *testNode) link() proto.HiveLink {
	n.agent.mu.Lock()
	defer n.agent.mu.Unlock()
	return n.agent.link
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *testHive) job(id string) proto.JobDetail {
	var jd proto.JobDetail
	h.api(http.MethodGet, "/api/v1/admin/jobs/"+id, nil, &jd)
	return jd
}

func (h *testHive) submit(spec proto.JobSpec) string {
	h.t.Helper()
	if spec.Requirements.Isolation == "" {
		spec.Requirements.Isolation = proto.IsolationAny
	}
	// The fixture machine has 480 MB of RAM and a display, so the default
	// 128 MB + 64 MB (RAM scratch) task wouldn't fit its budget.
	if spec.Resources.MemMB == 0 {
		spec.Resources = proto.Resources{Cores: 0.5, MemMB: 48, DiskMB: 16}
	}
	var jd proto.JobDetail
	if code := h.api(http.MethodPost, "/api/v1/admin/jobs", spec, &jd); code != 200 && code != 201 {
		h.t.Fatalf("submit: %d", code)
	}
	return jd.ID
}

func TestSwarmJobsOutputsAndDisplay(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startHive(t, t.TempDir(), "")
	defer h.stop()
	nodes := []*testNode{
		startNode(t, h, "node-a", nil),
		startNode(t, h, "node-b", nil),
		startNode(t, h, "node-c", nil),
	}
	waitFor(t, "3 nodes online", 30*time.Second, func() bool {
		var views []proto.NodeView
		h.api(http.MethodGet, "/api/v1/admin/nodes", nil, &views)
		online := 0
		for _, v := range views {
			if v.Liveness == proto.NodeOnline {
				online++
			}
		}
		return online == 3
	})
	for _, n := range nodes {
		if n.link() != proto.LinkConnected {
			t.Fatalf("node link %s", n.link())
		}
	}

	// A 5-task job that writes outputs and logs.
	id := h.submit(proto.JobSpec{
		Name:    "squares",
		Script:  "echo \"log of task $SAVIOR_TASK_INDEX\"\necho $(( {{index}} * {{index}} )) > result.txt\n",
		Count:   5,
		Outputs: []string{"result.txt"},
	})
	waitFor(t, "job succeeded", 60*time.Second, func() bool { return h.job(id).State == proto.JobSucceeded })

	var outs []proto.OutputEntry
	h.api(http.MethodGet, "/api/v1/admin/jobs/"+id+"/outputs", nil, &outs)
	if len(outs) != 5 {
		t.Fatalf("want 5 outputs, got %+v", outs)
	}
	for _, o := range outs {
		got := strings.TrimSpace(string(h.get("/api/v1/blobs/" + o.Blob)))
		if want := fmt.Sprint(o.Index * o.Index); got != want {
			t.Errorf("task %d output %q, want %q", o.Index, got, want)
		}
	}
	var page proto.TaskPage
	h.api(http.MethodGet, "/api/v1/admin/jobs/"+id+"/tasks", nil, &page)
	if len(page.Tasks) == 0 {
		t.Fatal("no tasks listed")
	}
	logText := string(h.get("/api/v1/admin/tasks/" + page.Tasks[0].ID + "/log"))
	if !strings.Contains(logText, "log of task") {
		t.Errorf("task log %q", logText)
	}

	// Display: assign text to node-a and check its screen changed.
	before := nodes[0].disp.Shows()
	text := proto.DisplaySpec{Mode: proto.DisplayText, Text: "HELLO SWARM", BG: "#003366", FG: "#ffffff"}
	if code := h.api(http.MethodPatch, "/api/v1/admin/nodes/node-a", proto.NodePatch{Display: &text}, nil); code != 200 {
		t.Fatalf("patch display: %d", code)
	}
	waitFor(t, "text on screen", 20*time.Second, func() bool {
		st := nodes[0].agent.disp.State()
		return st.Mode == proto.DisplayText && nodes[0].disp.Shows() > before
	})
	img := nodes[0].disp.DecodeLogical()
	if c := img.RGBAAt(2, 2); c.B < 0x50 || c.R > 0x10 {
		t.Errorf("background pixel %v, want #003366", c)
	}

	// Cancel a long job.
	long := h.submit(proto.JobSpec{Name: "sleepy", Command: []string{"sleep", "300"}, Count: 1})
	waitFor(t, "sleepy running", 30*time.Second, func() bool { return h.job(long).Counts.Running == 1 })
	h.api(http.MethodPost, "/api/v1/admin/jobs/"+long+"/cancel", nil, nil)
	waitFor(t, "task stopped on node", 30*time.Second, func() bool {
		for _, n := range nodes {
			n.agent.mu.Lock()
			held := len(n.agent.tasks)
			n.agent.mu.Unlock()
			if held > 0 {
				return false
			}
		}
		return true
	})
	if st := h.job(long).State; st != proto.JobCanceled {
		t.Errorf("canceled job state %s", st)
	}
}

func TestNodeLossRequeues(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startHive(t, t.TempDir(), "")
	defer h.stop()
	a := startNode(t, h, "loser", nil)
	waitFor(t, "loser connected", 30*time.Second, func() bool { return a.link() == proto.LinkConnected })
	id := h.submit(proto.JobSpec{Name: "survivor", Script: "sleep 4; echo done > out.txt", Outputs: []string{"out.txt"}, Count: 1})
	waitFor(t, "running on loser", 30*time.Second, func() bool { return h.job(id).Counts.Running == 1 })
	a.stop() // the machine vanishes mid-task
	b := startNode(t, h, "winner", nil)
	waitFor(t, "winner connected", 30*time.Second, func() bool { return b.link() == proto.LinkConnected })
	waitFor(t, "job succeeded elsewhere", 90*time.Second, func() bool { return h.job(id).State == proto.JobSucceeded })
	var page proto.TaskPage
	h.api(http.MethodGet, "/api/v1/admin/jobs/"+id+"/tasks", nil, &page)
	if len(page.Tasks) != 1 || page.Tasks[0].Node != "winner" || page.Tasks[0].Interruptions < 1 || page.Tasks[0].Failures != 0 {
		t.Fatalf("task %+v", page.Tasks)
	}
}

func TestWrongKeyAndPinMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startHive(t, t.TempDir(), "")
	defer h.stop()
	bad := startNode(t, h, "intruder", func(c *config.Config, _ *Options) { c.SwarmKey = "not-the-swarm-key-000000" })
	pinned := startNode(t, h, "pinned", func(c *config.Config, _ *Options) {
		c.HiveFingerprint = "sha256:" + strings.Repeat("0", 64)
	})
	waitFor(t, "intruder rejected", 30*time.Second, func() bool { return bad.link() == proto.LinkRejected })
	waitFor(t, "pin mismatch", 30*time.Second, func() bool { return pinned.link() == proto.LinkFingerprintMismatch })
	var views []proto.NodeView
	h.api(http.MethodGet, "/api/v1/admin/nodes", nil, &views)
	if len(views) != 0 {
		t.Fatalf("rejected nodes were registered: %+v", views)
	}
}

// mitmProxy passes the first connection through to the hive untouched
// (so /hello sees the real certificate) and terminates every later
// connection with its own certificate, recording any HTTP request.
type mitmProxy struct {
	ln       net.Listener
	target   string
	conns    atomic.Int32
	mu       sync.Mutex
	requests []string
	tlsCfg   *tls.Config
}

func newMITM(t *testing.T, target string) *mitmProxy {
	certPEM, keyPEM, err := auth.GenerateCert("savior-hive")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := tls.X509KeyPair(certPEM, keyPEM)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &mitmProxy{ln: ln, target: target, tlsCfg: &tls.Config{Certificates: []tls.Certificate{cert}}}
	go p.serve()
	t.Cleanup(func() { ln.Close() })
	return p
}

func (p *mitmProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.conns.Add(1) == 1 {
			go func() {
				up, err := net.Dial("tcp", p.target)
				if err != nil {
					c.Close()
					return
				}
				go func() { io.Copy(up, c); up.Close() }()
				io.Copy(c, up)
				c.Close()
			}()
			continue
		}
		go func() {
			defer c.Close()
			tc := tls.Server(c, p.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			buf := make([]byte, 4096)
			n, _ := tc.Read(buf)
			p.mu.Lock()
			p.requests = append(p.requests, string(buf[:n]))
			p.mu.Unlock()
		}()
	}
}

func TestMITMNeverGetsProof(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startHive(t, t.TempDir(), "")
	defer h.stop()
	p := newMITM(t, h.addr)
	n := startNode(t, h, "victim", func(c *config.Config, _ *Options) { c.Hive = "https://" + p.ln.Addr().String() })
	waitFor(t, "handshake attempted twice", 30*time.Second, func() bool { return p.conns.Load() >= 2 })
	time.Sleep(time.Second)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.requests {
		if strings.Contains(r, "proof") || strings.Contains(r, "/register") {
			t.Fatalf("the man in the middle received a registration: %q", r)
		}
	}
	if l := n.link(); l == proto.LinkConnected {
		t.Fatalf("node connected through a MITM")
	}
}

func TestHiveRestartReadoptsRunningTask(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	dir := t.TempDir()
	h := startHive(t, dir, "")
	addr := h.addr
	n := startNode(t, h, "steady", nil)
	waitFor(t, "connected", 30*time.Second, func() bool { return n.link() == proto.LinkConnected })
	id := h.submit(proto.JobSpec{Name: "long", Script: "sleep 8; echo ok > out.txt", Outputs: []string{"out.txt"}, Count: 1})
	waitFor(t, "running", 30*time.Second, func() bool { return h.job(id).Counts.Running == 1 })
	h.stop()

	h2 := startHive(t, dir, addr)
	defer h2.stop()
	waitFor(t, "job succeeded after restart", 90*time.Second, func() bool { return h2.job(id).State == proto.JobSucceeded })
	var page proto.TaskPage
	h2.api(http.MethodGet, "/api/v1/admin/jobs/"+id+"/tasks", nil, &page)
	if len(page.Tasks) != 1 || page.Tasks[0].Attempt != 1 {
		t.Fatalf("task was re-run instead of re-adopted: %+v", page.Tasks)
	}
}
