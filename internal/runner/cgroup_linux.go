package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// cgroup is one cgroup v2 directory.
type cgroup struct {
	path string // filesystem path
}

// wantControllers are the controllers a task cgroup needs.
var wantControllers = []string{"cpu", "memory", "pids"}

// readController reads a cgroup interface file, trimming whitespace.
func readCgroupFile(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	return strings.TrimSpace(string(b)), err
}

// writeCgroupFile writes value to a cgroup interface file.
func writeCgroupFile(dir, name, value string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644)
}

// controllers lists the controllers available in a cgroup directory.
func controllers(dir string) []string {
	s, err := readCgroupFile(dir, "cgroup.controllers")
	if err != nil {
		return nil
	}
	return strings.Fields(s)
}

// enableSubtree adds the wanted controllers to cgroup.subtree_control so
// children can use them. Controllers not present are skipped.
func enableSubtree(dir string) error {
	have := map[string]bool{}
	for _, c := range controllers(dir) {
		have[c] = true
	}
	var add []string
	for _, c := range wantControllers {
		if have[c] {
			add = append(add, "+"+c)
		}
	}
	if len(add) == 0 {
		return nil
	}
	// Writing all at once fails atomically; write one at a time so a single
	// unavailable controller doesn't block the others.
	var firstErr error
	for _, a := range add {
		if err := writeCgroupFile(dir, "cgroup.subtree_control", a); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// mkCgroup creates a child cgroup under parent.
func mkCgroup(parent, name string) (*cgroup, error) {
	p := filepath.Join(parent, name)
	if err := os.Mkdir(p, 0o755); err != nil {
		return nil, err
	}
	return &cgroup{path: p}, nil
}

// openFD opens the cgroup directory (for UseCgroupFD).
func (c *cgroup) openFD() (int, error) {
	return unix.Open(c.path, unix.O_DIRECTORY|unix.O_PATH|unix.O_CLOEXEC, 0)
}

func (c *cgroup) set(name, value string) error { return writeCgroupFile(c.path, name, value) }

// setLimits applies the task's resource limits (DESIGN 12 step 4).
func (c *cgroup) setLimits(cores float64, memMB, extraMemMB int) error {
	// cpu.max: quota period, both in microseconds. "max" means unlimited.
	if cores > 0 {
		const period = 100000
		quota := int64(cores * period)
		if quota < 1000 {
			quota = 1000
		}
		if err := c.set("cpu.max", fmt.Sprintf("%d %d", quota, period)); err != nil {
			return fmt.Errorf("cpu.max: %w", err)
		}
	}
	mem := int64(memMB+extraMemMB) << 20
	if mem > 0 {
		if err := c.set("memory.max", strconv.FormatInt(mem, 10)); err != nil {
			return fmt.Errorf("memory.max: %w", err)
		}
	}
	// Best effort: not every kernel exposes all of these.
	c.set("memory.swap.max", "0")
	c.set("memory.oom.group", "1")
	if err := c.set("pids.max", "1024"); err != nil {
		return fmt.Errorf("pids.max: %w", err)
	}
	return nil
}

// pids lists the PIDs currently in the cgroup.
func (c *cgroup) pids() []int {
	s, err := readCgroupFile(c.path, "cgroup.procs")
	if err != nil {
		return nil
	}
	var out []int
	for _, line := range strings.Fields(s) {
		if pid, err := strconv.Atoi(line); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

// populated reports whether the cgroup still has any process.
func (c *cgroup) populated() bool {
	s, err := readCgroupFile(c.path, "cgroup.events")
	if err != nil {
		return len(c.pids()) > 0
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "populated 1" {
			return true
		}
	}
	return false
}

// freeze pauses or resumes the whole cgroup (cgroup.freeze).
func (c *cgroup) freeze(frozen bool) error {
	v := "0"
	if frozen {
		v = "1"
	}
	if err := c.set("cgroup.freeze", v); err != nil {
		return err
	}
	// Wait until the state actually settles, so the timeout clock only
	// stops once the processes are really quiescent.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s, err := readCgroupFile(c.path, "cgroup.events")
		if err != nil {
			return nil
		}
		want := "frozen 1"
		if !frozen {
			want = "frozen 0"
		}
		if strings.Contains(s, want) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// kill kills every process in the cgroup (cgroup.kill), falling back to
// signalling each PID when the interface is absent.
func (c *cgroup) kill() {
	if err := c.set("cgroup.kill", "1"); err == nil {
		return
	}
	for _, pid := range c.pids() {
		unix.Kill(pid, unix.SIGKILL)
	}
}

// waitEmpty blocks until the cgroup has no processes or the deadline passes.
func (c *cgroup) waitEmpty(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !c.populated() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stats reports CPU time and peak memory (0 when unavailable).
func (c *cgroup) stats() (cpuSeconds float64, maxMemMB int) {
	if s, err := readCgroupFile(c.path, "cpu.stat"); err == nil {
		for _, line := range strings.Split(s, "\n") {
			if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
				if usec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					cpuSeconds = float64(usec) / 1e6
				}
			}
		}
	}
	if s, err := readCgroupFile(c.path, "memory.peak"); err == nil {
		if peak, err := strconv.ParseInt(s, 10, 64); err == nil {
			maxMemMB = int((peak + (1 << 20) - 1) >> 20)
		}
	}
	return cpuSeconds, maxMemMB
}

// remove kills and deletes the cgroup, retrying briefly while it drains.
func (c *cgroup) remove() error {
	c.kill()
	c.waitEmpty(killWait)
	var err error
	for i := 0; i < 200; i++ {
		if err = os.Remove(c.path); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// removeLeftoverTasks kills and removes leftover task-* cgroups under
// <parent>/tasks (DESIGN 10.5).
func removeLeftoverTasks(tasksDir string) {
	ents, err := os.ReadDir(tasksDir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "task-") {
			(&cgroup{path: filepath.Join(tasksDir, e.Name())}).remove()
		}
	}
}
