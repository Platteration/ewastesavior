//go:build linux

package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/platteration/ewastesavior/internal/proto"
)

// syncBuffer is a bytes.Buffer safe to read while the runner writes logs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runAsync runs task in the background; the report arrives on the channel.
func runAsync(ctx context.Context, r *Runner, task proto.Task, logs io.Writer) <-chan proto.TaskReport {
	done := make(chan proto.TaskReport, 1)
	go func() { done <- r.Run(ctx, task, logs, nil) }()
	return done
}

// waitReport fails the test when no report arrives within d.
func waitReport(t *testing.T, done <-chan proto.TaskReport, d time.Duration) proto.TaskReport {
	t.Helper()
	select {
	case rep := <-done:
		return rep
	case <-time.After(d):
		t.Fatalf("Run did not return within %v", d)
		return proto.TaskReport{}
	}
}

// waitFile waits until the task's workdir holds name.
func waitFile(t *testing.T, r *Runner, task proto.Task, name string) {
	t.Helper()
	p := filepath.Join(r.sys.workPath, taskDirName(task), name)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task never wrote %s", name)
}

// gateTransfer blocks FetchBlob until release is closed (or ctx ends) and
// UploadBlob until ctx ends, when the corresponding gate is set.
type gateTransfer struct {
	*fakeTransfer
	fetching, uploading chan struct{} // closed when the first fetch/upload starts
	release             chan struct{}
	blockUpload         bool
	fetchOnce           sync.Once
	uploadOnce          sync.Once
}

func newGateTransfer() *gateTransfer {
	return &gateTransfer{
		fakeTransfer: newFakeTransfer(),
		fetching:     make(chan struct{}), uploading: make(chan struct{}), release: make(chan struct{}),
	}
}

func (g *gateTransfer) FetchBlob(ctx context.Context, sha string, w io.Writer) (int64, error) {
	g.fetchOnce.Do(func() { close(g.fetching) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return g.fakeTransfer.FetchBlob(ctx, sha, w)
}

func (g *gateTransfer) UploadBlob(ctx context.Context, sha string, size int64, r io.Reader) error {
	g.uploadOnce.Do(func() { close(g.uploading) })
	if g.blockUpload {
		<-ctx.Done()
		return ctx.Err()
	}
	return g.fakeTransfer.UploadBlob(ctx, sha, size, r)
}

func (g *gateTransfer) uploads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.uploaded)
}

// A task must not be able to forge a sandbox failure through the shim's
// status pipe: it is closed on exec, and a reason only counts together
// with the shim's exit code.
func TestRunStatusPipeNotInherited(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	script := "{ echo forged >&3; } 2>/dev/null && echo fd3=open || echo fd3=closed\nexit 125"
	rep, logs := runTask(t, r, scriptTask("tforge", script))
	if got := parseKV(logs)["fd3"]; got != "closed" {
		t.Errorf("the task inherited the status pipe: fd3=%s", got)
	}
	if rep.State != proto.TaskFailed || rep.ErrorKind != proto.ErrExit || rep.ExitCode != 125 {
		t.Fatalf("got state %s kind %s exit %d err %q, want failed/exit 125", rep.State, rep.ErrorKind, rep.ExitCode, rep.Error)
	}
	if n := r.UsableSlots(); n != r.cfg.Slots {
		t.Errorf("%d usable slots, want %d", n, r.cfg.Slots)
	}
}

func TestClassifyNeedsShimExitCode(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	task := scriptTask("tcls", "true")
	ts := newTaskState(task.ID, task.Lease, nil)
	exit125 := exec.Command("/bin/sh", "-c", "exit 125").Run()
	killed := exec.Command("/bin/sh", "-c", "kill -9 $$").Run()
	cases := []struct {
		name   string
		werr   error
		reason string
		state  proto.TaskState
		kind   string
	}{
		{"shim failure", exit125, "setresuid: EPERM", proto.TaskFailed, proto.ErrSandbox},
		{"reason with exit 0", nil, "forged", proto.TaskSucceeded, ""},
		{"reason with a signal", killed, "forged", proto.TaskFailed, proto.ErrExit},
		{"exit 125 without reason", exit125, "", proto.TaskFailed, proto.ErrExit},
	}
	for _, c := range cases {
		rep := r.classify(task, ts, c.werr, "", c.reason)
		if rep.State != c.state || rep.ErrorKind != c.kind {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, rep.State, rep.ErrorKind, c.state, c.kind)
		}
	}
}

