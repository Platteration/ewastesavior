// Package node implements the SaviorOS node agent (`savior node`) and the
// text console (`savior console`). See docs/DESIGN.md section 10.
package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/config"
	"github.com/platteration/ewastesavior/internal/display"
	"github.com/platteration/ewastesavior/internal/hwinfo"
	"github.com/platteration/ewastesavior/internal/power"
	"github.com/platteration/ewastesavior/internal/proto"
	"github.com/platteration/ewastesavior/internal/runner"
	"github.com/platteration/ewastesavior/internal/version"
)

// Options holds everything that differs between a real node and a test.
type Options struct {
	Config config.Config
	Log    *slog.Logger

	// SysRoot is where /proc and /sys are read from ("" = "/").
	SysRoot string
	// Paths used by the task runner.
	WorkRoot, CacheDir, CgroupRoot, SelfExe string
	// StatusFile is written every heartbeat for `savior console` ("" = none).
	StatusFile string
	// HivePanelFile is a local hive's status panel ("" =
	// /run/savior/hive-status.json).
	HivePanelFile string
	// OpenDisplay overrides framebuffer selection (tests use memory devices).
	// When nil and the display role is active, the configured device is used.
	OpenDisplay func() (display.Device, error)
	// NoDisplay disables the display controller even with the display role.
	NoDisplay bool
	// DisplayProbe reports whether a display device exists, for
	// roles=auto (DESIGN 5.3). Nil = hwinfo.HasDisplay(SysRoot), and a set
	// OpenDisplay counts as a display. Tests use it with OpenDisplay to make
	// a display appear after start.
	DisplayProbe func() bool
	// DisplayPoll is how often roles=auto looks for a display that appeared
	// after start (0 = 5 s).
	DisplayPoll time.Duration
	// ManageSystem lets the agent change the machine: set the clock and
	// hostname, reboot/power off. True on SaviorOS; false in tests.
	ManageSystem bool
	// SystemAction, when set, carries out the hive's reboot and poweroff
	// actions instead of /sbin/reboot and /sbin/poweroff, with or without
	// ManageSystem (tests).
	SystemAction func(action string) error
	// Discovery overrides (tests).
	DiscoveryPort    int
	DiscoveryTargets []string
	// SkipBenchmark skips the startup CPU benchmark (tests).
	SkipBenchmark bool
	// Timing overrides (tests); zero = defaults.
	MinHeartbeat time.Duration
}

// taskRunner is the part of *runner.Runner the agent uses after setup
// (tests substitute a fake).
type taskRunner interface {
	FreeSlots() int
	Run(ctx context.Context, t proto.Task, logs io.Writer, progress func(proto.RunningTask)) proto.TaskReport
	Freeze(lease string, frozen bool) error
	Preempt(lease string) error
}

// Agent is a running node.
type Agent struct {
	opt    Options
	cfg    config.Config
	log    *slog.Logger
	id     hwinfo.Identity
	bootID string
	secret auth.Secret

	sampler     *hwinfo.Sampler
	sampleMu    sync.Mutex
	runner      taskRunner // nil without the compute role
	runnerSlots int        // the runner's concurrency (uid slots)
	urlf        *display.URLFetcher
	// autoDisplay: roles=auto without a display at start; Run watches for
	// one to appear (DESIGN 5.3).
	autoDisplay bool

	// shuttingDown is set once shutdownTasks starts: finishing tasks then
	// skip the immediate report flush so the agent can stop promptly.
	shuttingDown atomic.Bool

	mu           sync.Mutex
	disp         *display.Controller // nil without the display role; set once
	inv          proto.Inventory
	roles        []proto.Role
	rolesRev     int                // bumped when roles change after start
	endSession   context.CancelFunc // ends the current hive session, if any
	total        proto.Resources
	scratchInRAM bool
	memBudgetMB  int
	sandboxMode  string
	sandboxCaps  []string

	// Hive session state (guarded by mu).
	hc          *hiveClient
	hiveAddr    string
	link        proto.HiveLink
	linkErr     string
	name        string
	shortCode   string
	directives  proto.Directives
	pending     bool
	clockOffset time.Duration
	hiveSynced  bool
	hbInterval  time.Duration
	blacklist   map[string]time.Time
	unreachable map[string]time.Duration // current backoff per unreachable hive
	lastMetrics proto.Metrics
	decision    power.Decision
	displayRev  int64
	rotate      int
	sessionGen  int

	tasks       map[string]*task // by lease
	actionsDone map[string]bool
	ackPending  []string

	wake chan struct{} // capacity may have changed: re-check claim loop
}

