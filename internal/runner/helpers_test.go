package runner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// fakeTree implements treeLister over a map of path -> type.
type fakeTree map[string]entryType

func (f fakeTree) list(dir string) ([]dirEntry, error) {
	var out []dirEntry
	for p, typ := range f {
		parent, name := "", p
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			parent, name = p[:i], p[i+1:]
		}
		if parent == dir {
			out = append(out, dirEntry{name: name, typ: typ})
		}
	}
	return out, nil
}

func (f fakeTree) lstat(p string) (entryType, bool) {
	typ, ok := f[p]
	return typ, ok
}

func TestMatchOutputs(t *testing.T) {
	tree := fakeTree{
		"result.txt":      entryFile,
		"log.txt":         entryFile,
		"link.txt":        entryOther, // symlink
		"fifo":            entryOther,
		"out":             entryDir,
		"out/a.png":       entryFile,
		"out/b.png":       entryFile,
		"out/c.jpg":       entryFile,
		"out/deep":        entryDir,
		"out/deep/x.png":  entryFile,
		"linkdir":         entryOther, // symlink to a directory
		".savior-script":  entryFile,
		"bad:name.txt":    entryFile,
		"-dash.txt":       entryFile,
		"frames":          entryDir,
		"frames/0001.png": entryFile,
	}
	cases := []struct {
		name     string
		patterns []string
		want     []string
		noteSub  string
	}{
		{"literal", []string{"result.txt"}, []string{"result.txt"}, ""},
		{"missing literal", []string{"nope.txt"}, []string{}, ""},
		{"star txt", []string{"*.txt"}, []string{"log.txt", "result.txt"}, "link.txt"},
		{"subdir glob", []string{"out/*.png"}, []string{"out/a.png", "out/b.png"}, ""},
		{"dir glob", []string{"*/*.png"}, []string{"frames/0001.png", "out/a.png", "out/b.png"}, ""},
		{"deep", []string{"out/*/*.png"}, []string{"out/deep/x.png"}, ""},
		{"no symlinked dirs", []string{"linkdir/*"}, []string{}, ""},
		{"directory match noted", []string{"out"}, []string{}, "is a directory"},
		{"dedupe", []string{"*.txt", "result.txt", "res*"}, []string{"log.txt", "result.txt"}, ""},
		{"reserved names skipped silently", []string{"*"}, []string{"log.txt", "result.txt"}, "fifo"},
		{"invalid names noted", []string{"*name.txt"}, []string{}, "name not allowed"},
		{"char class", []string{"out/[ab].png"}, []string{"out/a.png", "out/b.png"}, ""},
		{"invalid pattern ignored", []string{"../x"}, []string{}, "invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, notes, err := matchOutputs(tree, c.patterns)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
			if c.noteSub != "" && !strings.Contains(strings.Join(notes, "\n"), c.noteSub) {
				t.Errorf("notes %q lack %q", notes, c.noteSub)
			}
			for _, n := range notes {
				if strings.Contains(n, ".savior") {
					t.Errorf("reserved name was noted: %q", n)
				}
			}
		})
	}
}

// bigTree lists a huge directory to exercise the scan bound.
type bigTree struct{}

func (bigTree) list(string) ([]dirEntry, error) {
	return make([]dirEntry, maxScanEntries+1), nil
}
func (bigTree) lstat(string) (entryType, bool) { return entryDir, true }

func TestMatchOutputsScanBound(t *testing.T) {
	if _, _, err := matchOutputs(bigTree{}, []string{"*"}); err == nil {
		t.Fatal("expected the scan bound to trip")
	}
}

func baseTask() proto.Task {
	return proto.Task{
		ID: "t0123456789abcdef", Lease: "L1", JobID: "j1", Index: 3, Count: 10, Attempt: 2,
		Kind: proto.KindExec, Command: []string{"/bin/true"}, TimeoutS: 60,
		Resources: proto.Resources{Cores: 1, MemMB: 64, DiskMB: 16},
	}
}

