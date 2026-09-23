package runner

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/platteration/ewastesavior/internal/proto"
)

// taskDir owns everything created for one task: the working directory
// (optionally a tmpfs) and the cgroup. cleanup undoes all of it.
type taskDir struct {
	runner *Runner
	task   proto.Task
	plan   sandboxPlan
	slot   int
	uid    int
	gid    int
	own    owner
	name   string // "<task_id>.<attempt>"
	abs    string // absolute path under WorkRoot
	argv   []string

	wd      *os.Root
	mounted bool
	cg      *cgroup
}

// create makes the working directory, applies ownership and, when
// ScratchInRAM is set on a root runner, mounts a size-bounded tmpfs on it.
func (d *taskDir) create() error {
	r := d.runner
	if err := r.sys.workRoot.Mkdir(d.name, 0o700); err != nil {
		return err
	}
	if f, err := r.sys.workRoot.OpenFile(d.name, os.O_RDONLY|oNoFollow, 0); err == nil {
		finishFile(f, d.own, 0o700)
		f.Close()
	}
	if r.cfg.ScratchInRAM && os.Geteuid() == 0 {
		opts := fmt.Sprintf("size=%dm,mode=0700,nr_inodes=%d", effectiveDiskMB(d.task), maxScanEntries)
		if d.plan.dropPriv {
			opts += fmt.Sprintf(",uid=%d,gid=%d", d.uid, d.gid)
		}
		if err := unix.Mount("tmpfs", d.abs, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, opts); err != nil {
			return fmt.Errorf("mount scratch tmpfs: %w", err)
		}
		d.mounted = true
	}
	wd, err := r.sys.workRoot.OpenRoot(d.name)
	if err != nil {
		return err
	}
	d.wd = wd
	return nil
}

// makeCgroup creates and configures the task cgroup (DESIGN 12 step 4). It
// returns nil (no cgroup) when cgroup v2 is not in the plan.
func (d *taskDir) makeCgroup() error {
	if !d.plan.useCgroup || d.runner.sys.tasksCg == "" {
		return nil
	}
	cg, err := mkCgroup(d.runner.sys.tasksCg, "task-"+d.name)
	if err != nil {
		return err
	}
	extraMem := 0
	if d.runner.cfg.ScratchInRAM {
		extraMem = effectiveDiskMB(d.task)
	}
	if err := cg.setLimits(d.task.Resources.Cores, d.task.Resources.MemMB, extraMem); err != nil {
		cg.remove()
		return err
	}
	d.cg = cg
	return nil
}

// cleanup removes the cgroup, unmounts the scratch tmpfs and removes the
// working directory. It never follows symlinks.
func (d *taskDir) cleanup() {
	if d.wd != nil {
		d.wd.Close()
		d.wd = nil
	}
	if d.cg != nil {
		if err := d.cg.remove(); err != nil {
			d.runner.log.Warn("could not remove task cgroup", "task", d.task.ID, "err", err)
		}
		d.cg = nil
	}
	if d.mounted {
		if err := unix.Unmount(d.abs, unix.MNT_DETACH); err != nil {
			d.runner.log.Warn("could not unmount scratch", "task", d.task.ID, "err", err)
		}
		d.mounted = false
	}
	// Remove the working directory through the parent Root (no symlink is
	// ever followed out of WorkRoot).
	if err := removeAllIn(d.runner.sys.workRoot, d.name); err != nil {
		d.runner.log.Warn("could not remove workdir", "task", d.task.ID, "err", err)
	}
}
