package hive

// The node swarm of the load harness (see load_test.go). It runs in a child
// process: the test binary re-executed with SAVIOR_LOAD_CHILD=<params file>,
// so that the parent process's CPU time, heap and goroutines are the hive's
// alone. Each simulated node speaks the node API the way internal/node does:
// hello + proof registration, a heartbeat every 5 s that lists
// running_tasks, long-poll claims (wait_s 25, max = free slots up to 4, the
// claim ID kept across failed attempts), a log chunk every 2 s while a task
// runs, an output upload (PUT /blobs/{sha}) and a final report retried until
// the hive answers 2xx or 409. A simulated dashboard polls the admin views
// the web UI polls, every 5 s.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// loadParams configures the child swarm.
type loadParams struct {
	HiveURL     string  `json:"hive_url"`
	Fingerprint string  `json:"fingerprint"`
	SwarmKey    string  `json:"swarm_key"`
	AdminToken  string  `json:"admin_token"`
	Nodes       int     `json:"nodes"`
	Slots       int     `json:"slots"`
	HBSeconds   float64 `json:"hb_s"`
	TaskSeconds float64 `json:"task_s"`  // mean task run time; uniform in [0.5, 1.5] x mean
	LogBPS      int     `json:"log_bps"` // log bytes per second per running task
	OutBytes    int     `json:"out_bytes"`
	DashSeconds float64 `json:"dash_s"`   // dashboard poll interval; 0 = no dashboard
	SpreadS     float64 `json:"spread_s"` // registrations are spread over this many seconds
	ResultPath  string  `json:"result_path"`
}

// loadResult is what the child reports back.
type loadResult struct {
	Endpoints  map[string]latSummary `json:"endpoints"`
	Counters   map[string]int64      `json:"counters"`
	CPUSeconds float64               `json:"cpu_s"`
	Errors     []string              `json:"errors"`
}

// --- latency recording ---

type latRecorder struct {
	mu  sync.Mutex
	eps map[string]*latSeries
}

type latSeries struct {
	samples  []float64 // ms
	errs     int
	codes    map[int]int
	bytes    int64
	maxBytes int
}

type latSummary struct {
	N        int            `json:"n"`
	Errs     int            `json:"errs"`
	Codes    map[string]int `json:"codes,omitempty"` // non-2xx statuses
	P50      float64        `json:"p50"`
	P90      float64        `json:"p90"`
	P99      float64        `json:"p99"`
	Max      float64        `json:"max"`
	AvgBytes int64          `json:"avg_bytes"`
	MaxBytes int            `json:"max_bytes"`
}

func newLatRecorder() *latRecorder { return &latRecorder{eps: map[string]*latSeries{}} }

func (r *latRecorder) add(ep string, d time.Duration, code, n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.eps[ep]
	if s == nil {
		s = &latSeries{codes: map[int]int{}}
		r.eps[ep] = s
	}
	if err != nil {
		s.errs++
		return
	}
	s.samples = append(s.samples, float64(d)/float64(time.Millisecond))
	s.codes[code]++
	s.bytes += int64(n)
	if n > s.maxBytes {
		s.maxBytes = n
	}
}

func (r *latRecorder) summary() map[string]latSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]latSummary{}
	for ep, s := range r.eps {
		v := append([]float64(nil), s.samples...)
		sort.Float64s(v)
		sum := latSummary{N: len(v), Errs: s.errs, P50: pctl(v, 0.50), P90: pctl(v, 0.90), P99: pctl(v, 0.99), MaxBytes: s.maxBytes}
		if len(v) > 0 {
			sum.Max = v[len(v)-1]
			sum.AvgBytes = s.bytes / int64(len(v))
		}
		for c, k := range s.codes {
			if c/100 != 2 {
				if sum.Codes == nil {
					sum.Codes = map[string]int{}
				}
				sum.Codes[strconv.Itoa(c)] = k
			}
		}
		out[ep] = sum
	}
	return out
}

