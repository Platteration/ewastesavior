package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/platteration/ewastesavior/internal/proto"
)

// sandboxPlan is how one task will be isolated, given the mode and caps.
type sandboxPlan struct {
	useNS      bool // mount/pid/ipc/uts namespaces + pivot_root
	useNetNS   bool // separate network namespace (only lo)
	useCgroup  bool // cgroup v2 limits
	useSeccomp bool // seccomp-BPF filter
	dropPriv   bool // setuid/setgid to the slot uid (root only)
	errKind    string
	errMsg     string
}

func (r *Runner) planFor(t proto.Task) sandboxPlan {
	root := os.Geteuid() == 0
	p := sandboxPlan{dropPriv: root}
	switch r.mode {
	case ModeNone:
		// No namespaces/cgroups/seccomp; privileges still dropped when root.
	case ModeStrict:
		if _, _, full := r.Caps(); !full {
			p.errKind = proto.ErrSandbox
			p.errMsg = "strict sandbox requires full isolation; missing: " + strings.Join(r.missingCaps(), ", ")
			return p
		}
		p.useNS, p.useNetNS, p.useCgroup, p.useSeccomp = true, !t.Network, true, true
	case ModeAuto:
		p.useNS = r.caps[CapMountNS] && r.caps[CapPidNS] && r.caps[CapIPCNS] && r.caps[CapUTSNS]
		p.useNetNS = p.useNS && r.caps[CapNetNS] && !t.Network
		p.useCgroup = r.caps[CapCgroup2]
		p.useSeccomp = r.caps[CapSeccomp]
	}
	return p
}

// run is the Linux implementation of Run.
func (r *Runner) run(ctx context.Context, t proto.Task, logs io.Writer, progress func(proto.RunningTask)) proto.TaskReport {
	if logs == nil {
		logs = io.Discard
	}
	if kind, reason := validateTask(t); kind != "" {
		return failReport(t, kind, reason)
	}
	plan := r.planFor(t)
	if plan.errKind != "" {
		return failReport(t, plan.errKind, plan.errMsg)
	}

	ts := newTaskState(t.ID, t.Lease, progress)
	if err := r.register(ts); err != nil {
		return failReport(t, proto.ErrInternal, err.Error())
	}
	defer r.unregister(ts)
	ts.report()

	// Waiting for a slot and fetching inputs also end on a preempt.
	pctx, pcancel := ts.preemptible(ctx)
	defer pcancel()

	slot, err := r.slots.acquire(pctx)
	if err != nil {
		return abortReport(t, ctx, ts, err)
	}
	retire := false
	defer func() { r.slots.release(slot, retire) }()
	if ts.isPreempted() {
		return abortReport(t, ctx, ts, nil)
	}

	uid, gid := os.Geteuid(), os.Getegid()
	if plan.dropPriv {
		uid = r.cfg.UIDBase + slot
		gid = uid
	}
	own := owner{uid: -1, gid: -1}
	if plan.dropPriv {
		own = owner{uid: uid, gid: gid}
	}

	tk := &taskDir{
		runner: r, task: t, plan: plan, slot: slot,
		uid: uid, gid: gid, own: own,
		name: taskDirName(t),
		abs:  filepath.Join(r.sys.workPath, taskDirName(t)),
	}
	defer tk.cleanup()

	if err := tk.create(); err != nil {
		return failReport(t, proto.ErrInternal, "workdir: "+err.Error())
	}

	// Inside the sandbox the workdir is /work and /tmp is a private tmpfs.
	// Without namespaces the task runs in the real workdir with a private
	// scratch subdir for TMPDIR.
	home, tmp, scriptPath := "/work", "/tmp", "/work/"+scriptName
	if !plan.useNS {
		home = tk.abs
		tmp = tk.abs + "/" + tmpDirName
		scriptPath = tk.abs + "/" + scriptName
		if err := tk.wd.Mkdir(tmpDirName, 0o700); err == nil {
			if f, err := tk.wd.OpenFile(tmpDirName, os.O_RDONLY|oNoFollow, 0); err == nil {
				finishFile(f, own, 0o700)
				f.Close()
			}
		}
	}
	env := buildEnv(t, home, tmp)

	ts.setPhase(proto.PhaseFetching)
	if err := r.writeInputs(pctx, tk.wd, t, own, ts); err != nil {
		if pctx.Err() != nil {
			return abortReport(t, ctx, ts, pctx.Err())
		}
		var ie inputError
		if errors.As(err, &ie) {
			return failReport(t, proto.ErrInput, ie.Error())
		}
		return failReport(t, proto.ErrInput, err.Error())
	}

	if t.Kind == proto.KindScript {
		if err := writeFileInRoot(tk.wd, scriptName, []byte(t.Script), own, 0o500); err != nil {
			return failReport(t, proto.ErrInternal, "write script: "+err.Error())
		}
	}

	if err := tk.makeCgroup(); err != nil {
		if r.mode == ModeStrict {
			return failReport(t, proto.ErrSandbox, "cgroup: "+err.Error())
		}
		r.log.Warn("running without a task cgroup", "task", t.ID, "err", err)
	}

	argv := taskArgv(t, scriptPath)

	// A preempt or cancel during the setup: never start the process.
	if ts.isPreempted() || ctx.Err() != nil {
		return abortReport(t, ctx, ts, ctx.Err())
	}

	rep := r.execute(ctx, tk, ts, env, argv, logs)
	rep.Lease = t.Lease
	// A sandbox or internal failure means the command never ran: the shim
	// reports those only before execve (its status pipe is close-on-exec),
	// so the slot uid owns nothing and stays in use.
	retire = tk.leaked
	return rep
}

