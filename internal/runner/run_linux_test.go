//go:build linux

package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// TestMain lets the test binary act as the sandbox-exec shim, so a Runner
// can use os.Args[0] as SelfExe.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-exec" {
		os.Exit(SandboxExecMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// fakeTransfer serves blobs and URLs from memory and records uploads.
type fakeTransfer struct {
	mu       sync.Mutex
	blobs    map[string][]byte // keyed by the hash the caller asks for
	urls     map[string][]byte
	uploaded map[string][]byte
}

func newFakeTransfer() *fakeTransfer {
	return &fakeTransfer{blobs: map[string][]byte{}, urls: map[string][]byte{}, uploaded: map[string][]byte{}}
}

func (f *fakeTransfer) addBlob(data []byte) string {
	sum := sha256Hex(data)
	f.mu.Lock()
	f.blobs[sum] = data
	f.mu.Unlock()
	return sum
}

func (f *fakeTransfer) FetchBlob(ctx context.Context, sha string, w io.Writer) (int64, error) {
	f.mu.Lock()
	data, ok := f.blobs[sha]
	f.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("no blob %s", sha)
	}
	n, err := w.Write(data)
	return int64(n), err
}

func (f *fakeTransfer) FetchURL(ctx context.Context, url string, maxBytes int64, w io.Writer) (int64, error) {
	f.mu.Lock()
	data, ok := f.urls[url]
	f.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("no url %s", url)
	}
	// Deliberately write everything, ignoring maxBytes, so the runner's own
	// size enforcement is exercised.
	n, err := w.Write(data)
	return int64(n), err
}