func pctl(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// --- the swarm ---

type simSwarm struct {
	p        loadParams
	sec      auth.Secret
	rec      *latRecorder
	submitNs atomic.Int64 // job submit time (unix ns), sent by the parent

	cmu      sync.Mutex
	counters map[string]int64
	errs     []string
	wg       sync.WaitGroup
}

func (sw *simSwarm) count(name string, d int64) {
	sw.cmu.Lock()
	sw.counters[name] += d
	sw.cmu.Unlock()
}

func (sw *simSwarm) noteErr(format string, args ...any) {
	sw.cmu.Lock()
	defer sw.cmu.Unlock()
	if len(sw.errs) < 20 {
		sw.errs = append(sw.errs, fmt.Sprintf(format, args...))
	}
}

// loadNodeID is simulated node i's ID ("n" + 12 hex, like hwinfo's IDs).
func loadNodeID(i int) string {
	sum := sha256.Sum256([]byte("savior-load-node-" + strconv.Itoa(i)))
	return "n" + hex.EncodeToString(sum[:6])
}

// loadLogText is shared, read-only log output (plausible text lines).
var loadLogText = func() []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < 256<<10; i++ {
		fmt.Fprintf(&b, "step %06d: processing frame %06d of input chunk, checksum 0x%08x ok\n", i, i*7%100000, uint32(i)*2654435761)
	}
	return b.Bytes()[:256<<10]
}()

// TestLoadSwarmChild is the node swarm process of the load harness. It
// does nothing unless SAVIOR_LOAD_CHILD names a params file.
func TestLoadSwarmChild(t *testing.T) {
	path := os.Getenv("SAVIOR_LOAD_CHILD")
	if path == "" {
		t.Skip("runs only as the load harness's node swarm (SAVIOR_LOAD_CHILD)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p loadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	sw := &simSwarm{p: p, sec: auth.NewSwarmSecret(p.SwarmKey), rec: newLatRecorder(), counters: map[string]int64{}}
	ctx, cancel := context.WithCancel(context.Background())
	// The parent writes "submit <unix ns>" when it submits the job and
	// closes stdin to stop the swarm (EOF also covers a dead parent).
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "submit "); ok {
				if ns, err := strconv.ParseInt(v, 10, 64); err == nil {
					sw.submitNs.Store(ns)
				}
			}
		}
		cancel()
	}()
	cpu0 := loadCPUSeconds()
	sw.run(ctx)
	res := loadResult{Endpoints: sw.rec.summary(), CPUSeconds: loadCPUSeconds() - cpu0}
	sw.cmu.Lock()
	res.Counters, res.Errors = sw.counters, sw.errs
	sw.cmu.Unlock()
	out, _ := json.Marshal(res)
	if err := os.WriteFile(p.ResultPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (sw *simSwarm) run(ctx context.Context) {
	nodes := make([]*simNode, sw.p.Nodes)
	for i := range nodes {
		nodes[i] = newSimNode(sw, i)
	}
	for i, n := range nodes {
		delay := time.Duration(float64(i) / float64(len(nodes)) * sw.p.SpreadS * float64(time.Second))
		sw.wg.Add(1)
		go func() {
			defer sw.wg.Done()
			n.run(ctx, delay)
		}()
	}
	if sw.p.DashSeconds > 0 {
		sw.dashboard(ctx)
	}
	<-ctx.Done()
	done := make(chan struct{})
	go func() { sw.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		sw.noteErr("swarm: goroutines still running 20 s after stop")
	}
	for _, n := range nodes {
		n.hc.CloseIdleConnections()
	}
}

// --- one simulated node ---

type simNode struct {
	sw    *simSwarm
	idx   int
	id    string
	hc    *http.Client
	total proto.Resources
	boot  string

	mu    sync.Mutex
	token string
	tasks map[string]*simTask // lease -> task, until the final report is acknowledged
	wake  chan struct{}
	regMu sync.Mutex
}

type simTask struct {
	t      proto.Task
	start  time.Time
	phase  string // n.mu
	xfer   int64  // n.mu
	cancel context.CancelFunc
}

func newSimNode(sw *simSwarm, i int) *simNode {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, LocalAddr: loadSourceAddr(i)}).DialContext,
		TLSClientConfig:     auth.ClientTLSConfig(sw.p.Fingerprint, nil),
		TLSHandshakeTimeout: 20 * time.Second,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	slots := max(sw.p.Slots, 1)
	return &simNode{
		sw: sw, idx: i, id: loadNodeID(i), hc: &http.Client{Transport: tr},
		total: proto.Resources{Cores: float64(slots), MemMB: 512 * slots, DiskMB: 10000},
		boot:  fmt.Sprintf("%08x-load-boot:%s", i, auth.NewID(4)),
		tasks: map[string]*simTask{}, wake: make(chan struct{}, 1),
	}
}

