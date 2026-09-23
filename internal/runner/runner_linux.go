package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/platteration/ewastesavior/internal/proto"
)

// sysState holds Linux-only runner state.
type sysState struct {
	workRoot    *os.Root // WorkRoot opened once, used for all task dirs
	workPath    string   // absolute WorkRoot path (for mounts)
	sandboxRoot string   // <WorkRoot>/.sandbox-root: the shims' root mountpoint
	tasksCg     string   // <CgroupRoot>/tasks (cgroup v2), "" when unavailable
	machine     string   // uname -m
	shimArch    string   // runtime.GOARCH of this binary
}

// sandboxRootName is the directory in WorkRoot on which every sandbox shim
// mounts its private root tmpfs, inside its own mount namespace. Task
// directory names start with a letter or digit (taskIDRE), so they never
// collide with it.
const sandboxRootName = ".sandbox-root"

// leftoverKillWait bounds how long New waits for a previous agent's task
// processes to die.
const leftoverKillWait = 5 * time.Second

// init probes capabilities and prepares WorkRoot, CacheDir and the cgroup
// parent, cleaning up whatever a previous agent left behind (DESIGN 10.5).
func (r *Runner) init() error {
	if r.cfg.SelfExe == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("runner: cannot find own executable: %w", err)
		}
		r.cfg.SelfExe = exe
	}
	r.sys.shimArch = runtime.GOARCH
	if u, err := unameMachine(); err == nil {
		r.sys.machine = u
	} else {
		r.sys.machine = r.sys.shimArch
	}

	if err := r.prepareWorkRoot(); err != nil {
		return err
	}
	if err := r.prepareCache(); err != nil {
		return err
	}
	r.prepareCgroup() // best effort; sets the cgroup2 capability
	r.killLeftoverProcs()

	r.probeCaps()
	if os.Geteuid() == 0 && (r.mode == ModeNone || !r.caps[CapMountNS]) {
		// Without a private root, task users reach the workdir through the
		// host's directories, so every ancestor must be searchable.
		if dir := unsearchableAncestor(r.sys.workPath); dir != "" {
			return fmt.Errorf("runner: task users (uid %d+) cannot reach %s: %s is not world-searchable (chmod o+x it or use another WorkRoot)",
				r.cfg.UIDBase, r.sys.workPath, dir)
		}
	}
	r.log.Info("runner ready",
		"sandbox", r.mode, "machine", r.sys.machine, "arch", r.sys.shimArch,
		"caps", strings.Join(capList(r.caps), ","))
	return nil
}

func (r *Runner) prepareWorkRoot() error {
	abs, err := filepath.Abs(r.cfg.WorkRoot)
	if err != nil {
		return err
	}
	r.sys.workPath = abs
	if err := os.MkdirAll(abs, 0o711); err != nil {
		return fmt.Errorf("runner: work root: %w", err)
	}
	if os.Geteuid() == 0 {
		os.Chown(abs, 0, 0)
	}
	os.Chmod(abs, 0o711)
	wipeDir(abs)
	root, err := os.OpenRoot(abs)
	if err != nil {
		return fmt.Errorf("runner: open work root: %w", err)
	}
	r.sys.workRoot = root
	// The shims' root mountpoint: empty and reachable by root only. Task
	// cleanup (removeAllIn by task dir name) and output collection (an
	// os.Root on the task dir) never reach it.
	if err := root.Mkdir(sandboxRootName, 0o700); err != nil {
		return fmt.Errorf("runner: sandbox root mountpoint: %w", err)
	}
	if f, err := root.OpenFile(sandboxRootName, os.O_RDONLY|oNoFollow, 0); err == nil {
		own := owner{uid: -1, gid: -1}
		if os.Geteuid() == 0 {
			own = owner{uid: 0, gid: 0}
		}
		finishFile(f, own, 0o700)
		f.Close()
	}
	r.sys.sandboxRoot = filepath.Join(abs, sandboxRootName)
	return nil
}

// killLeftoverProcs kills processes a previous agent's tasks left behind
// under the slot uids (DESIGN 10.5). Tasks without a cgroup and pid
// namespace can outlive the agent: their parent-death signal only reaches
// the task's first process.
func (r *Runner) killLeftoverProcs() {
	if os.Geteuid() != 0 {
		return // tasks run as the agent's own uid; nothing to tell apart
	}
	lo, hi := r.cfg.UIDBase, r.cfg.UIDBase+r.cfg.Slots-1
	n, ok := killUIDProcs(lo, hi, leftoverKillWait)
	switch {
	case !ok:
		r.log.Warn("leftover task processes survived SIGKILL", "uids", fmt.Sprintf("%d-%d", lo, hi))
	case n > 0:
		r.log.Info("killed leftover task processes", "count", n)
	}
}

