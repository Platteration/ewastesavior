package hive

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func TestStaleLeaseAndIdempotentReplay(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	other := h.newNode(nil)
	other.register()
	h.submit(scriptJob(1, nil))
	tk := n.claim(1)[0]
	if code := n.report(tk, proto.TaskReport{Lease: "wrong", State: proto.TaskSucceeded}); code != http.StatusConflict {
		t.Fatalf("wrong lease: %d", code)
	}
	// Another node can't report on our lease.
	if code := other.report(tk, proto.TaskReport{State: proto.TaskSucceeded}); code != http.StatusConflict {
		t.Fatalf("foreign node: %d", code)
	}
	if code := n.report(tk, proto.TaskReport{State: "bogus"}); code != http.StatusBadRequest {
		t.Fatalf("bad state: %d", code)
	}
	n.succeed(tk)
	// The same terminal report again is a harmless replay...
	if code := n.report(tk, proto.TaskReport{State: proto.TaskSucceeded}); code != 200 {
		t.Fatalf("replay: %d", code)
	}
	// ...a different outcome for that lease is not.
	if code := n.report(tk, proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrExit}); code != http.StatusConflict {
		t.Fatalf("conflicting replay: %d", code)
	}
	if code := n.report(tk, proto.TaskReport{State: proto.TaskRunning}); code != http.StatusConflict {
		t.Fatalf("running after done: %d", code)
	}
	if code, _ := n.api("POST", "tasks/tnope/report", proto.TaskReport{Lease: "x", State: proto.TaskSucceeded}, nil); code != http.StatusConflict {
		t.Fatalf("unknown task: %d", code)
	}
	if v := h.task(tk.ID); v.State != proto.TaskSucceeded {
		t.Fatalf("state changed by replays: %s", v.State)
	}
}

func TestLevelTriggeredCancel(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(3, nil))
	tasks := n.claim(2)
	var jv proto.JobView
	h.mustAdmin("POST", "jobs/"+d.ID+"/cancel", nil, &jv)
	if jv.State != proto.JobCanceled || jv.Counts.Canceled != 3 || jv.FinishedAt == nil {
		t.Fatalf("cancel: %+v", jv)
	}
	if v := h.task(tasks[0].ID); v.State != proto.TaskCanceled {
		t.Fatalf("assigned task shown as %s", v.State)
	}
	// Resources stay charged while the node still runs them.
	if v := h.nodeView(n.req.NodeID); v.Allocated.Cores != 2 || len(v.RunningTasks) != 0 {
		t.Fatalf("allocation during cancel: %+v", v)
	}
	for i := 0; i < 3; i++ {
		hb := n.heartbeat(running(tasks...)...)
		if len(hb.Directives.CancelTasks) != 2 {
			t.Fatalf("heartbeat %d: cancel %v", i, hb.Directives.CancelTasks)
		}
	}
	// A terminal report for a cancel-requested lease is accepted and keeps
	// the canceled state.
	if code := n.report(tasks[0], proto.TaskReport{State: proto.TaskCanceled, CPUSeconds: 1.5}); code != 200 {
		t.Fatalf("report after cancel: %d", code)
	}
	if v := h.task(tasks[0].ID); v.State != proto.TaskCanceled || v.CPUSeconds != 1.5 {
		t.Fatalf("after report: %+v", v)
	}
	// Still listed: still canceled.
	if hb := n.heartbeat(running(tasks[1])...); len(hb.Directives.CancelTasks) != 1 || hb.Directives.CancelTasks[0].ID != tasks[1].ID {
		t.Fatalf("cancel repeat: %v", hb.Directives.CancelTasks)
	}
	// Once the node stops listing it, the charge is released.
	if hb := n.heartbeat(); len(hb.Directives.CancelTasks) != 0 {
		t.Fatal("cancel directive for an unlisted task")
	}
	if v := h.nodeView(n.req.NodeID); v.Allocated.Cores != 0 {
		t.Fatalf("still allocated: %+v", v.Allocated)
	}
	// An unknown lease listed by a node is canceled too.
	hb := n.heartbeat(proto.RunningTask{ID: "tghost", Lease: "l", Phase: proto.PhaseRunning})
	if len(hb.Directives.CancelTasks) != 1 || hb.Directives.CancelTasks[0].ID != "tghost" {
		t.Fatalf("ghost task: %v", hb.Directives.CancelTasks)
	}
}