func (n *simNode) poke() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// do sends one request; it returns the status, body and latency.
func (n *simNode) do(ctx context.Context, method, path string, body []byte, ctype string, withToken bool) (int, []byte, time.Duration, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.sw.p.HiveURL+path, rd)
	if err != nil {
		return 0, nil, 0, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if withToken {
		n.mu.Lock()
		tok := n.token
		n.mu.Unlock()
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	start := time.Now()
	resp, err := n.hc.Do(req)
	if err != nil {
		return 0, nil, time.Since(start), err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	resp.Body.Close()
	return resp.StatusCode, raw, time.Since(start), err
}

func (n *simNode) run(ctx context.Context, delay time.Duration) {
	if !sleepCtx(ctx, delay) {
		return
	}
	if !n.register(ctx) {
		return
	}
	n.sw.count("registered", 1)
	n.sw.wg.Add(1)
	go func() {
		defer n.sw.wg.Done()
		n.claimLoop(ctx)
	}()
	n.heartbeatLoop(ctx)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (n *simNode) inventory() proto.Inventory {
	mac := fmt.Sprintf("00:1e:4f:%02x:%02x:%02x", n.idx>>16&0xff, n.idx>>8&0xff, n.idx&0xff)
	return proto.Inventory{
		Hostname: fmt.Sprintf("lab-pc-%03d", n.idx), Arch: "amd64", MachineArch: "x86_64", Kernel: "6.6.58-savior",
		OSVersion: "SaviorOS 1.0", CPUModel: "Intel(R) Core(TM)2 Quad CPU    Q6600  @ 2.40GHz", CPUVendor: "GenuineIntel",
		CPUFlags: []string{"lm", "pae", "nx", "sse", "sse2", "sse3", "ssse3", "vmx"}, Cores: int(n.total.Cores), PhysicalCores: int(n.total.Cores),
		CPUMHz: 2400, MemTotalMB: n.total.MemMB + 1024,
		Disks:        []proto.Disk{{Name: "sda", SizeMB: 152627, Model: "WDC WD1600AAJS-75M0A0", Rotational: true, Transport: "ata"}},
		NICs:         []proto.NIC{{Name: "eth0", MAC: mac, Bus: "pci", Driver: "e1000e", SpeedMb: 1000, Up: true, Carrier: true, Addrs: []string{fmt.Sprintf("10.20.%d.%d/16", n.idx/250, n.idx%250+1)}}},
		Framebuffers: []proto.FB{{Name: "fb0", Driver: "i915drmfb", Width: 1280, Height: 1024, BPP: 32}},
		GPUs:         []proto.GPU{{Card: "card0", Driver: "i915", Vendor: "0x8086", Device: "0x2e12"}},
		Connectors:   []proto.Connector{{Name: "VGA-1", Card: "card0", Status: "connected", Enabled: true, Preferred: "1280x1024", WidthMM: 376, HeightMM: 301}},
		Vendor:       "Dell Inc.", Product: "OptiPlex 755", BIOSDate: "04/21/2008", BenchScore: 160, TempSensor: "coretemp",
	}
}

// register runs hello + register until it succeeds or ctx ends.
func (n *simNode) register(ctx context.Context) bool {
	n.regMu.Lock()
	defer n.regMu.Unlock()
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		code, raw, d, err := n.do(ctx, "GET", "/api/v1/hello", nil, "", false)
		n.sw.rec.add("hello", d, code, len(raw), err)
		var hl proto.Hello
		if err == nil && code == 200 && json.Unmarshal(raw, &hl) == nil {
			req := proto.RegisterRequest{
				APIVersion: proto.APIVersion, NodeID: n.id, HWIDs: []string{"mac:" + strings.ReplaceAll(n.inventory().NICs[0].MAC, ":", "")},
				BootID: n.boot, Roles: []proto.Role{proto.RoleCompute}, Version: "load", Inventory: n.inventory(), Total: n.total,
				Sandbox: "strict", SandboxCaps: append([]string(nil), fullIsolationCaps...), RunningTasks: n.running(),
				HiveNonce: hl.Nonce, NodeNonce: auth.NewNonce(),
			}
			req.Proof = n.sw.sec.NodeProof(req.HiveNonce, req.NodeNonce, req.NodeID, n.sw.p.Fingerprint)
			body, _ := json.Marshal(req)
			code, raw, d, err = n.do(ctx, "POST", "/api/v1/register", body, "application/json", false)
			n.sw.rec.add("register", d, code, len(raw), err)
			var resp proto.RegisterResponse
			if err == nil && code == 200 && json.Unmarshal(raw, &resp) == nil {
				n.mu.Lock()
				n.token = resp.Token
				n.mu.Unlock()
				return true
			}
		}
		if err == nil && code != 429 {
			n.sw.noteErr("register %s: %d %s", n.id, code, proto.Sanitize(string(raw), 200, false))
		}
		if !sleepCtx(ctx, backoff) {
			return false
		}
		backoff = min(backoff*2, 10*time.Second)
	}
	return false
}

func (n *simNode) reregister(ctx context.Context) {
	n.sw.count("reregister", 1)
	n.register(ctx)
}

// running lists held tasks the way the agent's running_tasks does.
func (n *simNode) running() []proto.RunningTask {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]proto.RunningTask, 0, len(n.tasks))
	for _, t := range n.tasks {
		out = append(out, proto.RunningTask{ID: t.t.ID, Lease: t.t.Lease, Phase: t.phase, RunS: math.Round(time.Since(t.start).Seconds()*10) / 10, XferBytes: t.xfer})
	}
	return out
}

