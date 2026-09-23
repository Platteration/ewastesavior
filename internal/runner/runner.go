// Package runner executes swarm tasks on a node: it fetches inputs, runs the
// command inside the task sandbox (namespaces, cgroup v2 limits, privilege
// drop, seccomp), enforces the timeout on unfrozen time, and collects and
// uploads outputs. See docs/DESIGN.md sections 6.5, 8.4, 8.5 and 12.
//
// The sandbox shim (`savior sandbox-exec`, SandboxExecMain) runs in the new
// namespaces, builds the task root, drops privileges and execs the command.
// Everything except the portable helpers is Linux-only.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Transfer moves bytes between the node and the hive (or the web, for URL
// inputs). The node agent implements it with its hive session.
type Transfer interface {
	FetchBlob(ctx context.Context, sha256 string, w io.Writer) (int64, error)
	FetchURL(ctx context.Context, url string, maxBytes int64, w io.Writer) (int64, error)
	UploadBlob(ctx context.Context, sha256 string, size int64, r io.Reader) error
}

// Sandbox modes (config key `sandbox`).
const (
	ModeStrict = "strict"
	ModeAuto   = "auto"
	ModeNone   = "none"
)

// Sandbox capability names reported in RegisterRequest.SandboxCaps.
const (
	CapMountNS  = "mountns"
	CapPidNS    = "pidns"
	CapNetNS    = "netns"
	CapIPCNS    = "ipcns"
	CapUTSNS    = "utsns"
	CapCgroup2  = "cgroup2"
	CapSeccomp  = "seccomp"
	CapNNP      = "nnp"
	CapPrivDrop = "privdrop"
)

// AllCaps lists every capability FullIsolation needs, in canonical order.
var AllCaps = []string{CapMountNS, CapPidNS, CapNetNS, CapIPCNS, CapUTSNS, CapCgroup2, CapSeccomp, CapNNP, CapPrivDrop}

// Config configures a Runner.
type Config struct {
	WorkRoot   string // per-task directories (root:root 0711), wiped at start
	CacheDir   string // blob cache (root 0700)
	CgroupRoot string // cgroup2 directory delegated to savior, e.g. /sys/fs/cgroup/savior
	SelfExe    string // savior binary for the sandbox-exec shim ("" = os.Executable)
	Sandbox    string // strict | auto | none ("" = auto)
	// ScratchInRAM mounts a tmpfs of disk_mb on each task dir and charges it
	// to the task's memory limit.
	ScratchInRAM bool
	UIDBase      int // first task uid/gid (10000); slot n runs as UIDBase+n
	Slots        int // max concurrent tasks (= uid slots)
	Log          *slog.Logger

	// CacheMaxMB bounds the blob cache (0 = 64). Blobs larger than half of
	// it are streamed straight into the task directory instead.
	CacheMaxMB int
	// Now is a test hook for the wall clock used in logs ("" = time.Now).
	// Timeouts always use the monotonic clock.
	Now func() time.Time
}

// Exit code of the sandbox shim when it fails before exec. Run does not
// rely on it (the shim reports failures over a status pipe), but it keeps
// such failures recognizable in logs.
const shimFailCode = 125

// Defaults and limits.
const (
	defaultCacheMaxMB = 64
	minUIDBase        = 1000
	maxSlots          = 256
	killWait          = 10 * time.Second // bounded wait for the cgroup to empty
	progressInterval  = time.Second
)

// timeUnit is the length of one timeout second; tests shorten it.
var timeUnit = time.Second

// Runner runs tasks. It is safe for concurrent use.
type Runner struct {
	cfg  Config
	tr   Transfer
	log  *slog.Logger
	now  func() time.Time
	mode string
	caps map[string]bool

	slots *slotPool
	cache *blobCache

	mu    sync.Mutex
	tasks map[string]*taskState // by lease

	sys sysState // platform-specific state (see runner_linux.go)
}

// ErrUnsupported is returned by New on operating systems other than Linux.
var ErrUnsupported = errors.New("the task runner is only supported on Linux")