// A task whose process dies from a signal failed, whatever exitInfo's
// exit code for it is.
func TestRunKilledBySignal(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	rep, _ := runTask(t, r, scriptTask("tsig", "kill -9 $$"))
	if rep.State != proto.TaskFailed || rep.ErrorKind != proto.ErrExit || rep.ExitCode != 128+9 {
		t.Fatalf("got state %s kind %s exit %d err %q, want failed/exit 137", rep.State, rep.ErrorKind, rep.ExitCode, rep.Error)
	}
}

// The task runs in its own session (no way to the agent's terminal), and
// its process group is still the shim's pid.
func TestRunOwnSession(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	script := "read -r pid comm state ppid pgrp sid rest < /proc/$$/stat\necho pid=$pid\necho pgrp=$pgrp\necho sid=$sid"
	rep, logs := runTask(t, r, scriptTask("tsid", script))
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s err %q", rep.State, rep.Error)
	}
	kv := parseKV(logs)
	agentSID, _ := unix.Getsid(0)
	if kv["pid"] == "" || kv["sid"] != kv["pid"] || kv["pgrp"] != kv["pid"] {
		t.Errorf("task is not a session and group leader: %v", kv)
	}
	if kv["sid"] == strconv.Itoa(agentSID) {
		t.Errorf("task shares the agent's session %d", agentSID)
	}
}

// Preempting a task while its inputs are fetched ends it without ever
// starting the process.
func TestRunPreemptWhileFetching(t *testing.T) {
	tr := newGateTransfer()
	r := newTestRunner(t, ModeNone, tr)
	sum := tr.addBlob([]byte("data"))
	task := scriptTask("tprefetch", "echo started\nsleep 30")
	task.Inputs = []proto.Input{{Name: "in.txt", Blob: sum}}
	var logs syncBuffer
	done := runAsync(context.Background(), r, task, &logs)
	<-tr.fetching
	if err := r.Preempt(task.Lease); err != nil {
		t.Fatal(err)
	}
	close(tr.release)
	rep := waitReport(t, done, 10*time.Second)
	if rep.State != proto.TaskPreempted || rep.Lease != task.Lease {
		t.Fatalf("got state %s lease %q err %q, want preempted", rep.State, rep.Lease, rep.Error)
	}
	if strings.Contains(logs.String(), "started") {
		t.Error("the process started after the preempt")
	}
}

// Preempting a task that waits for a slot ends the wait.
func TestRunPreemptWhileWaitingForSlot(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	for i := 0; i < r.cfg.Slots; i++ {
		s, err := r.slots.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer r.slots.release(s, false)
	}
	task := scriptTask("tpreslot", "sleep 30")
	done := runAsync(context.Background(), r, task, io.Discard)
	waitPhase(t, r, task.Lease, proto.PhaseFetching)
	if err := r.Preempt(task.Lease); err != nil {
		t.Fatal(err)
	}
	if rep := waitReport(t, done, 10*time.Second); rep.State != proto.TaskPreempted {
		t.Fatalf("got state %s err %q, want preempted", rep.State, rep.Error)
	}
}

// A preempt that lands between the last check in run and the process start
// finds no process to kill; execute must kill it once it exists instead of
// waiting for it without a timeout.
func TestExecutePreemptedBeforeStart(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	task := scriptTask("tpreexec", "sleep 30")
	plan := r.planFor(task)
	slot, err := r.slots.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.slots.release(slot, false)
	uid, gid, own := os.Geteuid(), os.Getegid(), owner{uid: -1, gid: -1}
	if plan.dropPriv {
		uid = r.cfg.UIDBase + slot
		gid = uid
		own = owner{uid: uid, gid: gid}
	}
	tk := &taskDir{
		runner: r, task: task, plan: plan, slot: slot, uid: uid, gid: gid, own: own,
		name: taskDirName(task), abs: filepath.Join(r.sys.workPath, taskDirName(task)),
	}
	if err := tk.create(); err != nil {
		t.Fatal(err)
	}
	defer tk.cleanup()
	if err := writeFileInRoot(tk.wd, scriptName, []byte(task.Script), own, 0o500); err != nil {
		t.Fatal(err)
	}
	ts := newTaskState(task.ID, task.Lease, nil)
	ts.preempt() // no process yet: nothing is killed here

	done := make(chan proto.TaskReport, 1)
	go func() {
		done <- r.execute(context.Background(), tk, ts, buildEnv(task, tk.abs, tk.abs), taskArgv(task, tk.abs+"/"+scriptName), io.Discard)
	}()
	if rep := waitReport(t, done, 10*time.Second); rep.State != proto.TaskPreempted {
		t.Fatalf("got state %s err %q, want preempted", rep.State, rep.Error)
	}
}