// New prepares an agent: hardware inventory, identity, runner, display.
func New(opt Options) (*Agent, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("savior node runs on Linux (SaviorOS); use savior hive / savior ctl on this system")
	}
	cfg := opt.Config
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	a := &Agent{
		opt:         opt,
		cfg:         cfg,
		log:         log,
		link:        proto.LinkSearching,
		blacklist:   map[string]time.Time{},
		tasks:       map[string]*task{},
		actionsDone: map[string]bool{},
		wake:        make(chan struct{}, 1),
		hbInterval:  5 * time.Second,
		rotate:      cfg.DisplayRotate,
	}
	id, err := hwinfo.GetIdentity(opt.SysRoot)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	if cfg.NodeID != "" {
		id.NodeID = cfg.NodeID
		id.Stable = true
	}
	if !id.Stable {
		log.Warn("node identity is not stable across reboots (no usable NIC MAC, DMI UUID or serial)")
	}
	a.id = id
	a.bootID = hwinfo.BootID(opt.SysRoot) + ":" + auth.NewID(4)
	a.name = cfg.Name
	if a.name == "" {
		a.name = hwinfo.DefaultName(id)
	}
	a.shortCode = proto.ShortCode(id.NodeID)

	inv, err := hwinfo.Collect(opt.SysRoot)
	if err != nil {
		log.Warn("hardware inventory incomplete", "err", err)
	}
	if !opt.SkipBenchmark {
		inv.BenchScore = hwinfo.Benchmark(300 * time.Millisecond)
	}
	a.inv = inv
	a.sampler = hwinfo.NewSampler(opt.SysRoot, float64(cfg.MaxTempC))
	a.roles = cfg.EffectiveRoles(a.displayPresent())
	a.autoDisplay = !opt.NoDisplay && !proto.HasRole(a.roles, proto.RoleDisplay) &&
		proto.HasRole(cfg.EffectiveRoles(true), proto.RoleDisplay)

	if cfg.SwarmKey != "" {
		a.secret = auth.NewSwarmSecret(cfg.SwarmKey)
	}
	if proto.HasRole(a.roles, proto.RoleDisplay) && !opt.NoDisplay {
		a.disp = a.newDisplay()
	}
	a.computeCapacity()
	if proto.HasRole(a.roles, proto.RoleCompute) {
		if err := a.setupRunner(); err != nil {
			log.Error("task runner unavailable; compute role disabled", "err", err)
			a.roles = without(a.roles, proto.RoleCompute)
		}
	}
	return a, nil
}

func without(roles []proto.Role, r proto.Role) []proto.Role {
	var out []proto.Role
	for _, x := range roles {
		if x != r {
			out = append(out, x)
		}
	}
	return out
}

// withRole adds r to roles, keeping the order compute, display, hive.
func withRole(roles []proto.Role, r proto.Role) []proto.Role {
	var out []proto.Role
	for _, x := range []proto.Role{proto.RoleCompute, proto.RoleDisplay, proto.RoleHive} {
		if x == r || proto.HasRole(roles, x) {
			out = append(out, x)
		}
	}
	return out
}