func (n *simNode) freeLocked() proto.Resources {
	used := proto.Resources{}
	for _, t := range n.tasks {
		used = used.Add(t.t.Resources)
	}
	return nonNegative(n.total.Sub(used))
}

func (n *simNode) heartbeatLoop(ctx context.Context) {
	iv := time.Duration(n.sw.p.HBSeconds * float64(time.Second))
	if iv <= 0 {
		iv = DefaultHeartbeatInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		n.heartbeat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (n *simNode) heartbeat(ctx context.Context) {
	running := n.running()
	n.mu.Lock()
	free := n.freeLocked()
	n.mu.Unlock()
	st := proto.NodeStatus{
		State: proto.NodeIdle,
		Metrics: proto.Metrics{Time: time.Now(), UptimeS: 86400, Load1: float64(len(running)), CPUPercent: 25 * float64(len(running)),
			MemAvailableMB: 900, CPUTempC: 48, CPUTempLimitC: 90, BatteryPercent: -1, NetRxBytes: 123456789, NetTxBytes: 98765432},
		Total: n.total, Free: free, RunningTasks: running, Addrs: []string{fmt.Sprintf("10.20.%d.%d", n.idx/250, n.idx%250+1)},
	}
	if len(running) > 0 {
		st.State = proto.NodeBusy
	}
	body, _ := json.Marshal(proto.HeartbeatRequest{Status: st})
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, raw, d, err := n.do(hctx, "POST", "/api/v1/heartbeat", body, "application/json", true)
	if ctx.Err() != nil {
		return
	}
	n.sw.rec.add("heartbeat", d, code, len(raw), err)
	switch {
	case err != nil:
		n.sw.noteErr("heartbeat %s: %v", n.id, err)
		return
	case code == 401:
		n.reregister(ctx)
		return
	case code != 200:
		return
	}
	var resp proto.HeartbeatResponse
	if json.Unmarshal(raw, &resp) != nil {
		return
	}
	for _, ref := range resp.Directives.CancelTasks {
		n.cancelTask(ref)
	}
}

func (n *simNode) cancelTask(ref proto.TaskRef) {
	n.mu.Lock()
	t := n.tasks[ref.Lease]
	if t == nil || t.t.ID != ref.ID {
		n.mu.Unlock()
		return
	}
	reporting := t.phase == proto.PhaseReporting
	if reporting {
		delete(n.tasks, ref.Lease) // the hive no longer wants the result
	}
	n.mu.Unlock()
	n.sw.count("cancel_directives", 1)
	if reporting {
		// Like the agent (internal/node cancelTask): stop listing it, but
		// let the report in flight finish. The hive sends this when a
		// heartbeat lists a task whose report it has just applied, and
		// aborting the report would lose its answer.
		n.poke()
		return
	}
	t.cancel()
	n.poke()
}

func (n *simNode) claimLoop(ctx context.Context) {
	backoff := time.Second
	claimID := ""
	for ctx.Err() == nil {
		n.mu.Lock()
		slots := max(n.sw.p.Slots, 1) - len(n.tasks)
		free := n.freeLocked()
		n.mu.Unlock()
		slots = min(slots, 4)
		if slots <= 0 || free.Cores < 0.1-1e-9 {
			select {
			case <-ctx.Done():
				return
			case <-n.wake:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if claimID == "" {
			claimID = auth.NewID(12)
		}
		body, _ := json.Marshal(proto.ClaimRequest{ClaimID: claimID, Free: free, Max: slots, WaitS: 25})
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		code, raw, d, err := n.do(cctx, "POST", "/api/v1/claim", body, "application/json", true)
		cancel()
		if ctx.Err() != nil {
			return
		}
		var resp proto.ClaimResponse
		if err != nil || code != 200 || json.Unmarshal(raw, &resp) != nil {
			n.sw.rec.add("claim (failed)", d, code, len(raw), err)
			if code == 401 {
				n.reregister(ctx)
				continue
			}
			if err != nil {
				n.sw.noteErr("claim %s: %v", n.id, err)
			}
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff, claimID = time.Second, ""
		switch sub := n.sw.submitNs.Load(); {
		case len(resp.Tasks) == 0:
			n.sw.rec.add("claim (empty)", d, code, len(raw), nil)
		case sub == 0 || start.UnixNano() < sub:
			// Waited for the job to be submitted: not a dispatch latency.
			n.sw.rec.add("claim (woken by submit)", d, code, len(raw), nil)
		default:
			n.sw.rec.add("claim (got tasks)", d, code, len(raw), nil)
		}
		n.sw.count("tasks_claimed", int64(len(resp.Tasks)))
		for _, t := range resp.Tasks {
			n.startTask(ctx, t)
		}
	}
}

func (n *simNode) startTask(ctx context.Context, t proto.Task) {
	n.mu.Lock()
	if _, dup := n.tasks[t.Lease]; dup {
		n.mu.Unlock()
		return
	}
	tctx, cancel := context.WithCancel(ctx)
	st := &simTask{t: t, start: time.Now(), phase: proto.PhaseFetching, cancel: cancel}
	n.tasks[t.Lease] = st
	n.mu.Unlock()
	n.sw.wg.Add(1)
	go func() {
		defer n.sw.wg.Done()
		n.runTask(tctx, st)
	}()
}

func (n *simNode) setPhase(st *simTask, phase string, xfer int64) {
	n.mu.Lock()
	st.phase = phase
	st.xfer += xfer
	n.mu.Unlock()
}

func (n *simNode) drop(st *simTask) {
	n.mu.Lock()
	if n.tasks[st.t.Lease] == st {
		delete(n.tasks, st.t.Lease)
	}
	n.mu.Unlock()
	n.poke()
}

// taskDuration is uniform in [0.5, 1.5] x the mean, fixed per task index.
func (n *simNode) taskDuration(t proto.Task) time.Duration {
	h := sha256.Sum256([]byte(t.ID))
	f := 0.5 + float64(h[0])/255
	return time.Duration(f * n.sw.p.TaskSeconds * float64(time.Second))
}

func (n *simNode) runTask(ctx context.Context, st *simTask) {
	defer st.cancel()
	n.setPhase(st, proto.PhaseRunning, 0)
	chunk := n.sw.p.LogBPS * 2
	var off int64
	stale := false
	end := time.Now().Add(n.taskDuration(st.t))
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		wait := time.Until(end)
		if wait <= 0 {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			n.drop(st)
			return
		case <-tick.C:
			timer.Stop()
			if chunk > 0 && !stale {
				off, stale = n.postLog(ctx, st, off, chunk)
			}
		case <-timer.C:
		}
	}
	// The agent flushes the log tail before the final report.
	if chunk > 0 && !stale {
		off, _ = n.postLog(ctx, st, off, max(chunk/2, 64))
	}
	rep := proto.TaskReport{Lease: st.t.Lease, State: proto.TaskSucceeded, RunS: time.Since(st.start).Seconds(), MaxMemMB: 37}
	rep.CPUSeconds = rep.RunS * 0.9
	if n.sw.p.OutBytes > 0 {
		n.setPhase(st, proto.PhaseUploading, 0)
		data := loadOutput(st.t, n.sw.p.OutBytes)
		sum := sha(data)
		code, raw, d, err := n.do(ctx, "PUT", "/api/v1/blobs/"+sum, data, "application/octet-stream", true)
		if ctx.Err() != nil {
			n.drop(st)
			return
		}
		n.sw.rec.add("blob put (output)", d, code, len(raw), err)
		if err != nil || code != 200 {
			n.sw.noteErr("output upload %s: %d %v %s", st.t.ID, code, err, proto.Sanitize(string(raw), 200, false))
			rep.State, rep.ErrorKind, rep.Error, rep.ExitCode = proto.TaskFailed, proto.ErrOutput, "upload failed", 0
		} else {
			rep.Outputs = []proto.Output{{Name: "result.json", Blob: sum, Size: int64(len(data))}}
			n.setPhase(st, proto.PhaseUploading, int64(len(data)))
		}
	}
	n.setPhase(st, proto.PhaseReporting, 0)
	n.report(ctx, st, rep)
}

func loadOutput(t proto.Task, size int) []byte {
	b := fmt.Appendf(nil, `{"task":%q,"job":%q,"index":%d,"attempt":%d,"result":"`, t.ID, t.JobID, t.Index, t.Attempt)
	for len(b) < size-2 {
		b = append(b, "0123456789abcdef"[len(b)%16])
	}
	return append(b, '"', '}')
}

// postLog ships one chunk at off; it returns the next offset and whether
// the hive called the lease stale.
func (n *simNode) postLog(ctx context.Context, st *simTask, off int64, size int) (int64, bool) {
	start := int(off % int64(len(loadLogText)-size))
	chunk := loadLogText[start : start+size]
	path := "/api/v1/tasks/" + url.PathEscape(st.t.ID) + "/log?lease=" + url.QueryEscape(st.t.Lease) + "&offset=" + strconv.FormatInt(off, 10)
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, raw, d, err := n.do(lctx, "POST", path, chunk, "text/plain; charset=utf-8", true)
	if ctx.Err() != nil {
		return off, false
	}
	n.sw.rec.add("log chunk", d, code, len(raw), err)
	switch {
	case err != nil:
		return off, false
	case code == 409 || code == 404:
		n.sw.count("log_stale", 1)
		return off, true
	case code != 200:
		return off, false
	}
	var out struct {
		Next int64 `json:"next"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Next <= off {
		return off + int64(size), false
	}
	return out.Next, false
}

// report sends the final report like the agent's outbox: retried every
// 3 s until the hive answers 2xx, 404 or 409.
func (n *simNode) report(ctx context.Context, st *simTask, rep proto.TaskReport) {
	body, _ := json.Marshal(rep)
	path := "/api/v1/tasks/" + url.PathEscape(st.t.ID) + "/report"
	for ctx.Err() == nil {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		code, raw, d, err := n.do(rctx, "POST", path, body, "application/json", true)
		cancel()
		if ctx.Err() != nil {
			break
		}
		n.sw.rec.add("report", d, code, len(raw), err)
		switch {
		case err == nil && code/100 == 2:
			n.sw.count("reports_ok", 1)
			if rep.State == proto.TaskSucceeded {
				n.sw.count("tasks_succeeded", 1)
			}
			n.drop(st)
			return
		case err == nil && (code == 409 || code == 404):
			n.sw.count("reports_stale", 1)
			n.drop(st)
			return
		case err == nil && code == 401:
			n.reregister(ctx)
		default:
			n.sw.count("report_retries", 1)
		}
		sleepCtx(ctx, 3*time.Second)
	}
	n.drop(st)
}

// --- the dashboard ---

// dashboard polls what the web UI polls every 5 s (internal/hive/web/static/js):
// the overview (stats + info), the nodes page, the jobs page (50 newest)
// and an open job's detail page (job + its first 100 tasks), plus a deep
// task page with a state filter, as one browser tab each.
func (sw *simSwarm) dashboard(ctx context.Context) {
	tr := &http.Transport{TLSClientConfig: auth.ClientTLSConfig(sw.p.Fingerprint, nil), MaxIdleConnsPerHost: 8}
	hc := &http.Client{Transport: tr, Timeout: 120 * time.Second}
	iv := time.Duration(sw.p.DashSeconds * float64(time.Second))
	get := func(ep, path string, out any) bool {
		req, _ := http.NewRequestWithContext(ctx, "GET", sw.p.HiveURL+"/api/v1"+path, nil)
		req.Header.Set("Authorization", "Bearer "+sw.p.AdminToken)
		start := time.Now()
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				sw.rec.add(ep, time.Since(start), 0, 0, err)
			}
			return false
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if ctx.Err() != nil {
			return false
		}
		sw.rec.add(ep, time.Since(start), resp.StatusCode, len(raw), err)
		if err == nil && resp.StatusCode == 200 && out != nil {
			return json.Unmarshal(raw, out) == nil
		}
		return err == nil && resp.StatusCode == 200
	}
	tab := func(fn func()) {
		sw.wg.Add(1)
		go func() {
			defer sw.wg.Done()
			t := time.NewTicker(iv)
			defer t.Stop()
			for {
				fn()
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	tab(func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); get("admin stats", "/stats", nil) }()
		go func() { defer wg.Done(); get("admin info", "/admin/info", nil) }()
		wg.Wait()
	})
	tab(func() { get("admin nodes", "/admin/nodes", nil) })
	tab(func() { get("admin jobs (50)", "/admin/jobs?limit=50", nil) })
	tab(func() {
		var jobs []proto.JobView
		if !get("admin jobs (newest)", "/admin/jobs?limit=1", &jobs) || len(jobs) == 0 {
			return
		}
		p := "/admin/jobs/" + jobs[0].ID
		get("admin job detail", p, nil)
		var page proto.TaskPage
		get("admin tasks page 1", p+"/tasks?offset=0&limit=100", &page)
		deep := max(page.Total-100, 0)
		get("admin tasks deep+filter", p+"/tasks?state=succeeded&offset="+strconv.Itoa(deep/2)+"&limit=100", nil)
	})
}