func TestMissingFromRunningTasksRequeue(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.MissingAfter = 200 * time.Millisecond })
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(2, nil))
	tasks := n.claim(2)
	// Right after dispatch a missing task is tolerated (claim response in flight).
	n.heartbeat(running(tasks[1])...)
	if v := h.task(tasks[0].ID); v.State != proto.TaskAssigned {
		t.Fatalf("requeued too early: %s", v.State)
	}
	time.Sleep(250 * time.Millisecond)
	n.heartbeat(running(tasks[1])...)
	v := h.task(tasks[0].ID)
	if v.State != proto.TaskPending || v.Interruptions != 0 || v.Failures != 0 || v.History[0].Outcome != "lost" {
		t.Fatalf("never-seen task: %+v", v)
	}
	// A task that was seen and then disappears counts as an interruption.
	n.heartbeat() // tasks[1] was listed before
	v = h.task(tasks[1].ID)
	if v.State != proto.TaskPending || v.Interruptions != 1 {
		t.Fatalf("seen task: %+v", v)
	}
	// Its old lease is stale now.
	if code := n.report(tasks[1], proto.TaskReport{State: proto.TaskSucceeded}); code != http.StatusConflict {
		t.Fatalf("stale report: %d", code)
	}
}

func TestOfflineRequeue(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) {
		c.OfflineAfter = 200 * time.Millisecond
		c.LostAfter = 200 * time.Millisecond
	})
	a := h.newNode(nil)
	a.register()
	h.submit(scriptJob(1, nil))
	tk := a.claim(1)[0]
	a.heartbeat(running(tk)...)
	eventually(t, "offline", func() bool { return h.nodeView(a.req.NodeID).Liveness == proto.NodeOffline })
	if v := h.task(tk.ID); v.State != proto.TaskAssigned && v.State != proto.TaskRunning {
		t.Fatalf("requeued before LostAfter: %s", v.State)
	}
	eventually(t, "lost requeue", func() bool { return h.task(tk.ID).State == proto.TaskPending })
	b := h.newNode(nil)
	b.register()
	got := b.claim(1)
	if len(got) != 1 || got[0].ID != tk.ID || got[0].Attempt != 2 {
		t.Fatalf("requeued task: %+v", got)
	}
	if v := h.task(tk.ID); v.Interruptions != 1 {
		t.Fatalf("interruptions: %d", v.Interruptions)
	}
	// The returning node is told to cancel its stale copy.
	a.register()
	if hb := a.heartbeat(running(tk)...); len(hb.Directives.CancelTasks) != 1 {
		t.Fatalf("stale copy not canceled: %v", hb.Directives.CancelTasks)
	}
}

func TestRunTimeDeadline(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(1, func(s *proto.JobSpec) { s.TimeoutS = 10; s.Retries = ptr(0) }))
	tk := n.claim(1)[0]
	rt := proto.RunningTask{ID: tk.ID, Lease: tk.Lease, Phase: proto.PhaseRunning, RunS: 65}
	if hb := n.heartbeat(rt); len(hb.Directives.CancelTasks) != 0 {
		t.Fatal("canceled within the grace period")
	}
	rt.RunS = 71
	hb := n.heartbeat(rt)
	if len(hb.Directives.CancelTasks) != 1 {
		t.Fatalf("deadline not enforced: %v", hb.Directives.CancelTasks)
	}
	v := h.task(tk.ID)
	if v.State != proto.TaskFailed || v.ErrorKind != proto.ErrTimeout || !strings.Contains(v.Error, "hive deadline") {
		t.Fatalf("deadline failure: %+v", v)
	}
	// Still charged until the node lets go.
	if nv := h.nodeView(n.req.NodeID); nv.Allocated.Cores != 1 {
		t.Fatalf("charge released early: %+v", nv.Allocated)
	}
	if code := n.report(tk, proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrTimeout}); code != http.StatusConflict {
		t.Fatalf("late report: %d", code)
	}
	if nv := h.nodeView(n.req.NodeID); nv.Allocated.Cores != 0 {
		t.Fatalf("charge not released after the 409: %+v", nv.Allocated)
	}
}