func (r *Runner) prepareCache() error {
	abs, err := filepath.Abs(r.cfg.CacheDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return fmt.Errorf("runner: cache dir: %w", err)
	}
	if os.Geteuid() == 0 {
		os.Chown(abs, 0, 0)
	}
	os.Chmod(abs, 0o700)
	r.cache = newBlobCache(abs, int64(r.cfg.CacheMaxMB)<<20, r.log)
	r.cache.clean()
	return nil
}

// prepareCgroup sets up <CgroupRoot>/tasks with the wanted controllers and
// removes leftover task-* cgroups. Failures leave the cgroup2 capability off.
func (r *Runner) prepareCgroup() {
	if r.cfg.CgroupRoot == "" {
		return
	}
	root := r.cfg.CgroupRoot
	if !isCgroup2(root) {
		if err := os.MkdirAll(root, 0o755); err != nil || !isCgroup2(root) {
			r.log.Info("cgroup v2 root unavailable; running without cgroup limits", "root", root)
			return
		}
	}
	// Leftover task cgroups can sit directly under the root (older layout)
	// or under tasks/.
	removeLeftoverTasks(root)
	if err := enableSubtree(root); err != nil {
		r.log.Warn("cannot enable cgroup controllers on the root", "root", root, "err", err)
	}
	tasks := filepath.Join(root, "tasks")
	if err := os.Mkdir(tasks, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		r.log.Warn("cannot create the cgroup parent", "path", tasks, "err", err)
		return
	}
	removeLeftoverTasks(tasks)
	enableSubtree(tasks)
	r.sys.tasksCg = tasks
}

// probeCaps fills r.caps from the environment.
func (r *Runner) probeCaps() {
	root := os.Geteuid() == 0
	if root {
		for _, p := range []struct {
			file, cap string
		}{
			{"mnt", CapMountNS}, {"pid", CapPidNS}, {"net", CapNetNS},
			{"ipc", CapIPCNS}, {"uts", CapUTSNS},
		} {
			if _, err := os.Stat("/proc/self/ns/" + p.file); err == nil {
				r.caps[p.cap] = true
			}
		}
		r.caps[CapPrivDrop] = true
	}
	if _, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); err == nil {
		r.caps[CapNNP] = true
	}
	if _, err := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0); err == nil {
		if _, perr := buildSeccompProgram(filterArches(r.sys.shimArch, r.sys.machine)); perr == nil {
			r.caps[CapSeccomp] = true
		}
	}
	if r.sys.tasksCg != "" {
		have := map[string]bool{}
		for _, c := range controllers(r.sys.tasksCg) {
			have[c] = true
		}
		if have["cpu"] && have["memory"] && have["pids"] {
			r.caps[CapCgroup2] = true
		}
	}
}

// SetCPULimit sets the swarm-wide CPU cap on the parent cgroup (DESIGN 10.3).
// It is a no-op when cgroup v2 is unavailable.
func (r *Runner) SetCPULimit(cores float64) error {
	if r.sys.tasksCg == "" || !r.caps[CapCgroup2] {
		return nil
	}
	const period = 100000
	v := "max " + strconv.Itoa(period)
	if cores > 0 {
		q := int64(cores * period)
		if q < 1000 {
			q = 1000
		}
		v = fmt.Sprintf("%d %d", q, period)
	}
	return writeCgroupFile(r.sys.tasksCg, "cpu.max", v)
}

// Run executes one task and returns its report. It blocks until the task
// ends, is canceled (ctx) or preempted.
func (r *Runner) Run(ctx context.Context, t proto.Task, logs io.Writer, progress func(proto.RunningTask)) proto.TaskReport {
	return r.run(ctx, t, logs, progress)
}

// unsearchableAncestor returns the first proper ancestor of path that
// others can't traverse (missing o+x), or "".
func unsearchableAncestor(path string) string {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if st, err := os.Stat(dir); err == nil && st.Mode().Perm()&0o001 == 0 {
			return dir
		}
		if dir == "/" || dir == "." {
			return ""
		}
	}
}
