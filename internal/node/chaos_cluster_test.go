//go:build linux

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/hive"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/runner"
)

// This file runs the chaos soak's cluster: one real hive (restartable from
// its data dir) and real node agents (sandbox = none) that talk to it only
// through a fault-injecting HTTPS proxy (chaos_proxy_test.go).

// chaosCluster is the system under test.
type chaosCluster struct {
	t       *testing.T
	cfg     chaosConfig
	start   time.Time
	base    string // searchable temp dir for everything
	hiveDir string
	addr    string // the hive's fixed listen address
	fp      string
	token   string // admin token
	admin   *http.Client
	proxy   *faultProxy
	nodes   []*chaosNode
	ev      *chaosEvents
	viol    *chaosViolations
	checker *chaosChecker

	hiveClockOff atomic.Int64 // ns added to the hive's wall clock
	admin429     atomic.Int64 // admin requests refused as rate limited

	// writeMu orders admin writes against simulated crashes: a write holds
	// it shared from its request until its answer, a crash exclusively, so
	// no acknowledgement falls between the crash and the restart.
	writeMu sync.RWMutex
	crashes []chaosCrash

	mu        sync.Mutex
	srv       *hive.Server
	hiveStop  context.CancelFunc
	hiveDone  chan struct{}
	hiveEpoch int // bumped on every (re)start
	hiveUp    bool
	logFiles  []*os.File
}

const chaosAdminToken = "chaos-admin-token-0123456789abcdefghijklmn"

// hiveConfig for the chaos cluster: the integration test timings, a
// longer recovery window (nodes back off while the hive is away) and a
// movable wall clock.
func (c *chaosCluster) hiveConfig(ln net.Listener) hive.Config {
	return hive.Config{
		DataDir:           c.hiveDir,
		SwarmKey:          testKey,
		AdminToken:        chaosAdminToken,
		Listener:          ln,
		StatusFile:        "-",
		HeartbeatInterval: time.Second,
		OfflineAfter:      4 * time.Second,
		LostAfter:         3 * time.Second,
		MissingAfter:      6 * time.Second,
		RecoveryWindow:    15 * time.Second,
		ReserveAfter:      10 * time.Second,
		Clock:             func() time.Time { return time.Now().Add(time.Duration(c.hiveClockOff.Load())) },
		Log:               c.logger("hive"),
	}
}

func newChaosCluster(t *testing.T, cfg chaosConfig) *chaosCluster {
	base := searchableTempDir(t)
	c := &chaosCluster{t: t, cfg: cfg, start: time.Now(), base: base, hiveDir: filepath.Join(base, "hive"), token: chaosAdminToken}
	c.ev = newChaosEvents(c.start, cfg.logDir)
	c.viol = newChaosViolations(c)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.addr = ln.Addr().String()
	if err := c.startHiveOn(ln); err != nil {
		t.Fatal(err)
	}
	c.admin = &http.Client{Transport: &http.Transport{TLSClientConfig: auth.ClientTLSConfig(c.fp, nil), MaxIdleConnsPerHost: 8}, Timeout: 30 * time.Second}
	c.proxy = newFaultProxy(c)
	for i := 0; i < cfg.nodes; i++ {
		n, err := c.newNode(i)
		if err != nil {
			t.Fatal(err)
		}
		c.nodes = append(c.nodes, n)
	}
	return c
}