// execute launches the sandbox, waits with timeout handling, and collects
// outputs.
func (r *Runner) execute(ctx context.Context, tk *taskDir, ts *taskState, env, argv []string, logs io.Writer) proto.TaskReport {
	t := tk.task
	lc, err := r.startSandbox(tk, env, argv, logs)
	if err != nil {
		if ctx.Err() != nil {
			return ctxFail(t, ctx, ctx.Err())
		}
		return failReport(t, proto.ErrSandbox, "start sandbox: "+err.Error())
	}

	proc := &runProc{cg: tk.cg, pgid: lc.cmd.Process.Pid}
	if err := ts.started(proc); err != nil {
		r.log.Warn("could not apply the initial freeze", "task", t.ID, "err", err)
	}

	killReason := ""
	timeout := time.Duration(t.TimeoutS) * timeUnit
	var werr error
loop:
	for {
		// A preempt that came before ts.started had no process to kill.
		if killReason == "" && ts.isPreempted() {
			killReason = "preempted"
			proc.kill()
		}
		if killReason != "" {
			werr = <-lc.waitCh
			break
		}
		frozen, elapsed := ts.freezeState()
		var timerC <-chan time.Time
		var timer *time.Timer
		if !frozen {
			rem := timeout - elapsed
			if rem <= 0 {
				killReason = proto.ErrTimeout
				proc.kill()
				continue
			}
			timer = time.NewTimer(rem)
			timerC = timer.C
		}
		select {
		case werr = <-lc.waitCh:
			if timer != nil {
				timer.Stop()
			}
			break loop
		case <-ctx.Done():
			killReason = "canceled"
			proc.kill()
		case <-timerC:
			killReason = proto.ErrTimeout
			proc.kill()
		case <-ts.wake:
		}
		if timer != nil {
			timer.Stop()
		}
	}
	ts.stopped()

	// Wait for every process in the cgroup (or group) to be gone before
	// touching outputs (DESIGN 8.5).
	if tk.cg != nil {
		tk.cg.kill()
		if !tk.cg.waitEmpty(killWait) {
			r.log.Warn("task cgroup did not drain", "task", t.ID)
		}
	} else {
		unix.Kill(-proc.pgid, unix.SIGKILL)
		reapGroup(proc.pgid, killWait)
	}
	// Without a cgroup or pid namespace, a process that left the process
	// group (setsid) survives the kill and can hold the output pipe open.
	// Don't wait for it forever: close the pipes, then kill it by the slot
	// uid (the task's alone); if that fails the slot is retired.
	if !lc.drain(ctx, drainGrace) {
		r.log.Warn("task output pipes still open after the kill; closed them", "task", t.ID)
		if tk.cg == nil && tk.plan.dropPriv {
			if n, ok := killUIDProcs(tk.uid, tk.uid, killWait); !ok {
				r.log.Warn("task processes survived SIGKILL; retiring the slot", "task", t.ID, "uid", tk.uid)
				tk.leaked = true
			} else {
				r.log.Info("killed task processes outside the process group", "task", t.ID, "count", n)
			}
		}
	}

	shimReason := lc.statusReason()
	rep := r.classify(t, ts, werr, killReason, shimReason)

	// CPU and memory accounting.
	rep.RunS = ts.runTime().Seconds()
	if tk.cg != nil {
		rep.CPUSeconds, rep.MaxMemMB = tk.cg.stats()
	}

	// Collect outputs only from a task that ran to its end: not after a
	// setup failure, a cancel or a preempt.
	switch {
	case rep.State == proto.TaskCanceled || rep.State == proto.TaskPreempted:
	case rep.ErrorKind == proto.ErrSandbox || rep.ErrorKind == proto.ErrInternal || rep.ErrorKind == proto.ErrInput:
	default:
		outs, oerr := r.collectOutputs(ctx, tk, ts, logs)
		switch {
		case oerr == nil:
			rep.Outputs = outs
		case ctx.Err() != nil:
			// Canceled while uploading: that is the outcome, not a node
			// error. The run statistics stay.
			c := ctxFail(t, ctx, ctx.Err())
			rep.State, rep.ErrorKind, rep.Error, rep.ExitCode = c.State, c.ErrorKind, c.Error, 0
		case rep.State != proto.TaskFailed:
			rep.State = proto.TaskFailed
			rep.ErrorKind = proto.ErrOutput
			rep.Error = oerr.Error()
		default:
			fmt.Fprintf(logs, "savior: collecting outputs: %v\n", oerr)
		}
	}
	return rep
}