func TestValidateTask(t *testing.T) {
	sha := strings.Repeat("a", 64)
	cases := []struct {
		name string
		mod  func(*proto.Task)
		kind string
	}{
		{"ok", func(*proto.Task) {}, ""},
		{"bad id", func(t *proto.Task) { t.ID = "../x" }, proto.ErrInternal},
		{"dot id", func(t *proto.Task) { t.ID = ".x" }, proto.ErrInternal},
		{"no lease", func(t *proto.Task) { t.Lease = "" }, proto.ErrInternal},
		{"no timeout", func(t *proto.Task) { t.TimeoutS = 0 }, proto.ErrInternal},
		{"no command", func(t *proto.Task) { t.Command = nil }, proto.ErrInternal},
		{"nul arg", func(t *proto.Task) { t.Command = []string{"a\x00b"} }, proto.ErrInternal},
		{"empty script", func(t *proto.Task) { t.Kind, t.Command, t.Script = proto.KindScript, nil, "  " }, proto.ErrInternal},
		{"bad kind", func(t *proto.Task) { t.Kind = "docker" }, proto.ErrInternal},
		{"bad cores", func(t *proto.Task) { t.Resources.Cores = 0 }, proto.ErrInternal},
		{"input escape", func(t *proto.Task) { t.Inputs = []proto.Input{{Name: "../etc/passwd", Blob: sha}} }, proto.ErrInput},
		{"input dup", func(t *proto.Task) { t.Inputs = []proto.Input{{Name: "a", Blob: sha}, {Name: "a", Blob: sha}} }, proto.ErrInput},
		{"input nested in file", func(t *proto.Task) { t.Inputs = []proto.Input{{Name: "a", Blob: sha}, {Name: "a/b", Blob: sha}} }, proto.ErrInput},
		{"input both", func(t *proto.Task) { t.Inputs = []proto.Input{{Name: "a", Blob: sha, URL: "http://x"}} }, proto.ErrInput},
		{"url no size", func(t *proto.Task) { t.Inputs = []proto.Input{{Name: "a", URL: "http://x", SHA256: sha}} }, proto.ErrInput},
		{"url ok", func(t *proto.Task) { t.Inputs = []proto.Input{{Name: "d/a", URL: "http://x", SHA256: sha, Size: 5}} }, ""},
		{"bad output", func(t *proto.Task) { t.Outputs = []string{"/abs"} }, proto.ErrOutput},
	}
	for _, c := range cases {
		task := baseTask()
		c.mod(&task)
		if kind, reason := validateTask(task); kind != c.kind {
			t.Errorf("%s: kind %q (%s), want %q", c.name, kind, reason, c.kind)
		}
	}
}

func TestBuildEnv(t *testing.T) {
	task := baseTask()
	task.Env = map[string]string{
		"FOO":            "bar",
		"LANG":           "de_DE.UTF-8",
		"SAVIOR_TASK_ID": "spoofed",
		"SAVIOR_EXTRA":   "hive-set",
		"bad-key":        "x",
		"NUL":            "a\x00b",
	}
	env := buildEnv(task, "/work", "/tmp")
	want := []string{
		"FOO=bar",
		"HOME=/work",
		"LANG=de_DE.UTF-8",
		"PATH=" + taskPath,
		"SAVIOR_ATTEMPT=2",
		"SAVIOR_EXTRA=hive-set",
		"SAVIOR_JOB_ID=j1",
		"SAVIOR_TASK_COUNT=10",
		"SAVIOR_TASK_ID=t0123456789abcdef",
		"SAVIOR_TASK_INDEX=3",
		"TMPDIR=/tmp",
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env:\n got %q\nwant %q", env, want)
	}
}

func TestTaskArgvAndNames(t *testing.T) {
	task := baseTask()
	if got := taskDirName(task); got != "t0123456789abcdef.2" {
		t.Errorf("taskDirName = %q", got)
	}
	if got := taskArgv(task, "/work/.savior-script"); !reflect.DeepEqual(got, []string{"/bin/true"}) {
		t.Errorf("exec argv = %q", got)
	}
	task.Kind, task.Command, task.Script = proto.KindScript, nil, "echo hi"
	if got := taskArgv(task, "/work/.savior-script"); !reflect.DeepEqual(got, []string{"/bin/sh", "/work/.savior-script"}) {
		t.Errorf("script argv = %q", got)
	}
	task.Resources.DiskMB = 0
	if effectiveDiskMB(task) != 1 {
		t.Error("disk_mb 0 must map to 1 (tmpfs size=0 is unlimited)")
	}
}