func TestTransferStallDeadline(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.tune.xferStall = 150 * time.Millisecond })
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(1, nil))
	tk := n.claim(1)[0]
	rt := proto.RunningTask{ID: tk.ID, Lease: tk.Lease, Phase: proto.PhaseFetching, XferBytes: 10}
	n.heartbeat(rt)
	for i := 0; i < 4; i++ { // progress keeps it alive
		time.Sleep(60 * time.Millisecond)
		rt.XferBytes += 100
		n.heartbeat(rt)
	}
	if v := h.task(tk.ID); v.State != proto.TaskAssigned {
		t.Fatalf("progressing transfer failed: %+v", v)
	}
	eventually(t, "stall deadline", func() bool { return h.task(tk.ID).ErrorKind == proto.ErrTimeout })
	if v := h.task(tk.ID); v.State != proto.TaskPending || v.Failures != 1 {
		t.Fatalf("stall: %+v", v)
	}
}

func TestErrorKindsAndQuarantine(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(1, nil)) // retries 1
	tk := n.claim(1)[0]
	n.report(tk, proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrExit, ExitCode: 1, RunS: 30})
	if v := h.task(tk.ID); v.State != proto.TaskPending || v.Failures != 1 {
		t.Fatalf("first exit failure: %+v", v)
	}
	tk = n.claim(1)[0]
	n.report(tk, proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrExit, ExitCode: 1, RunS: 30})
	if v := h.task(tk.ID); v.State != proto.TaskFailed || v.Failures != 2 || len(v.History) != 2 {
		t.Fatalf("out of attempts: %+v", v)
	}
	if j := h.job(d.ID); j.State != proto.JobFailed {
		t.Fatalf("job: %s", j.State)
	}

	// Node errors never consume attempts; three of them quarantine the node.
	h.submit(scriptJob(3, func(s *proto.JobSpec) { s.Retries = ptr(0) }))
	for i, kind := range []string{proto.ErrInput, proto.ErrOutput, ""} {
		got := n.claim(1)
		if len(got) != 1 {
			t.Fatalf("node error %d: no task", i)
		}
		n.report(got[0], proto.TaskReport{State: proto.TaskFailed, ErrorKind: kind, Error: "broken disk"})
		v := h.task(got[0].ID)
		if v.State != proto.TaskPending || v.Failures != 0 || v.NodeErrors < 1 {
			t.Fatalf("node error %d: %+v", i, v)
		}
		if kind == "" && v.ErrorKind != proto.ErrInternal {
			t.Fatalf("unknown kind should be internal: %q", v.ErrorKind)
		}
	}
	nv := h.nodeView(n.req.NodeID)
	if !strings.Contains(nv.Quarantine, "node errors") {
		t.Fatalf("not quarantined: %+v", nv.Quarantine)
	}
	if got := n.claim(1); len(got) != 0 {
		t.Fatal("quarantined node got a task")
	}
	h.mustAdmin("PATCH", "nodes/"+n.req.NodeID, proto.NodePatch{ClearQuarantine: true}, nil)
	if got := n.claim(1); len(got) != 1 {
		t.Fatal("cleared node got nothing")
	}
}