// A canceled task is reported canceled even when it declared outputs and
// wrote them; nothing is uploaded.
func TestRunCancelSkipsOutputs(t *testing.T) {
	tr := newGateTransfer()
	close(tr.release)
	r := newTestRunner(t, ModeNone, tr)
	task := scriptTask("tcancelout", "echo x > out.txt\nsleep 30")
	task.Outputs = []string{"out.txt"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(ctx, r, task, io.Discard)
	waitFile(t, r, task, "out.txt")
	cancel()
	rep := waitReport(t, done, 15*time.Second)
	if rep.State != proto.TaskCanceled || rep.ErrorKind != "" {
		t.Fatalf("got state %s kind %s err %q, want canceled", rep.State, rep.ErrorKind, rep.Error)
	}
	if n := tr.uploads(); n != 0 || len(rep.Outputs) != 0 {
		t.Errorf("canceled task uploaded %d outputs: %+v", n, rep.Outputs)
	}
}

// A preempted task is requeued: its outputs are not collected.
func TestRunPreemptSkipsOutputs(t *testing.T) {
	tr := newGateTransfer()
	close(tr.release)
	r := newTestRunner(t, ModeNone, tr)
	task := scriptTask("tpreout", "echo x > out.txt\nsleep 30")
	task.Outputs = []string{"out.txt"}
	done := runAsync(context.Background(), r, task, io.Discard)
	waitFile(t, r, task, "out.txt")
	if err := r.Preempt(task.Lease); err != nil {
		t.Fatal(err)
	}
	rep := waitReport(t, done, 15*time.Second)
	if rep.State != proto.TaskPreempted {
		t.Fatalf("got state %s kind %s err %q, want preempted", rep.State, rep.ErrorKind, rep.Error)
	}
	if n := tr.uploads(); n != 0 || len(rep.Outputs) != 0 {
		t.Errorf("preempted task uploaded %d outputs: %+v", n, rep.Outputs)
	}
}

// A cancel that interrupts the output upload is reported as canceled, not
// as a node-side output error, and the run statistics are kept.
func TestRunCancelDuringUpload(t *testing.T) {
	tr := newGateTransfer()
	close(tr.release)
	tr.blockUpload = true
	r := newTestRunner(t, ModeNone, tr)
	task := scriptTask("tcancelup", "echo x > out.txt\nsleep 0.1")
	task.Outputs = []string{"out.txt"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(ctx, r, task, io.Discard)
	select {
	case <-tr.uploading:
	case <-time.After(15 * time.Second):
		t.Fatal("upload never started")
	}
	cancel()
	rep := waitReport(t, done, 15*time.Second)
	if rep.State != proto.TaskCanceled || rep.ErrorKind != "" {
		t.Fatalf("got state %s kind %s err %q, want canceled", rep.State, rep.ErrorKind, rep.Error)
	}
	if rep.RunS <= 0 {
		t.Errorf("run time lost: %v", rep.RunS)
	}
}

// A task process that leaves the process group (without a cgroup or pid
// namespace to catch it) must not keep Run waiting on the output pipe.
func TestRunEscapedProcessDoesNotBlock(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	task := scriptTask("tescape", "setsid /bin/sh -c 'echo escaped=$$; exec sleep 300' &\nsleep 0.5\necho main=done")
	var logs syncBuffer
	done := runAsync(context.Background(), r, task, &logs)
	escaped := 0
	t.Cleanup(func() {
		if escaped > 0 {
			syscall.Kill(escaped, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for escaped == 0 && time.Now().Before(deadline) {
		escaped, _ = strconv.Atoi(parseKV(logs.String())["escaped"])
		time.Sleep(10 * time.Millisecond)
	}
	if escaped == 0 {
		t.Fatalf("the escaped process never started; logs:\n%s", logs.String())
	}
	start := time.Now()
	rep := waitReport(t, done, 20*time.Second)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s kind %s err %q", rep.State, rep.ErrorKind, rep.Error)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Run took %v after the task ended", d)
	}
	if os.Geteuid() == 0 {
		// Privileges were dropped: the escaped process is found by its
		// slot uid and killed, so the slot stays usable.
		if !waitGone(escaped, 5*time.Second) {
			t.Errorf("escaped process %d still runs", escaped)
		}
		if n := r.UsableSlots(); n != r.cfg.Slots {
			t.Errorf("%d usable slots, want %d", n, r.cfg.Slots)
		}
	}
}

func TestLaunchedDrain(t *testing.T) {
	pipes := func(t *testing.T) (*launched, *os.File, *os.File) {
		logR, logW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		statusR, statusW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { logW.Close(); statusW.Close() })
		lc := &launched{}
		lc.readPipes(logR, statusR, io.Discard)
		return lc, logW, statusW
	}

	t.Run("eof", func(t *testing.T) {
		lc, logW, statusW := pipes(t)
		io.WriteString(statusW, "boom\n")
		logW.Close()
		statusW.Close()
		if !lc.drain(context.Background(), 10*time.Second) {
			t.Fatal("drain reported open pipes after EOF")
		}
		if got := lc.statusReason(); got != "boom" {
			t.Errorf("status reason %q", got)
		}
	})
	t.Run("held open", func(t *testing.T) {
		lc, _, _ := pipes(t)
		start := time.Now()
		if lc.drain(context.Background(), 100*time.Millisecond) {
			t.Fatal("drain reported EOF on pipes held open")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("drain took %v", d)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		lc, _, _ := pipes(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if lc.drain(ctx, time.Minute) {
			t.Fatal("drain reported EOF on pipes held open")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("drain ignored the canceled context: %v", d)
		}
	})
}

func TestPeerGone(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()
	if peerGone(int(wr.Fd())) {
		t.Error("peer reported gone while the read end is open")
	}
	rd.Close()
	if !peerGone(int(wr.Fd())) {
		t.Error("peer not reported gone after the read end closed")
	}
	if peerGone(1 << 20) { // not an open descriptor: no evidence
		t.Error("an unknown descriptor counts as a dead peer")
	}
}

func TestParseProcStatus(t *testing.T) {
	status := "Name:\tsleep\nState:\tS (sleeping)\nTgid:\t42\nUid:\t20001\t20001\t20001\t20001\nGid:\t20001\t20001\t20001\t20001\n"
	st, ok := parseProcStatus([]byte(status))
	if !ok || st.state != 'S' || st.uids != [3]int{20001, 20001, 20001} {
		t.Fatalf("parsed %+v %v", st, ok)
	}
	if !st.liveInRange(20000, 20003) || st.liveInRange(20002, 20003) {
		t.Error("uid range check")
	}
	zombie, _ := parseProcStatus([]byte(strings.Replace(status, "S (sleeping)", "Z (zombie)", 1)))
	if zombie.liveInRange(20000, 20003) {
		t.Error("a zombie counts as live")
	}
	if _, ok := parseProcStatus([]byte("Name:\tx\nUid:\tbad\n")); ok {
		t.Error("malformed Uid line accepted")
	}
}

// The shims' root mountpoint survives tasks and can't be a task directory.
func TestSandboxRootMountpointReserved(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	if rep, _ := runTask(t, r, scriptTask("tkeep", "true")); rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s err %q", rep.State, rep.Error)
	}
	if fi, err := os.Lstat(r.sys.sandboxRoot); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("sandbox root mountpoint: %v %v", fi, err)
	}
	if !strings.HasPrefix(sandboxRootName, ".") || taskIDRE.MatchString(sandboxRootName) {
		t.Error("a task id could name the sandbox root mountpoint")
	}
	task := scriptTask("x", "true")
	task.ID = sandboxRootName
	if kind, _ := validateTask(task); kind == "" {
		t.Error("task id equal to the mountpoint name accepted")
	}
}