func TestSlotPool(t *testing.T) {
	p := newSlotPool(2)
	ctx := context.Background()
	a, _ := p.acquire(ctx)
	b, _ := p.acquire(ctx)
	if a == b || p.free() != 0 {
		t.Fatalf("slots %d %d free %d", a, b, p.free())
	}
	got := make(chan int, 1)
	go func() {
		s, _ := p.acquire(ctx)
		got <- s
	}()
	time.Sleep(20 * time.Millisecond)
	p.release(a, false)
	select {
	case s := <-got:
		if s != a {
			t.Errorf("waiter got slot %d, want %d", s, a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken")
	}
	if p.usable() != 2 {
		t.Errorf("usable = %d before any retire", p.usable())
	}
	// A retired slot is never handed out again.
	p.release(b, true)
	if p.usable() != 1 {
		t.Errorf("usable = %d after a retire, want 1", p.usable())
	}
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := p.acquire(cctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("acquire after retire: %v", err)
	}
}

func TestRunClock(t *testing.T) {
	var c runClock
	t0 := time.Unix(1000, 0)
	if c.elapsed(t0) != 0 {
		t.Fatal("unstarted clock must read 0")
	}
	c.start(t0, false)
	c.pause(t0.Add(2 * time.Second))
	if got := c.elapsed(t0.Add(10 * time.Second)); got != 2*time.Second {
		t.Errorf("paused elapsed = %v", got)
	}
	c.resume(t0.Add(10 * time.Second))
	if got := c.elapsed(t0.Add(13 * time.Second)); got != 5*time.Second {
		t.Errorf("resumed elapsed = %v", got)
	}
	c.pause(t0.Add(14 * time.Second))
	c.pause(t0.Add(20 * time.Second)) // double pause is a no-op
	if got := c.elapsed(t0.Add(30 * time.Second)); got != 6*time.Second {
		t.Errorf("elapsed = %v", got)
	}
	var d runClock
	d.start(t0, true)
	if d.elapsed(t0.Add(time.Hour)) != 0 {
		t.Error("clock started paused must not advance")
	}
}

type fakeProc struct {
	freezeErr error
	frozen    bool
	killed    bool
}

func (f *fakeProc) freeze(fr bool) error {
	if f.freezeErr != nil {
		return f.freezeErr
	}
	f.frozen = fr
	return nil
}
func (f *fakeProc) kill() { f.killed = true }

func TestTaskStateFreezeAndPreempt(t *testing.T) {
	var phases []string
	ts := newTaskState("t1", "L", func(rt proto.RunningTask) { phases = append(phases, rt.Phase) })
	// Freeze before the process exists is remembered and applied at start.
	if err := ts.setFrozen(true); err != nil {
		t.Fatal(err)
	}
	pc := &fakeProc{}
	if err := ts.started(pc); err != nil {
		t.Fatal(err)
	}
	if !pc.frozen || ts.snapshot().Phase != proto.PhaseFrozen {
		t.Fatalf("pending freeze not applied: %+v", ts.snapshot())
	}
	time.Sleep(30 * time.Millisecond)
	if ts.runTime() != 0 {
		t.Errorf("frozen task accumulated %v", ts.runTime())
	}
	if err := ts.setFrozen(false); err != nil || ts.snapshot().Phase != proto.PhaseRunning {
		t.Fatalf("unfreeze: %v %+v", err, ts.snapshot())
	}
	// A failing freeze must not stop the clock.
	pc.freezeErr = errors.New("no freezer")
	if err := ts.setFrozen(true); err == nil {
		t.Fatal("expected freeze error")
	}
	before := ts.runTime()
	time.Sleep(30 * time.Millisecond)
	if ts.runTime() <= before {
		t.Error("clock stopped although the freeze failed")
	}
	ts.preempt()
	if !pc.killed || !ts.isPreempted() {
		t.Error("preempt did not kill")
	}
	if len(phases) == 0 {
		t.Error("no progress reports")
	}
}