// displayPresent reports whether this machine has a display device.
func (a *Agent) displayPresent() bool {
	if a.opt.DisplayProbe != nil {
		return a.opt.DisplayProbe()
	}
	return hwinfo.HasDisplay(a.opt.SysRoot) || a.opt.OpenDisplay != nil
}

func (a *Agent) setupRunner() error {
	self := a.opt.SelfExe
	if self == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		self = exe
	}
	slots := int(math.Ceil(a.total.Cores)) * 2
	if slots < 2 {
		slots = 2
	}
	if slots > 16 {
		slots = 16
	}
	r, err := runner.New(runner.Config{
		WorkRoot:     orDefault(a.opt.WorkRoot, "/var/lib/savior/work"),
		CacheDir:     orDefault(a.opt.CacheDir, "/var/cache/savior/blobs"),
		CgroupRoot:   orDefault(a.opt.CgroupRoot, "/sys/fs/cgroup/savior"),
		SelfExe:      self,
		Sandbox:      a.cfg.Sandbox,
		ScratchInRAM: a.scratchInRAM,
		UIDBase:      10000,
		Slots:        slots,
		Log:          a.log.With("component", "runner"),
	}, transfer{a})
	if err != nil {
		return err
	}
	a.runner, a.runnerSlots = r, slots
	mode, caps, _ := r.Caps()
	a.sandboxMode, a.sandboxCaps = mode, caps
	if err := r.SetCPULimit(a.total.Cores); err != nil {
		a.log.Warn("cannot apply the swarm CPU limit", "err", err)
	}
	return nil
}