// classify turns the wait result and kill reason into a TaskReport state.
// A status reason counts only together with the shim's own exit code.
func (r *Runner) classify(t proto.Task, ts *taskState, werr error, killReason, shimReason string) proto.TaskReport {
	code, signal, ok := exitInfo(werr)
	if shimReason != "" {
		if ok && signal == 0 && code == shimFailCode {
			return failReport(t, proto.ErrSandbox, "sandbox: "+shimReason)
		}
		r.log.Warn("ignoring a sandbox status without the shim's exit code", "task", t.ID)
	}
	if ts.isPreempted() {
		return proto.TaskReport{State: proto.TaskPreempted, Error: "preempted"}
	}
	switch killReason {
	case "canceled":
		return proto.TaskReport{State: proto.TaskCanceled, Error: "canceled"}
	case proto.ErrTimeout:
		return failReport(t, proto.ErrTimeout, fmt.Sprintf("killed after the %ds timeout", t.TimeoutS))
	}
	switch {
	case ok && code == 0 && signal == 0: // exitInfo reports code 0 for a signal
		return proto.TaskReport{State: proto.TaskSucceeded, ExitCode: 0}
	case signal != 0:
		return withCode(failReport(t, proto.ErrExit, "killed by signal "+signalName(signal)), 128+int(signal))
	default:
		return withCode(failReport(t, proto.ErrExit, fmt.Sprintf("exited with status %d", code)), code)
	}
}

// withCode sets the report's exit code.
func withCode(rep proto.TaskReport, code int) proto.TaskReport {
	rep.ExitCode = code
	return rep
}

// abortReport is the report of a task stopped before its process ran:
// preempted, else what ctxFail makes of the context error.
func abortReport(t proto.Task, ctx context.Context, ts *taskState, err error) proto.TaskReport {
	if ts.isPreempted() {
		return proto.TaskReport{Lease: t.Lease, State: proto.TaskPreempted, Error: "preempted"}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = errors.New("task aborted")
	}
	return ctxFail(t, ctx, err)
}

// ctxFail maps a context error to canceled, else internal.
func ctxFail(t proto.Task, ctx context.Context, err error) proto.TaskReport {
	if errors.Is(ctx.Err(), context.Canceled) {
		return proto.TaskReport{Lease: t.Lease, State: proto.TaskCanceled, Error: "canceled"}
	}
	return failReport(t, proto.ErrInternal, err.Error())
}

func failReport(t proto.Task, kind, msg string) proto.TaskReport {
	return proto.TaskReport{Lease: t.Lease, State: proto.TaskFailed, ErrorKind: kind, Error: proto.Sanitize(msg, 4096, true)}
}
