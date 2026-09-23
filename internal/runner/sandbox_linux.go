package runner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// runProc controls a started task's processes: freeze via the cgroup when
// available, else SIGSTOP/SIGCONT on the process group; kill likewise.
type runProc struct {
	cg   *cgroup
	pgid int
}

func (p *runProc) freeze(frozen bool) error {
	if p.cg != nil {
		return p.cg.freeze(frozen)
	}
	sig := unix.SIGCONT
	if frozen {
		sig = unix.SIGSTOP
	}
	return unix.Kill(-p.pgid, sig)
}

func (p *runProc) kill() {
	if p.cg != nil {
		p.cg.kill()
	}
	// Always signal the group too: a fallback when the cgroup is absent and
	// a backstop for anything the cgroup missed. SIGCONT unwedges a frozen
	// (SIGSTOP'd) group so the SIGKILL is delivered.
	unix.Kill(-p.pgid, unix.SIGCONT)
	unix.Kill(-p.pgid, unix.SIGKILL)
}

// launched is a started sandbox.
type launched struct {
	cmd        *exec.Cmd
	waitCh     chan error
	logR       *os.File
	statusR    *os.File
	logDone    chan struct{} // the log copier returned
	statusDone chan struct{} // the status reader returned; reason is set
	reason     string
}

// Bounds for drain: how long the pipes may stay open after the task's
// processes were killed, and after the caller's context ended.
var (
	drainGrace    = 2 * time.Second
	drainCtxGrace = 100 * time.Millisecond
)

// drain waits for the log copier and the status reader to reach EOF, that
// is for every holder of the pipes' write ends to be gone. A process that
// escaped the kill (no cgroup and no pid namespace) can hold them open
// forever: after grace (or shortly after ctx ends) the read ends are closed
// so both readers return. drain reports whether the pipes reached EOF.
func (l *launched) drain(ctx context.Context, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ctxDone := ctx.Done()
	drained := true
	for _, done := range []chan struct{}{l.logDone, l.statusDone} {
	wait:
		for drained {
			select {
			case <-done:
				break wait
			case <-timer.C:
				drained = false
			case <-ctxDone:
				// Still let the readers flush what the task wrote.
				ctxDone = nil
				timer.Reset(min(grace, drainCtxGrace))
			}
		}
	}
	if !drained {
		// Closing a pollable pipe wakes a blocked Read.
		l.logR.Close()
		l.statusR.Close()
		<-l.logDone
		<-l.statusDone
	}
	return drained
}

// statusReason is the failure reason the shim wrote; only valid after drain.
func (l *launched) statusReason() string { return l.reason }

// startSandbox starts `savior sandbox-exec` for the task and wires up the
// combined-output pipe and the status pipe.
func (r *Runner) startSandbox(tk *taskDir, env, argv []string, logs io.Writer) (*launched, error) {
	logR, logW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		logR.Close()
		logW.Close()
		return nil, err
	}

	flags := r.shimFlags(tk, env, argv)
	useFD := tk.cg != nil
	var cgFD int = -1
	if useFD {
		if fd, err := tk.cg.openFD(); err == nil {
			cgFD = fd
		} else {
			useFD = false
		}
	}
	// Self-join is used when we can't hand the kernel a cgroup fd.
	selfJoin := tk.cg != nil && !useFD
	cmd := r.buildCmd(tk, flags, selfJoin)
	cmd.Stdout = logW
	cmd.Stderr = logW
	cmd.ExtraFiles = []*os.File{statusW}
	if useFD {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = cgFD
	}

	startErr := cmd.Start()
	if cgFD >= 0 {
		unix.Close(cgFD)
	}
	if startErr != nil && useFD && tk.cg != nil {
		// The kernel may lack clone3/CLONE_INTO_CGROUP: retry with the shim
		// joining the cgroup itself.
		cmd = r.buildCmd(tk, flags, true)
		cmd.Stdout = logW
		cmd.Stderr = logW
		cmd.ExtraFiles = []*os.File{statusW}
		startErr = cmd.Start()
	}
	// The child holds its own dups; drop the parent's write ends so the
	// readers see EOF when the child exits or execs.
	logW.Close()
	statusW.Close()
	if startErr != nil {
		logR.Close()
		statusR.Close()
		return nil, startErr
	}

	lc := &launched{cmd: cmd, waitCh: make(chan error, 1)}
	lc.readPipes(logR, statusR, logs)
	go func() { lc.waitCh <- cmd.Wait() }()
	return lc, nil
}

// readPipes starts the goroutines that copy the task output to logs and
// read the shim's status reason.
func (l *launched) readPipes(logR, statusR *os.File, logs io.Writer) {
	l.logR, l.statusR = logR, statusR
	l.logDone, l.statusDone = make(chan struct{}), make(chan struct{})
	go func() {
		io.Copy(logs, logR)
		logR.Close()
		close(l.logDone)
	}()
	go func() {
		b, _ := io.ReadAll(io.LimitReader(statusR, 4096))
		statusR.Close()
		l.reason = trimReason(string(b))
		close(l.statusDone)
	}()
}

// buildCmd assembles the shim command with its SysProcAttr.
func (r *Runner) buildCmd(tk *taskDir, flags []string, selfJoin bool) *exec.Cmd {
	args := append([]string{"sandbox-exec"}, flags...)
	if selfJoin {
		args = append(args, "--cgroup", tk.cg.path)
	}
	args = append(args, "--")
	args = append(args, tk.argv...)
	cmd := exec.Command(r.cfg.SelfExe, args...)
	cmd.Env = []string{} // no node secrets ever reach the child's environment
	var cf uintptr
	if tk.plan.useNS {
		cf = unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS
		if tk.plan.useNetNS {
			cf |= unix.CLONE_NEWNET
		}
	}
	// Setsid: the task gets its own session (no controlling terminal, no
	// access to the agent's) and process group (pgid == pid, which kill
	// and SIGSTOP freezing rely on).
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: cf,
		Setsid:     true,
		Pdeathsig:  syscall.SIGKILL,
	}
	return cmd
}

// shimFlags builds the shim's control flags (the command follows "--").
func (r *Runner) shimFlags(tk *taskDir, env, argv []string) []string {
	mode := "full"
	if !tk.plan.useNS {
		mode = "none"
	}
	f := []string{
		"--work", tk.abs,
		"--uid", strconv.Itoa(tk.uid),
		"--gid", strconv.Itoa(tk.gid),
		"--fsize-mb", strconv.Itoa(effectiveDiskMB(tk.task)),
		"--sandbox", mode,
		"--strict=" + strconv.FormatBool(r.mode == ModeStrict),
		"--seccomp=" + strconv.FormatBool(tk.plan.useSeccomp),
		"--netns=" + strconv.FormatBool(tk.plan.useNetNS),
		"--network=" + strconv.FormatBool(tk.task.Network),
	}
	if tk.plan.useNS {
		f = append(f, "--root-mnt", r.sys.sandboxRoot)
	}
	for _, e := range env {
		f = append(f, "--env", e)
	}
	tk.argv = argv
	return f
}

func trimReason(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}
