//go:build linux

package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/display"
	"github.com/platteration/ewastesavior/internal/hive"
	"github.com/platteration/ewastesavior/internal/hwinfo"
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

// putBlob uploads data as an admin blob and returns its hash.
func (h *testHive) putBlob(data []byte) string {
	h.t.Helper()
	sum := sha256Hex(data)
	req, _ := http.NewRequest(http.MethodPut, h.url()+"/api/v1/blobs/"+sum, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+h.srv.AdminToken())
	resp, err := h.admin.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("put blob: %d %s", resp.StatusCode, b)
	}
	return sum
}

func (h *testHive) nodeView(ref string) proto.NodeView {
	h.t.Helper()
	var v proto.NodeView
	if code := h.api(http.MethodGet, "/api/v1/admin/nodes/"+ref, nil, &v); code != http.StatusOK {
		h.t.Fatalf("node %s: %d", ref, code)
	}
	return v
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

// screenAt returns a pixel of the node's (logical) screen.
func (n *testNode) screenAt(x, y int) color.RGBA { return n.disp.DecodeLogical().RGBAAt(x, y) }

var (
	red   = color.RGBA{255, 0, 0, 255}
	blue  = color.RGBA{0, 0, 255, 255}
	white = color.RGBA{255, 255, 255, 255}
)

// Uploaded images reach the screen through the hive's render endpoint (node
// token scope, exact geometry, pixel-for-pixel decode), and the spec's
// background fills the letterbox (SPEC-RUNTIME-10, DISPLAY-HW-1/5).
func TestBlobImageOnScreen(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startHive(t, t.TempDir(), "")
	defer h.stop()
	n := startNode(t, h, "lobby", nil)
	waitFor(t, "lobby connected", 30*time.Second, func() bool { return n.link() == proto.LinkConnected })

	// 400×100 red on the 320×240 screen: contain puts it at y 80..160.
	img := image.NewRGBA(image.Rect(0, 0, 400, 100))
	for i := range img.Pix {
		img.Pix[i] = []byte{255, 0, 0, 255}[i%4]
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	sum := h.putBlob(b.Bytes())
	spec := proto.DisplaySpec{Mode: proto.DisplayImage, Image: &proto.Media{Blob: sum}, BG: "#ffffff", Fit: "contain"}
	if code := h.api(http.MethodPatch, "/api/v1/admin/nodes/lobby", proto.NodePatch{Display: &spec}, nil); code != 200 {
		t.Fatalf("patch display: %d", code)
	}
	waitFor(t, "image on screen", 30*time.Second, func() bool {
		st := n.agent.disp.State()
		return st.Mode == proto.DisplayImage && st.Ready && n.screenAt(160, 120) == red
	})
	if c := n.screenAt(10, 10); c != white {
		t.Errorf("letterbox %v, want the spec's bg #ffffff", c)
	}
	if st := n.agent.disp.State(); len(st.MediaErrors) != 0 {
		t.Errorf("media errors %v", st.MediaErrors)
	}
}

// A video wall across two real agents: each shows its part of an uploaded
// image, follows content changes, and returns to its status screen when
// the wall is deleted (DESIGN 15, E2E-GAPS-13).
func TestWallAcrossAgents(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startHive(t, t.TempDir(), "")
	defer h.stop()
	left := startNode(t, h, "wall-a", nil)
	right := startNode(t, h, "wall-b", nil)
	waitFor(t, "both connected", 30*time.Second, func() bool {
		return left.link() == proto.LinkConnected && right.link() == proto.LinkConnected
	})
	both := []*testNode{left, right}

	// 1×2 wall of 320×240 mm cells (1 mm = 1 screen pixel), 640×240
	// image: left half red, right half blue.
	sum := h.putBlob(splitPNG(t, 640, 240))
	wall := proto.WallSpec{Name: "lobby", Rows: 1, Cols: 2,
		Cells: []proto.WallCell{
			{Node: "wall-a", Row: 0, Col: 0, WidthMM: 320, HeightMM: 240},
			{Node: "wall-b", Row: 0, Col: 1, WidthMM: 320, HeightMM: 240},
		},
		Content: proto.DisplaySpec{Mode: proto.DisplayImage, Image: &proto.Media{Blob: sum}, Fit: "stretch"}}
	var saved proto.WallSpec
	if code := h.api(http.MethodPost, "/api/v1/admin/walls", wall, &saved); code != 200 || saved.ID == "" {
		t.Fatalf("create wall: %d %+v", code, saved)
	}
	waitFor(t, "each screen shows its half", 30*time.Second, func() bool {
		return left.screenAt(160, 120) == red && right.screenAt(160, 120) == blue &&
			left.screenAt(310, 120) == red && right.screenAt(10, 120) == blue
	})
	for _, n := range both {
		if st := n.agent.disp.State(); st.Mode != proto.DisplayWall || len(st.MediaErrors) != 0 {
			t.Errorf("%s: display state %+v", n.agent.name, st)
		}
	}

	// contain with a bg: the image (4:1 on the 8:3 canvas) leaves bands
	// at the top and bottom of both screens in the spec's colour.
	wide := h.putBlob(splitPNG(t, 400, 100))
	saved.Content = proto.DisplaySpec{Mode: proto.DisplayImage, Image: &proto.Media{Blob: wide}, Fit: "contain", BG: "#ffffff"}
	if code := h.api(http.MethodPut, "/api/v1/admin/walls/"+saved.ID, saved, &saved); code != 200 {
		t.Fatalf("update wall: %d", code)
	}
	waitFor(t, "letterboxed wall", 30*time.Second, func() bool {
		return left.screenAt(160, 120) == red && right.screenAt(160, 120) == blue &&
			left.screenAt(160, 10) == white && right.screenAt(160, 230) == white
	})

	// New content reaches both screens.
	before := [][]byte{left.disp.DecodeLogical().Pix, right.disp.DecodeLogical().Pix}
	saved.Content = proto.DisplaySpec{Mode: proto.DisplayTest}
	if code := h.api(http.MethodPut, "/api/v1/admin/walls/"+saved.ID, saved, &saved); code != 200 {
		t.Fatalf("update wall: %d", code)
	}
	waitFor(t, "test pattern on both", 30*time.Second, func() bool {
		for i, n := range both {
			if bytes.Equal(n.disp.DecodeLogical().Pix, before[i]) || n.screenAt(160, 10) == white {
				return false
			}
		}
		return true
	})

	if code := h.api(http.MethodDelete, "/api/v1/admin/walls/"+saved.ID, nil, nil); code != 200 {
		t.Fatalf("delete wall: %d", code)
	}
	waitFor(t, "status screens again", 30*time.Second, func() bool {
		return left.agent.disp.State().Mode == proto.DisplayStatus && right.agent.disp.State().Mode == proto.DisplayStatus
	})
}

// identify shows on the node's screen, and reboot is acked to the real
// hive before the agent acts on it (SPEC-RUNTIME-09).
func TestNodeActions(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startHive(t, t.TempDir(), "")
	defer h.stop()
	var agent atomic.Pointer[Agent]
	type sysCall struct {
		action      string
		stillQueued bool // the hive still sends the action after the ack
		ackPending  int
	}
	calls := make(chan sysCall, 4)
	n := startNode(t, h, "actor", func(_ *config.Config, o *Options) {
		o.SystemAction = func(action string) error {
			a := agent.Load()
			a.mu.Lock()
			hc, pending := a.hc, len(a.ackPending)
			a.mu.Unlock()
			// Ask the hive what it still wants, acking nothing.
			st := a.nodeStatus()
			st.AckedActions = nil
			resp, err := hc.heartbeat(context.Background(), &proto.HeartbeatRequest{Status: st})
			queued := err != nil
			for _, act := range resp.Directives.Actions {
				queued = queued || act.Action == action
			}
			calls <- sysCall{action, queued, pending}
			return nil
		}
	})
	agent.Store(n.agent)
	waitFor(t, "actor connected", 30*time.Second, func() bool { return n.link() == proto.LinkConnected })
	green := proto.DisplaySpec{Mode: proto.DisplayColor, BG: "#00ff00"}
	if code := h.api(http.MethodPatch, "/api/v1/admin/nodes/actor", proto.NodePatch{Display: &green}, nil); code != 200 {
		t.Fatalf("patch display: %d", code)
	}
	waitFor(t, "green screen", 20*time.Second, func() bool { return n.screenAt(160, 120) == color.RGBA{0, 255, 0, 255} })

	if code := h.api(http.MethodPost, "/api/v1/admin/nodes/actor/action", proto.NodeAction{Action: proto.ActionIdentify, Seconds: 5}, nil); code != 200 {
		t.Fatalf("identify: %d", code)
	}
	// The overlay panel (inset 15 px on 320×240) flashes yellow and dark.
	waitFor(t, "identify overlay", 20*time.Second, func() bool {
		c := n.screenAt(22, 22)
		return c == color.RGBA{0xff, 0xd4, 0x00, 0xff} || c == color.RGBA{0x10, 0x10, 0x10, 0xff}
	})

	if code := h.api(http.MethodPost, "/api/v1/admin/nodes/actor/action", proto.NodeAction{Action: proto.ActionReboot}, nil); code != 200 {
		t.Fatalf("reboot: %d", code)
	}
	var c sysCall
	select {
	case c = <-calls:
	case <-time.After(30 * time.Second):
		t.Fatal("the agent never acted on the reboot")
	}
	if c.action != proto.ActionReboot || c.stillQueued || c.ackPending != 0 {
		t.Fatalf("reboot ran as %+v; want it acked to the hive first", c)
	}
	// The hive no longer sends it, and the agent doesn't run it again.
	select {
	case c = <-calls:
		t.Fatalf("second system action %+v", c)
	case <-time.After(3 * time.Second):
	}
}

// roles=auto: a display that appears after the agent started enables the
// display role, and the node registers again with it (DESIGN 5.3,
// SPEC-CORE-4).
func TestLateDisplayEnablesRole(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	h := startHive(t, t.TempDir(), "")
	defer h.stop()
	root := t.TempDir() // an empty /sys: no DRM card or framebuffer yet
	n := startNode(t, h, "late", func(c *config.Config, o *Options) {
		c.Roles = []string{"auto"}
		o.DisplayProbe = func() bool { return hwinfo.HasDisplay(root) }
		o.DisplayPoll = 100 * time.Millisecond
	})
	waitFor(t, "late connected", 30*time.Second, func() bool { return n.link() == proto.LinkConnected })
	if roles := h.nodeView("late").Roles; !slices.Equal(roles, []proto.Role{proto.RoleCompute}) {
		t.Fatalf("roles before a display: %v", roles)
	}
	// A spec set before the display exists is shown once it does.
	text := proto.DisplaySpec{Mode: proto.DisplayText, Text: "HELLO", BG: "#003366"}
	if code := h.api(http.MethodPatch, "/api/v1/admin/nodes/late", proto.NodePatch{Display: &text}, nil); code != 200 {
		t.Fatalf("patch display: %d", code)
	}
	time.Sleep(2 * time.Second) // a heartbeat or two delivers it to the display-less agent
	n.agent.mu.Lock()
	early := n.agent.disp
	n.agent.mu.Unlock()
	if early != nil {
		t.Fatal("display controller started without a display")
	}

	if err := os.MkdirAll(filepath.Join(root, "sys/class/graphics/fb0"), 0o755); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "display role registered", 30*time.Second, func() bool {
		v := h.nodeView("late")
		return slices.Equal(v.Roles, []proto.Role{proto.RoleCompute, proto.RoleDisplay}) && v.Liveness == proto.NodeOnline
	})
	waitFor(t, "late connected again", 30*time.Second, func() bool { return n.link() == proto.LinkConnected })
	waitFor(t, "text on the new screen", 30*time.Second, func() bool {
		n.agent.mu.Lock()
		disp := n.agent.disp
		n.agent.mu.Unlock()
		return disp != nil && disp.State().Mode == proto.DisplayText && n.screenAt(2, 2) == color.RGBA{0x00, 0x33, 0x66, 0xff}
	})
	n.agent.mu.Lock()
	reserved := n.agent.total.MemMB
	n.agent.mu.Unlock()
	if v := h.nodeView("late"); v.Status.Total.MemMB != reserved {
		t.Errorf("hive sees %d MB for tasks, agent offers %d", v.Status.Total.MemMB, reserved)
	}
}
