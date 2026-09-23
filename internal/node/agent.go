// Package node implements the SaviorOS node agent (`savior node`) and the
// text console (`savior console`). See docs/DESIGN.md section 10.
package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
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
	// OpenDisplay overrides framebuffer selection (tests use memory devices).
	// When nil and the display role is active, the configured device is used.
	OpenDisplay func() (display.Device, error)
	// NoDisplay disables the display controller even with the display role.
	NoDisplay bool
	// ManageSystem lets the agent change the machine: set the clock and
	// hostname, reboot/power off. True on SaviorOS; false in tests.
	ManageSystem bool
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
	disp        *display.Controller
	urlf        *display.URLFetcher

	// shuttingDown is set once shutdownTasks starts: finishing tasks then
	// skip the immediate report flush so the agent can stop promptly.
	shuttingDown atomic.Bool

	mu           sync.Mutex
	inv          proto.Inventory
	roles        []proto.Role
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
	a.roles = cfg.EffectiveRoles(hwinfo.HasDisplay(opt.SysRoot) || opt.OpenDisplay != nil)

	if cfg.SwarmKey != "" {
		a.secret = auth.NewSwarmSecret(cfg.SwarmKey)
	}
	if proto.HasRole(a.roles, proto.RoleDisplay) && !opt.NoDisplay {
		a.setupDisplay()
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
		// Shadow, back and next-slide buffers plus the frame cache.
		fbMB := 0
		for _, fb := range a.inv.Framebuffers {
			if b := fb.Width * fb.Height * 4 * 3 >> 20; b > fbMB {
				fbMB = b
			}
		}
		budget -= fbMB + int(display.DefaultCacheBytes()>>20)
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
	if a.disp != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.disp.Run(ctx); err != nil && ctx.Err() == nil {
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
		a.session(ctx, hc)
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
// the hive forgets us (401), we lose it for too long, or ctx ends.
func (a *Agent) session(ctx context.Context, hc *hiveClient) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer hc.close()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.heartbeatLoop(sctx, hc, cancel) }()
	go func() { defer wg.Done(); a.claimLoop(sctx, hc) }()
	go func() { defer wg.Done(); a.outboxLoop(sctx, hc) }()
	wg.Wait()
}

// transfer adapts the current hive session for the runner.
type transfer struct{ a *Agent }

func (t transfer) client() (*hiveClient, error) {
	t.a.mu.Lock()
	defer t.a.mu.Unlock()
	if t.a.hc == nil {
		return nil, errors.New("not connected to the hive")
	}
	return t.a.hc, nil
}

func (t transfer) FetchBlob(ctx context.Context, sha string, w io.Writer) (int64, error) {
	c, err := t.client()
	if err != nil {
		return 0, err
	}
	return c.FetchBlob(ctx, sha, w)
}

func (t transfer) FetchURL(ctx context.Context, u string, maxBytes int64, w io.Writer) (int64, error) {
	return fetchURL(ctx, u, maxBytes, w)
}

func (t transfer) UploadBlob(ctx context.Context, sha string, size int64, r io.Reader) error {
	c, err := t.client()
	if err != nil {
		return err
	}
	return c.UploadBlob(ctx, sha, size, r)
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