// displayReserveMB is the memory the display role keeps from tasks: the
// shadow, back and next-slide buffers plus the frame cache (DESIGN 10.3).
func (a *Agent) displayReserveMB() int {
	fbMB := 0
	for _, fb := range a.inv.Framebuffers {
		if b := fb.Width * fb.Height * 4 * 3 >> 20; b > fbMB {
			fbMB = b
		}
	}
	return fbMB + int(display.DefaultCacheBytes()>>20)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// computeCapacity implements DESIGN 10.3.
func (a *Agent) computeCapacity() {
	avail := hwinfo.MemAvailableMB(a.opt.SysRoot)
	budget := avail - 48
	if a.disp != nil {
		budget -= a.displayReserveMB()
	}
	if budget < 0 {
		budget = 0
	}
	a.memBudgetMB = budget
	cores := float64(a.inv.Cores)
	if cores < 1 {
		cores = 1
	}
	c := math.Floor(cores*float64(a.cfg.MaxCPUPercent)/100*20) / 20
	if c < 0.1 {
		c = 0.1
	}
	a.total = proto.Resources{Cores: c, MemMB: budget * a.cfg.MaxMemPercent / 100}
	a.scratchInRAM = a.cfg.Scratch == "ram"
	if !a.scratchInRAM {
		if free, err := freeDiskMB(orDefault(a.opt.WorkRoot, "/var/lib/savior/work")); err == nil {
			a.total.DiskMB = free * 9 / 10
		}
	}
}

// now returns hive-adjusted time.
func (a *Agent) now() time.Time {
	a.mu.Lock()
	off := a.clockOffset
	a.mu.Unlock()
	return time.Now().Add(off)
}

// setLink records the hive link state (shown on screen and console).
func (a *Agent) setLink(l proto.HiveLink, addr, errMsg string) {
	a.mu.Lock()
	changed := a.link != l
	a.link, a.linkErr = l, errMsg
	if addr != "" {
		a.hiveAddr = addr
	}
	a.mu.Unlock()
	if changed {
		a.log.Info("hive link", "state", string(l), "hive", addr, "detail", errMsg)
	}
	a.writeStatus()
}

// Run runs the agent until ctx is canceled.
func (a *Agent) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	a.mu.Lock()
	disp := a.disp
	a.mu.Unlock()
	if disp != nil || a.autoDisplay {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if disp == nil {
				if disp = a.awaitDisplay(ctx); disp == nil {
					return
				}
			}
			if err := disp.Run(ctx); err != nil && ctx.Err() == nil {
				a.log.Error("display controller stopped", "err", err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.powerLoop(ctx)
	}()
	a.writeStatus()
	backoff := time.Second
	for ctx.Err() == nil {
		a.mu.Lock()
		rolesRev := a.rolesRev
		a.mu.Unlock()
		hc, err := a.join(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			sleep(ctx, backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		a.session(ctx, hc, rolesRev)
	}
	a.shutdownTasks()
	wg.Wait()
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// session runs heartbeat and claim loops against one registered hive until
// the hive forgets us (401), we lose it for too long, our roles change (we
// register again with the new ones), or ctx ends. rolesRev is a.rolesRev
// from before the registration.
func (a *Agent) session(ctx context.Context, hc *hiveClient, rolesRev int) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer hc.close()
	a.mu.Lock()
	stale := a.rolesRev != rolesRev
	a.endSession = cancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.endSession = nil
		a.mu.Unlock()
	}()
	if stale {
		a.log.Info("roles changed while registering; registering again")
		return
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.heartbeatLoop(sctx, hc, cancel) }()
	go func() { defer wg.Done(); a.claimLoop(sctx, hc) }()
	go func() { defer wg.Done(); a.outboxLoop(sctx, hc) }()
	wg.Wait()
}

// awaitDisplay polls for a display device that appears after start
// (roles=auto: "a display that appears later enables the role", DESIGN
// 5.3) and enables the display role when one does. It returns nil when ctx
// ends first.
func (a *Agent) awaitDisplay(ctx context.Context) *display.Controller {
	poll := a.opt.DisplayPoll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if a.displayPresent() {
			return a.enableDisplay()
		}
	}
}

// enableDisplay turns on the display role for a display that appeared
// after start: it creates the controller, keeps the display's memory from
// tasks, and ends the hive session so the node registers again with the
// new role. The caller runs the controller.
func (a *Agent) enableDisplay() *display.Controller {
	a.log.Info("a display appeared; enabling the display role")
	fresh, err := hwinfo.Collect(a.opt.SysRoot)
	disp := a.newDisplay()
	a.mu.Lock()
	if err == nil {
		a.inv.Framebuffers, a.inv.Connectors = fresh.Framebuffers, fresh.Connectors
	}
	a.disp = disp
	a.roles = withRole(a.roles, proto.RoleDisplay)
	a.rolesRev++
	a.memBudgetMB = max(a.memBudgetMB-a.displayReserveMB(), 0)
	a.total.MemMB = a.memBudgetMB * a.cfg.MaxMemPercent / 100
	// The hive's display spec, if one came before, was dropped: apply the
	// next one it sends.
	a.displayRev = 0
	rotate, blank := a.rotate, a.decision.BlankDisplay
	end := a.endSession
	a.mu.Unlock()
	disp.SetRotate(rotate)
	if blank {
		disp.SetBlank("lid", true)
	}
	if end != nil {
		end()
	}
	a.writeStatus()
	return disp
}

// transfer adapts the current hive session for the runner.
type transfer struct{ a *Agent }

var (
	errNoSession    = errors.New("not connected to the hive")
	errStaleSession = errors.New("the hive no longer knows this node's session; waiting for the node to register again")
)

// Blob transfers are retried (DESIGN 10.5): a broken connection, a 5xx or
// 429, no hive session, or a 401 while the node registers again with a
// restarted hive would otherwise fail the task as an input or output node
// error and count against a healthy machine. Retries back off from
// transferBackoff, doubling up to transferBackoffMax, for at most
// transferRetryFor, which is less than the hive's 5 min transfer-stall
// deadline. The hive's other answers (400 hash mismatch, 403 no such
// assignment, 404, 413) fail at once.
var (
	transferBackoff    = time.Second
	transferBackoffMax = 15 * time.Second
	transferRetryFor   = 4 * time.Minute
)

func (t transfer) client() (*hiveClient, error) {
	t.a.mu.Lock()
	defer t.a.mu.Unlock()
	if t.a.hc == nil {
		return nil, errNoSession
	}
	return t.a.hc, nil
}

// transientTransferErr reports whether another attempt may succeed.
func transientTransferErr(err error) bool {
	if code := statusOf(err); code != 0 {
		return code == http.StatusUnauthorized || code == http.StatusTooManyRequests || code >= 500
	}
	var ne netError
	var ue *url.Error // anything net/http's client ran into: dial, TLS, a dropped connection
	return errors.Is(err, errNoSession) || errors.Is(err, errStaleSession) || errors.As(err, &ne) || errors.As(err, &ue)
}

// retry runs op with the current hive session until it succeeds, fails
// for good, ctx ends or transferRetryFor has passed. After a 401 it waits
// for the next session instead of sending the old token again.
func (t transfer) retry(ctx context.Context, what, sha string, op func(*hiveClient) error) error {
	start := time.Now()
	wait := transferBackoff
	var stale *hiveClient
	for attempt := 1; ; attempt++ {
		c, err := t.client()
		if err == nil && c == stale {
			err = errStaleSession
		}
		if err == nil {
			if err = op(c); err == nil {
				return nil
			}
			if statusOf(err) == http.StatusUnauthorized {
				stale = c
			}
		}
		if ctx.Err() != nil || !transientTransferErr(err) || time.Since(start)+wait > transferRetryFor {
			return err
		}
		t.a.log.Info("blob "+what+" failed; retrying", "sha256", sha, "attempt", attempt, "in", wait, "err", err)
		sleep(ctx, wait)
		wait = min(2*wait, transferBackoffMax)
	}
}

// FetchBlob streams a blob into w and verifies its hash. An interrupted
// download resumes where it broke off.
func (t transfer) FetchBlob(ctx context.Context, sha string, w io.Writer) (int64, error) {
	if !proto.ValidSHA256(sha) {
		return 0, errors.New("invalid blob hash")
	}
	h := sha256.New()
	cw := &sizeWriter{w: io.MultiWriter(w, h)}
	err := t.retry(ctx, "download", sha, func(c *hiveClient) error {
		_, err := c.fetchBlob(ctx, sha, cw.n, cw)
		return err
	})
	if err != nil {
		return cw.n, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return cw.n, fmt.Errorf("blob hash mismatch: got %s", got)
	}
	return cw.n, nil
}

// sizeWriter counts what was written through it.
type sizeWriter struct {
	w io.Writer
	n int64
}

func (s *sizeWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.n += int64(n)
	return n, err
}

func (t transfer) FetchURL(ctx context.Context, u string, maxBytes int64, w io.Writer) (int64, error) {
	return fetchURL(ctx, u, maxBytes, w)
}

// UploadBlob uploads size bytes from r as blob sha. Uploads are content
// addressed, so an attempt that reached the hive does no harm when it is
// repeated; r is rewound for each attempt (one attempt only when it can't
// be).
func (t transfer) UploadBlob(ctx context.Context, sha string, size int64, r io.Reader) error {
	rs, ok := r.(io.Seeker)
	if !ok {
		c, err := t.client()
		if err != nil {
			return err
		}
		return c.UploadBlob(ctx, sha, size, r)
	}
	return t.retry(ctx, "upload", sha, func(c *hiveClient) error {
		if _, err := rs.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return c.UploadBlob(ctx, sha, size, r)
	})
}

// statusPath returns the status file path, creating its directory.
func (a *Agent) statusPath() string {
	p := a.opt.StatusFile
	if p != "" {
		os.MkdirAll(filepath.Dir(p), 0o755)
	}
	return p
}

// versionString is the agent version reported to the hive.
func versionString() string { return version.Version }
