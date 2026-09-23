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

	"golang.org/x/sys/unix"

	"github.com/platteration/ewastesavior/internal/proto"
)

// sysState holds Linux-only runner state.
type sysState struct {
	workRoot *os.Root // WorkRoot opened once, used for all task dirs
	workPath string   // absolute WorkRoot path (for mounts)
	tasksCg  string   // <CgroupRoot>/tasks (cgroup v2), "" when unavailable
	machine  string   // uname -m
	shimArch string   // runtime.GOARCH of this binary
}

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

	r.probeCaps()
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
	return nil
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