func TestFastFailureQuarantine(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	bad := h.newNode(nil)
	bad.register()
	h.submit(scriptJob(5, func(s *proto.JobSpec) { s.Retries = ptr(1); s.Resources.Cores = 0.5 }))
	failed := bad.claimWith(proto.ClaimRequest{ClaimID: "c1", Free: proto.Resources{Cores: 8, MemMB: 4096}, Max: 5})
	if len(failed) != 5 {
		t.Fatalf("claimed %d tasks", len(failed))
	}
	for _, tk := range failed {
		bad.report(tk, proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrExit, ExitCode: 127, RunS: 0.2})
	}
	// A job that fails fast everywhere says nothing about the node.
	if q := h.nodeView(bad.req.NodeID).Quarantine; q != "" {
		t.Fatalf("quarantined before any evidence: %q", q)
	}
	h.mustAdmin("PATCH", "nodes/"+bad.req.NodeID, proto.NodePatch{Drain: ptr(true)}, nil)
	good := h.newNode(nil)
	good.register()
	for range failed {
		tk := good.claim(1)[0]
		good.report(tk, proto.TaskReport{State: proto.TaskSucceeded, RunS: 1})
	}
	if q := h.nodeView(bad.req.NodeID).Quarantine; !strings.Contains(q, "succeeded on other nodes") {
		t.Fatalf("fast failures: %q", q)
	}
	if q := h.nodeView(good.req.NodeID).Quarantine; q != "" {
		t.Fatalf("good node quarantined: %q", q)
	}
}

func TestBrokenJobDoesNotQuarantine(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	var nodes []*testNode
	for i := 0; i < 2; i++ {
		n := h.newNode(nil)
		n.register()
		nodes = append(nodes, n)
	}
	h.submit(scriptJob(10, func(s *proto.JobSpec) { s.Retries = ptr(1) }))
	for i := 0; i < 20; i++ {
		n := nodes[i%2]
		got := n.claim(1)
		if len(got) == 0 {
			break
		}
		n.report(got[0], proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrExit, ExitCode: 1, RunS: 0.1})
	}
	for _, n := range nodes {
		if q := h.nodeView(n.req.NodeID).Quarantine; q != "" {
			t.Fatalf("a broken job quarantined %s: %q", n.req.NodeID, q)
		}
	}
}

func TestPreemptedAndInterruptionCap(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Retries = ptr(0) }))
	// 1 + retries + MaxInterruptions = 7 dispatches allowed.
	for i := 1; i <= 7; i++ {
		got := n.claim(1)
		if len(got) != 1 || got[0].Attempt != i {
			t.Fatalf("dispatch %d: %+v", i, got)
		}
		n.report(got[0], proto.TaskReport{State: proto.TaskPreempted, Error: "battery low"})
		v := h.task(got[0].ID)
		if v.Failures != 0 || v.Interruptions != i {
			t.Fatalf("preempt %d: %+v", i, v)
		}
		if i < 7 && v.State != proto.TaskPending {
			t.Fatalf("preempt %d: %s", i, v.State)
		}
		if i == 7 && (v.State != proto.TaskFailed || !strings.Contains(v.Error, "too many interruptions")) {
			t.Fatalf("cap: %+v", v)
		}
	}
}

func TestOutputsValidatedOnReport(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(1, nil))
	tk := n.claim(1)[0]
	for _, outs := range [][]proto.Output{
		{{Name: "../etc/passwd", Blob: sha([]byte("x")), Size: 1}},
		{{Name: "ok.txt", Blob: sha([]byte("never uploaded")), Size: 14}},
	} {
		code := n.report(tk, proto.TaskReport{State: proto.TaskSucceeded, Outputs: outs})
		if code != http.StatusBadRequest {
			t.Fatalf("bad outputs accepted: %d", code)
		}
		v := h.task(tk.ID)
		if v.State != proto.TaskPending || v.ErrorKind != proto.ErrOutput || v.NodeErrors == 0 {
			t.Fatalf("bad outputs: %+v", v)
		}
		tk = n.claim(1)[0]
	}
}