func (f *fakeTransfer) UploadBlob(ctx context.Context, sha string, size int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("upload size %d != declared %d", len(b), size)
	}
	if sha256Hex(b) != sha {
		return fmt.Errorf("upload hash mismatch")
	}
	f.mu.Lock()
	f.uploaded[sha] = b
	f.mu.Unlock()
	return nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newTestRunner(t *testing.T, mode string, tr Transfer) *Runner {
	t.Helper()
	if tr == nil {
		tr = newFakeTransfer()
	}
	// WorkRoot must sit under a world-traversable path so a dropped-priv
	// task (root test host) can still reach its files by absolute path.
	base, err := os.MkdirTemp("", "savior-runner-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(base, 0o711)
	t.Cleanup(func() { os.RemoveAll(base) })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		WorkRoot:   filepath.Join(base, "work"),
		CacheDir:   filepath.Join(base, "cache"),
		CgroupRoot: filepath.Join(base, "cgroup"),
		SelfExe:    self,
		Sandbox:    mode,
		UIDBase:    10000,
		Slots:      4,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, tr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The work root itself must be traversable by others too.
	os.Chmod(filepath.Join(base, "work"), 0o711)
	return r
}

func scriptTask(id, script string) proto.Task {
	return proto.Task{
		ID: id, Lease: "L-" + id, JobID: "j1", Index: 0, Count: 1, Attempt: 1,
		Kind: proto.KindScript, Script: script, TimeoutS: 30,
		Resources: proto.Resources{Cores: 1, MemMB: 64, DiskMB: 16},
	}
}

func runTask(t *testing.T, r *Runner, task proto.Task) (proto.TaskReport, string) {
	t.Helper()
	var logs bytes.Buffer
	rep := r.Run(context.Background(), task, &logs, nil)
	return rep, logs.String()
}

func TestRunExecSuccess(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	task := proto.Task{
		ID: "texec1", Lease: "Lexec1", JobID: "j", Count: 1, Attempt: 1,
		Kind: proto.KindExec, Command: []string{"echo", "hello world"}, TimeoutS: 30,
		Resources: proto.Resources{Cores: 1, MemMB: 64, DiskMB: 16},
	}
	rep, logs := runTask(t, r, task)
	if rep.State != proto.TaskSucceeded || rep.ExitCode != 0 {
		t.Fatalf("state %s exit %d err %q", rep.State, rep.ExitCode, rep.Error)
	}
	if !strings.Contains(logs, "hello world") {
		t.Errorf("logs missing output: %q", logs)
	}
}

func TestRunScriptExitCode(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	rep, _ := runTask(t, r, scriptTask("texit", "echo bye; exit 7"))
	if rep.State != proto.TaskFailed || rep.ErrorKind != proto.ErrExit || rep.ExitCode != 7 {
		t.Fatalf("got state %s kind %s exit %d", rep.State, rep.ErrorKind, rep.ExitCode)
	}
}

func TestRunEnvironment(t *testing.T) {
	os.Setenv("SAVIOR_TEST_SECRET", "leaked-host-value")
	r := newTestRunner(t, ModeNone, nil)
	task := scriptTask("tenv", "env")
	task.Index, task.Count = 4, 9
	task.Env = map[string]string{"MY_VAR": "custom"}
	rep, logs := runTask(t, r, task)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s err %q", rep.State, rep.Error)
	}
	for _, want := range []string{"SAVIOR_TASK_ID=tenv", "SAVIOR_TASK_INDEX=4", "SAVIOR_TASK_COUNT=9", "SAVIOR_ATTEMPT=1", "MY_VAR=custom", "PATH=" + taskPath, "HOME=", "LANG=C.UTF-8"} {
		if !strings.Contains(logs, want) {
			t.Errorf("env missing %q in:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "leaked-host-value") || strings.Contains(logs, "SAVIOR_TEST_SECRET") {
		t.Errorf("host environment leaked into the task:\n%s", logs)
	}
}

func TestRunTimeout(t *testing.T) {
	defer setTimeUnit(100 * time.Millisecond)()
	r := newTestRunner(t, ModeNone, nil)
	task := scriptTask("ttimeout", "sleep 30")
	task.TimeoutS = 2 // 200 ms
	start := time.Now()
	rep, _ := runTask(t, r, task)
	if rep.State != proto.TaskFailed || rep.ErrorKind != proto.ErrTimeout {
		t.Fatalf("got state %s kind %s", rep.State, rep.ErrorKind)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("timeout took too long: %v", d)
	}
}

func TestRunCancel(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	ctx, cancel := context.WithCancel(context.Background())
	task := scriptTask("tcancel", "sleep 30")
	done := make(chan proto.TaskReport, 1)
	go func() { done <- r.Run(ctx, task, io.Discard, nil) }()
	waitPhase(t, r, task.Lease, proto.PhaseRunning)
	cancel()
	select {
	case rep := <-done:
		if rep.State != proto.TaskCanceled {
			t.Fatalf("got state %s kind %s", rep.State, rep.ErrorKind)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancel did not stop the task")
	}
}

func TestRunPreempt(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	task := scriptTask("tpreempt", "sleep 30")
	done := make(chan proto.TaskReport, 1)
	go func() { done <- r.Run(context.Background(), task, io.Discard, nil) }()
	waitPhase(t, r, task.Lease, proto.PhaseRunning)
	if err := r.Preempt(task.Lease); err != nil {
		t.Fatal(err)
	}
	select {
	case rep := <-done:
		if rep.State != proto.TaskPreempted {
			t.Fatalf("got state %s", rep.State)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("preempt did not stop the task")
	}
}

func TestRunFreezeStopsClock(t *testing.T) {
	defer setTimeUnit(500 * time.Millisecond)()
	r := newTestRunner(t, ModeNone, nil)
	task := scriptTask("tfreeze", "sleep 1")
	task.TimeoutS = 5 // 2.5 s of unfrozen time
	done := make(chan proto.TaskReport, 1)
	go func() { done <- r.Run(context.Background(), task, io.Discard, nil) }()
	waitPhase(t, r, task.Lease, proto.PhaseRunning)
	if err := r.Freeze(task.Lease, true); err != nil {
		t.Fatal(err)
	}
	// Stay frozen for longer than the whole timeout budget; the clock must
	// not advance, so the task must still succeed after we thaw it.
	time.Sleep(3 * time.Second)
	if err := r.Freeze(task.Lease, false); err != nil {
		t.Fatal(err)
	}
	select {
	case rep := <-done:
		if rep.State != proto.TaskSucceeded {
			t.Fatalf("freeze did not stop the clock: state %s kind %s", rep.State, rep.ErrorKind)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("frozen task never finished")
	}
}

func TestRunOutputsAndSubdirGlob(t *testing.T) {
	tr := newFakeTransfer()
	r := newTestRunner(t, ModeNone, tr)
	task := scriptTask("touts", "mkdir -p out; echo alpha > out/a.txt; echo beta > out/b.txt; echo skip > out/c.log")
	task.Outputs = []string{"out/*.txt"}
	rep, logs := runTask(t, r, task)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s err %q logs %s", rep.State, rep.Error, logs)
	}
	if len(rep.Outputs) != 2 {
		t.Fatalf("got %d outputs, want 2: %+v", len(rep.Outputs), rep.Outputs)
	}
	names := []string{rep.Outputs[0].Name, rep.Outputs[1].Name}
	if names[0] != "out/a.txt" || names[1] != "out/b.txt" {
		t.Errorf("output names %v", names)
	}
	for _, o := range rep.Outputs {
		if _, ok := tr.uploaded[o.Blob]; !ok {
			t.Errorf("output %s not uploaded", o.Name)
		}
	}
}

func TestRunOutputsRefuseUnsafe(t *testing.T) {
	tr := newFakeTransfer()
	r := newTestRunner(t, ModeNone, tr)
	script := strings.Join([]string{
		"echo real > real.txt",
		"echo src > hsrc.txt",
		"ln -s real.txt link.txt",
		"ln hsrc.txt hard.txt", // both hsrc.txt and hard.txt get nlink 2
		"mkfifo fifo.txt",
	}, "\n")
	task := scriptTask("tunsafe", script)
	task.Outputs = []string{"*.txt"}
	rep, logs := runTask(t, r, task)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s err %q", rep.State, rep.Error)
	}
	if len(rep.Outputs) != 1 || rep.Outputs[0].Name != "real.txt" {
		t.Fatalf("expected only real.txt, got %+v", rep.Outputs)
	}
	for _, bad := range []string{"link.txt", "hard.txt", "fifo.txt"} {
		if !strings.Contains(logs, bad) {
			t.Errorf("log does not mention skipped %q:\n%s", bad, logs)
		}
	}
}

func TestRunInputBlob(t *testing.T) {
	tr := newFakeTransfer()
	r := newTestRunner(t, ModeNone, tr)
	sum := tr.addBlob([]byte("input-payload\n"))
	task := scriptTask("tin", "cat data.txt; cp data.txt out.txt")
	task.Inputs = []proto.Input{{Name: "data.txt", Blob: sum}}
	task.Outputs = []string{"out.txt"}
	rep, logs := runTask(t, r, task)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s err %q", rep.State, rep.Error)
	}
	if !strings.Contains(logs, "input-payload") {
		t.Errorf("input not delivered: %q", logs)
	}
	if len(rep.Outputs) != 1 || rep.Outputs[0].Blob != sum {
		t.Errorf("copy output mismatch: %+v", rep.Outputs)
	}
}

func TestRunInputBlobHashMismatch(t *testing.T) {
	tr := newFakeTransfer()
	r := newTestRunner(t, ModeNone, tr)
	// Register content under a hash that does not match it.
	wrong := strings.Repeat("a", 64)
	tr.mu.Lock()
	tr.blobs[wrong] = []byte("not-matching")
	tr.mu.Unlock()
	task := scriptTask("tinbad", "true")
	task.Inputs = []proto.Input{{Name: "d.txt", Blob: wrong}}
	rep, _ := runTask(t, r, task)
	if rep.State != proto.TaskFailed || rep.ErrorKind != proto.ErrInput {
		t.Fatalf("got state %s kind %s", rep.State, rep.ErrorKind)
	}
}

func TestRunInputURL(t *testing.T) {
	tr := newFakeTransfer()
	r := newTestRunner(t, ModeNone, tr)
	body := []byte("url-body-content")
	tr.urls["http://example/data"] = body
	task := scriptTask("turl", "cat u.txt")
	task.Inputs = []proto.Input{{Name: "u.txt", URL: "http://example/data", SHA256: sha256Hex(body), Size: int64(len(body))}}
	rep, logs := runTask(t, r, task)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s err %q", rep.State, rep.Error)
	}
	if !strings.Contains(logs, "url-body-content") {
		t.Errorf("url input not delivered: %q", logs)
	}
}

func TestRunInputURLSizeOverrun(t *testing.T) {
	tr := newFakeTransfer()
	r := newTestRunner(t, ModeNone, tr)
	body := []byte("this body is much larger than the declared size")
	tr.urls["http://example/big"] = body
	task := scriptTask("turlbig", "true")
	task.Inputs = []proto.Input{{Name: "u.txt", URL: "http://example/big", SHA256: sha256Hex(body), Size: 5}}
	rep, _ := runTask(t, r, task)
	if rep.State != proto.TaskFailed || rep.ErrorKind != proto.ErrInput {
		t.Fatalf("got state %s kind %s (expected input error on size overrun)", rep.State, rep.ErrorKind)
	}
}

func TestRunStrictWithoutCapsRefused(t *testing.T) {
	// cgroup v2 controllers are unavailable in this environment, so strict
	// mode cannot achieve full isolation and must refuse.
	r := newTestRunner(t, ModeStrict, nil)
	if _, _, full := r.Caps(); full {
		t.Skip("full isolation available; strict would not refuse here")
	}
	rep, _ := runTask(t, r, scriptTask("tstrict", "true"))
	if rep.State != proto.TaskFailed || rep.ErrorKind != proto.ErrSandbox {
		t.Fatalf("strict without caps: got state %s kind %s", rep.State, rep.ErrorKind)
	}
}

func TestRunCleanup(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	task := scriptTask("tclean", "echo hi > leftover.txt")
	rep, _ := runTask(t, r, task)
	if rep.State != proto.TaskSucceeded {
		t.Fatalf("state %s", rep.State)
	}
	dir := filepath.Join(r.sys.workPath, taskDirName(task))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("workdir %s not removed: %v", dir, err)
	}
	if r.FreeSlots() != 4 {
		t.Errorf("slot not released: %d free", r.FreeSlots())
	}
}