// logger returns a debug logger writing to <logDir>/<name>.log, or a
// discarding one.
func (c *chaosCluster) logger(name string) *slog.Logger {
	if c.cfg.logDir == "" {
		return testLogger().With("c", name)
	}
	f, err := os.OpenFile(filepath.Join(c.cfg.logDir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	c.logFiles = append(c.logFiles, f)
	c.mu.Unlock()
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (c *chaosCluster) closeLogs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.logFiles {
		f.Close()
	}
	c.logFiles = nil
}

// --- hive lifecycle ---

func (c *chaosCluster) startHiveOn(ln net.Listener) error {
	srv, err := hive.New(c.hiveConfig(ln))
	if err != nil {
		ln.Close()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Run(ctx)
	}()
	c.mu.Lock()
	c.srv, c.hiveStop, c.hiveDone = srv, cancel, done
	c.hiveEpoch++
	c.hiveUp = true
	c.fp = srv.Fingerprint()
	c.mu.Unlock()
	return nil
}

// startHive restarts the hive on its old address from its data dir.
func (c *chaosCluster) startHive() error {
	var ln net.Listener
	var err error
	for i := 0; i < 100; i++ {
		if ln, err = net.Listen("tcp", c.addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	if err := c.startHiveOn(ln); err != nil {
		return err
	}
	c.ev.add("hive started (epoch %d)", c.epoch())
	return nil
}

// stopHive shuts the hive down gracefully (it saves its state).
func (c *chaosCluster) stopHive() {
	c.mu.Lock()
	stop, done, up := c.hiveStop, c.hiveDone, c.hiveUp
	c.hiveUp = false
	c.mu.Unlock()
	if !up {
		return
	}
	t0 := time.Now()
	stop()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		c.viol.add("hive-stop-hang", "the hive did not stop within 60 s")
		<-done
	}
	c.ev.add("hive stopped in %s", time.Since(t0).Round(time.Millisecond))
}

// chaosCrash is a simulated hive crash (power cut): the hive came back
// from the state it had saved by persisted.
type chaosCrash struct {
	at, persisted time.Time // when the hive was down; when the state it came back from was written
}

// crashHive simulates a power cut of the hive machine: it restarts from
// the state files that were on disk at the crash (state.json, the settled
// jobs' files in jobs/, live.json), losing what the hive changed and
// acknowledged since state.json was written (reports, claims: only admin
// writes such as job submissions and cancels and node and wall patches are
// saved before they are answered, DESIGN 9). Blobs and log tails written
// since stay on disk, as after a real crash.
func (c *chaosCluster) crashHive(down time.Duration) (chaosCrash, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	cur := filepath.Join(c.hiveDir, "state.json")
	data, err := os.ReadFile(cur)
	if err != nil {
		return chaosCrash{}, err
	}
	fi, err := os.Stat(cur)
	if err != nil {
		return chaosCrash{}, err
	}
	// After state.json: the hive writes a settled job's file before the
	// state.json that leaves the job out, and removes a deleted job's file
	// only after the state.json without it, so files read now are
	// consistent with that state.json.
	files, err := readStateFiles(c.hiveDir)
	if err != nil {
		return chaosCrash{}, err
	}
	cr := chaosCrash{persisted: fi.ModTime()}
	// The old hive serves until it has stopped (in-flight requests, up to
	// its 5 s shutdown timeout); all of that is lost with the file swap, so
	// the crash happens when it is down.
	c.stopHive()
	cr.at = time.Now()
	if err := os.WriteFile(cur, data, 0o600); err != nil {
		return cr, err
	}
	if err := restoreStateFiles(c.hiveDir, files); err != nil {
		return cr, err
	}
	time.Sleep(down)
	c.mu.Lock()
	c.crashes = append(c.crashes, cr)
	c.mu.Unlock()
	// Forget what the crash may have taken before anyone looks at the new
	// hive.
	c.proxy.forgetAfter(cr)
	if c.checker != nil {
		c.checker.forgetAfter(cr)
	}
	if err := c.startHive(); err != nil {
		return cr, err
	}
	return cr, nil
}

// readStateFiles reads the hive's state files besides state.json:
// live.json and jobs/*.json (relative path -> content).
func readStateFiles(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	names, err := filepath.Glob(filepath.Join(dir, "jobs", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, p := range append(names, filepath.Join(dir, "live.json")) {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue // removed meanwhile, or no live.json yet
		}
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = b
	}
	return out, nil
}

// restoreStateFiles puts back what readStateFiles read and removes the
// state files written since.
func restoreStateFiles(dir string, files map[string][]byte) error {
	now, err := filepath.Glob(filepath.Join(dir, "jobs", "*.json"))
	if err != nil {
		return err
	}
	for _, p := range append(now, filepath.Join(dir, "live.json")) {
		if rel, _ := filepath.Rel(dir, p); files[rel] == nil {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	for rel, b := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (c *chaosCluster) isHiveUp() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hiveUp
}

func (c *chaosCluster) epoch() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hiveEpoch
}

// --- admin API (non-fatal: the hive may be down) ---

func (c *chaosCluster) adminDo(ctx context.Context, method, path string, in, out any) (int, []byte, error) {
	var body io.Reader
	ctype := "application/json"
	switch in := in.(type) {
	case nil:
	case rawBody:
		body, ctype = bytes.NewReader(in), "application/octet-stream"
	default:
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.addr+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", ctype)
	resp, err := c.admin.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, b, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return resp.StatusCode, b, nil
}

func (c *chaosCluster) adminGet(path string, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, b, err := c.adminDo(ctx, http.MethodGet, path, nil, out)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return &HTTPError{Status: code, Msg: strings.TrimSpace(string(b))}
	}
	return nil
}

// adminRetry repeats a write until the hive answers it (it may be
// restarting); the answer is the acknowledgement the invariants rely on.
func (c *chaosCluster) adminRetry(method, path string, in, out any, within time.Duration) (int, error) {
	if method != http.MethodGet {
		c.writeMu.RLock()
		defer c.writeMu.RUnlock()
	}
	deadline := time.Now().Add(within)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		code, b, err := c.adminDo(ctx, method, path, in, out)
		cancel()
		if code == http.StatusTooManyRequests {
			// The admin never sends wrong credentials, and node requests
			// (the proxy's address is ours too) must not count as failed
			// admin logins, stale node tokens included: a violation (see
			// the end of runChaos).
			c.ev.add("admin %s %s: 429 %s", method, path, strings.TrimSpace(string(b)))
			c.admin429.Add(1)
		}
		if err == nil && code < 500 && code != http.StatusTooManyRequests {
			if code >= 300 {
				return code, fmt.Errorf("%s %s: %d %s", method, path, code, strings.TrimSpace(string(b)))
			}
			return code, nil
		}
		if time.Now().After(deadline) {
			return code, fmt.Errorf("%s %s: no answer within %s: %v", method, path, within, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// --- nodes ---

// chaosNode is one machine: a stable identity, its own fixture /sys (with a
// battery and a thermal zone the harness controls), work dirs and uid range.
type chaosNode struct {
	c       *chaosCluster
	idx     int
	id      string
	uidBase int
	dir     string
	sysRoot string
	log     *slog.Logger

	clockOff atomic.Int64 // ns added to the node's sampler clock
	// disturb is bumped whenever the node's link to the hive is cut or
	// restored on purpose (agent restart, partition); disagreements with
	// the hive are timed from the last disturbance.
	disturb atomic.Int64

	mu     sync.Mutex
	agent  *Agent
	cancel context.CancelFunc
	done   chan struct{}
	starts int
	power  string // ac | battery | lowbatt | hot
	up     bool
}

func (c *chaosCluster) newNode(i int) (*chaosNode, error) {
	n := &chaosNode{c: c, idx: i, id: fmt.Sprintf("chaos-%d", i), uidBase: c.cfg.uidBase + 100*i, power: "ac"}
	n.dir = filepath.Join(c.base, n.id)
	n.sysRoot = filepath.Join(n.dir, "root")
	if err := copyTree("../hwinfo/testdata/qemu", n.sysRoot); err != nil {
		return nil, err
	}
	if err := n.setPower("ac"); err != nil {
		return nil, err
	}
	n.log = c.logger(n.id)
	return n, nil
}

// copyTree copies a fixture tree, keeping symlinks as symlinks.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, out)
		case fi.IsDir():
			return os.MkdirAll(out, 0o755)
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(out, b, 0o644)
		}
	})
}

func writeAttr(path, val string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(val+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// setPower writes the node's battery and thermal sensors (DESIGN 10.4):
//
//	ac      on mains, battery charging at 90 %, CPU 45 C: accept work
//	battery on battery at 80 %: accept nothing (run_on_battery = no)
//	lowbatt on battery at 10 % (< battery_min_percent 40): preempt
//	hot     CPU 95 C (>= max_temp_c 85): pause (freeze) tasks
//
// The agent's power loop reads them every 5 s.
func (n *chaosNode) setPower(state string) error {
	ps := filepath.Join(n.sysRoot, "sys/class/power_supply")
	tz := filepath.Join(n.sysRoot, "sys/class/thermal/thermal_zone0")
	online, status, capacity, temp := "1", "Charging", "90", "45000"
	switch state {
	case "battery":
		online, status, capacity = "0", "Discharging", "80"
	case "lowbatt":
		online, status, capacity = "0", "Discharging", "10"
	case "hot":
		temp = "95000"
	}
	// A reading that never changes counts as a stuck sensor after 10
	// minutes; keep it moving a little.
	t, _ := strconv.Atoi(temp)
	temp = strconv.Itoa(t + int(time.Now().UnixNano()/1e6%500))
	for _, kv := range [][2]string{
		{ps + "/AC/type", "Mains"}, {ps + "/AC/online", online},
		{ps + "/BAT0/type", "Battery"}, {ps + "/BAT0/present", "1"},
		{ps + "/BAT0/status", status}, {ps + "/BAT0/capacity", capacity},
		{tz + "/type", "acpitz"}, {tz + "/temp", temp},
	} {
		if err := writeAttr(kv[0], kv[1]); err != nil {
			return err
		}
	}
	n.mu.Lock()
	n.power = state
	n.mu.Unlock()
	return nil
}

// newAgent builds an agent like startNode does, except that its task
// runner uses this node's own uid range: every agent in this process
// shares the machine, and the runner kills all processes of its slot uids
// when it starts (DESIGN 10.5), which with the fixed uid base would kill
// the other agents' tasks.
func (n *chaosNode) newAgent() (*Agent, error) {
	c := n.c
	cfg := config.Default()
	cfg.NodeID = n.id
	cfg.Name = n.id
	cfg.SwarmKey = testKey
	cfg.Hive = c.proxy.url()
	cfg.Sandbox = "none"
	// No compute role yet: New would start a runner with the fixed uid base.
	cfg.Roles = []string{"display"}
	opt := Options{
		Config:        cfg,
		Log:           n.log,
		SysRoot:       n.sysRoot,
		WorkRoot:      filepath.Join(n.dir, "work"),
		CacheDir:      filepath.Join(n.dir, "cache"),
		CgroupRoot:    filepath.Join(n.dir, "no-cgroup"),
		StatusFile:    filepath.Join(n.dir, "status.json"),
		NoDisplay:     true,
		SkipBenchmark: true,
		MinHeartbeat:  time.Second,
	}
	a, err := New(opt)
	if err != nil {
		return nil, err
	}
	a.sampler.SetClock(func() time.Time { return time.Now().Add(time.Duration(n.clockOff.Load())) })
	// setupRunner, with a per-node uid base.
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	slots := min(max(int(math.Ceil(a.total.Cores))*2, 2), 16)
	r, err := runner.New(runner.Config{
		WorkRoot:     opt.WorkRoot,
		CacheDir:     opt.CacheDir,
		CgroupRoot:   opt.CgroupRoot,
		SelfExe:      self,
		Sandbox:      "none",
		ScratchInRAM: a.scratchInRAM,
		UIDBase:      n.uidBase,
		Slots:        slots,
		Log:          n.log.With("component", "runner"),
	}, transfer{a})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.runner, a.runnerSlots = r, slots
	mode, caps, _ := r.Caps()
	a.sandboxMode, a.sandboxCaps = mode, caps
	a.roles = []proto.Role{proto.RoleCompute}
	a.mu.Unlock()
	return a, nil
}

func (n *chaosNode) startAgent() error {
	a, err := n.newAgent()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	n.mu.Lock()
	n.agent, n.cancel, n.done, n.up = a, cancel, done, true
	n.starts++
	starts := n.starts
	n.mu.Unlock()
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	n.c.ev.add("node %s started (#%d, boot %s)", n.id, starts, a.bootID)
	return nil
}

// stopAgent stops the agent like a process exit: its tasks are canceled
// and their reports never sent.
func (n *chaosNode) stopAgent() {
	n.mu.Lock()
	cancel, done, up := n.cancel, n.done, n.up
	n.up = false
	n.mu.Unlock()
	if !up {
		return
	}
	t0 := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		n.c.viol.add("agent-stop-hang", "agent %s did not stop within 60 s", n.id)
		<-done
	}
	n.mu.Lock()
	n.agent = nil // let its runner's files be finalized
	n.mu.Unlock()
	n.c.ev.add("node %s stopped in %s", n.id, time.Since(t0).Round(time.Millisecond))
}

func (n *chaosNode) disturbance(hiveEpoch int) int {
	return hiveEpoch<<20 + int(n.disturb.Load())
}

func (n *chaosNode) current() (*Agent, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.agent, n.up
}

// heldTask is the node's view of one held assignment.
type chaosHeld struct {
	id, lease, job string
	attempt        int
	reporting      bool // final report waiting in the outbox
	phase          string
}

// held lists the agent's assignments.
func (a *Agent) chaosHeld() []chaosHeld {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]chaosHeld, 0, len(a.tasks))
	for lease, t := range a.tasks {
		t.mu.Lock()
		out = append(out, chaosHeld{id: t.t.ID, lease: lease, job: t.t.JobID, attempt: t.t.Attempt, reporting: t.report != nil, phase: t.phase})
		t.mu.Unlock()
	}
	return out
}

// chaosCapacity reports what the agent offers and what its runner has
// free, for the capacity-leak check.
func (a *Agent) chaosCapacity() (total, free proto.Resources, freeSlots, slots int, link proto.HiveLink) {
	a.mu.Lock()
	defer a.mu.Unlock()
	total, free, link = a.total, a.freeLocked(), a.link
	if a.runner != nil {
		freeSlots = a.runner.FreeSlots()
	}
	return total, free, freeSlots, a.runnerSlots, link
}

// --- events and violations ---

type chaosEvents struct {
	start time.Time
	mu    sync.Mutex
	lines []string
	f     *os.File
}

func newChaosEvents(start time.Time, dir string) *chaosEvents {
	e := &chaosEvents{start: start}
	if dir != "" {
		e.f, _ = os.OpenFile(filepath.Join(dir, "events.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	return e
}

func (e *chaosEvents) add(format string, args ...any) {
	line := fmt.Sprintf("%8.2fs %s", time.Since(e.start).Seconds(), fmt.Sprintf(format, args...))
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lines = append(e.lines, line)
	if len(e.lines) > 4000 {
		e.lines = e.lines[len(e.lines)-3000:]
	}
	if e.f != nil {
		fmt.Fprintln(e.f, line)
	}
}

func (e *chaosEvents) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f != nil {
		e.f.Close()
		e.f = nil
	}
}

func (e *chaosEvents) tail(n int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.lines) < n {
		n = len(e.lines)
	}
	return append([]string(nil), e.lines[len(e.lines)-n:]...)
}

// chaosViolations collects invariant violations; any one fails the run.
type chaosViolations struct {
	c    *chaosCluster
	mu   sync.Mutex
	list []chaosViolation
	seen map[string]int
	keys map[string]bool
}

type chaosViolation struct {
	id  string
	at  time.Duration
	msg string
}

func newChaosViolations(c *chaosCluster) *chaosViolations {
	return &chaosViolations{c: c, seen: map[string]int{}, keys: map[string]bool{}}
}

// add records a violation of invariant id. At most 20 are kept per id.
func (v *chaosViolations) add(id string, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	v.mu.Lock()
	v.seen[id]++
	keep := v.seen[id] <= 20
	if keep {
		v.list = append(v.list, chaosViolation{id: id, at: time.Since(v.c.start), msg: msg})
	}
	v.mu.Unlock()
	v.c.ev.add("VIOLATION %s: %s", id, msg)
}

// once records a violation only the first time key is seen (a task that
// stays wrong is reported once).
func (v *chaosViolations) once(key, id string, format string, args ...any) {
	v.mu.Lock()
	dup := v.keys[id+"|"+key]
	v.keys[id+"|"+key] = true
	v.mu.Unlock()
	if !dup {
		v.add(id, format, args...)
	}
}

func (v *chaosViolations) snapshot() ([]chaosViolation, map[string]int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	counts := map[string]int{}
	for k, n := range v.seen {
		counts[k] = n
	}
	return append([]chaosViolation(nil), v.list...), counts
}