// New probes the sandbox capabilities, prepares WorkRoot, CacheDir and the
// cgroup parent, and cleans up leftovers of a previous agent (DESIGN 10.5).
func New(cfg Config, tr Transfer) (*Runner, error) {
	if tr == nil {
		return nil, errors.New("runner: nil Transfer")
	}
	switch cfg.Sandbox {
	case "":
		cfg.Sandbox = ModeAuto
	case ModeStrict, ModeAuto, ModeNone:
	default:
		return nil, fmt.Errorf("runner: unknown sandbox mode %q", cfg.Sandbox)
	}
	if cfg.WorkRoot == "" || cfg.CacheDir == "" {
		return nil, errors.New("runner: WorkRoot and CacheDir are required")
	}
	if cfg.Slots < 1 || cfg.Slots > maxSlots {
		return nil, fmt.Errorf("runner: Slots must be 1..%d", maxSlots)
	}
	if cfg.UIDBase < minUIDBase || cfg.UIDBase > 1<<30 {
		return nil, fmt.Errorf("runner: UIDBase must be %d..%d", minUIDBase, 1<<30)
	}
	if cfg.CacheMaxMB <= 0 {
		cfg.CacheMaxMB = defaultCacheMaxMB
	}
	r := &Runner{
		cfg:   cfg,
		tr:    tr,
		log:   cfg.Log,
		now:   cfg.Now,
		mode:  cfg.Sandbox,
		caps:  map[string]bool{},
		slots: newSlotPool(cfg.Slots),
		tasks: map[string]*taskState{},
	}
	if r.log == nil {
		r.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if r.now == nil {
		r.now = time.Now
	}
	if err := r.init(); err != nil {
		return nil, err
	}
	return r, nil
}

// Caps reports the effective sandbox mode, the available isolation
// capabilities (canonical order) and whether they amount to FullIsolation.
func (r *Runner) Caps() (mode string, caps []string, full bool) {
	full = true
	for _, c := range AllCaps {
		if r.caps[c] {
			caps = append(caps, c)
		} else {
			full = false
		}
	}
	return r.mode, caps, full
}

// missingCaps lists the FullIsolation capabilities that are unavailable.
func (r *Runner) missingCaps() []string {
	var out []string
	for _, c := range AllCaps {
		if !r.caps[c] {
			out = append(out, c)
		}
	}
	return out
}

// FreeSlots is the number of tasks that can start without waiting for a
// uid slot. The agent should not claim more tasks than this.
func (r *Runner) FreeSlots() int { return r.slots.free() }

// Running lists the tasks currently inside Run, sorted by task ID.
func (r *Runner) Running() []proto.RunningTask {
	r.mu.Lock()
	out := make([]proto.RunningTask, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t.snapshot())
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Freeze pauses (frozen=true) or resumes a task. While frozen the timeout
// clock is stopped and the phase is "frozen". Freezing a task that is not
// running yet (fetching) or already finishing only records the state.
func (r *Runner) Freeze(lease string, frozen bool) error {
	t := r.lookup(lease)
	if t == nil {
		return fmt.Errorf("runner: no task with lease %q", lease)
	}
	return t.setFrozen(frozen)
}

// Preempt kills a task; Run then reports it as preempted (the hive requeues
// it without consuming an attempt). A task whose process has already exited
// on its own keeps its real result.
func (r *Runner) Preempt(lease string) error {
	t := r.lookup(lease)
	if t == nil {
		return fmt.Errorf("runner: no task with lease %q", lease)
	}
	t.preempt()
	return nil
}

func (r *Runner) lookup(lease string) *taskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks[lease]
}

// register adds t under its lease; it fails for a duplicate lease.
func (r *Runner) register(t *taskState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tasks[t.lease]; dup {
		return fmt.Errorf("lease %q is already running", t.lease)
	}
	r.tasks[t.lease] = t
	return nil
}

func (r *Runner) unregister(t *taskState) {
	r.mu.Lock()
	if r.tasks[t.lease] == t {
		delete(r.tasks, t.lease)
	}
	r.mu.Unlock()
}