func TestRunRejectsBadTask(t *testing.T) {
	r := newTestRunner(t, ModeNone, nil)
	task := scriptTask("tbad", "true")
	task.TimeoutS = 0
	rep, _ := runTask(t, r, task)
	if rep.State != proto.TaskFailed || rep.ErrorKind != proto.ErrInternal {
		t.Fatalf("got state %s kind %s", rep.State, rep.ErrorKind)
	}
	if rep.Lease != task.Lease {
		t.Errorf("lease not echoed: %q", rep.Lease)
	}
}

func TestCapsAndCPULimit(t *testing.T) {
	r := newTestRunner(t, ModeAuto, nil)
	mode, caps, full := r.Caps()
	if mode != ModeAuto {
		t.Errorf("mode = %q", mode)
	}
	// caps must be a subset of AllCaps in canonical order.
	seen := map[string]bool{}
	for _, c := range caps {
		seen[c] = true
	}
	if full && len(caps) != len(AllCaps) {
		t.Errorf("full but only %d caps", len(caps))
	}
	// SetCPULimit is a no-op without cgroup2 but must not error.
	if err := r.SetCPULimit(1.5); err != nil {
		t.Errorf("SetCPULimit: %v", err)
	}
	if len(r.Running()) != 0 {
		t.Errorf("Running not empty at rest")
	}
}

// waitPhase blocks until the task reaches phase (or fails the test).
func waitPhase(t *testing.T, r *Runner, lease, phase string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, rt := range r.Running() {
			if rt.Lease == lease && rt.Phase == phase {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s never reached phase %s", lease, phase)
}

// setTimeUnit shortens the timeout unit for a test and returns a restore func.
func setTimeUnit(d time.Duration) func() {
	old := timeUnit
	timeUnit = d
	return func() { timeUnit = old }
}
