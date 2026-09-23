package hive

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

func TestClaimFitAndClamps(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	small := h.newNode(func(r *proto.RegisterRequest) {
		r.Inventory.Cores = 2
		r.Total = proto.Resources{Cores: 64, MemMB: 1 << 20, DiskMB: 1000} // lies: clamped to inventory
	})
	small.register()
	v := h.nodeView(small.req.NodeID)
	if v.Status.Total != (proto.Resources{}) || v.Allocated != (proto.Resources{}) {
		t.Fatalf("fresh node view: %+v", v)
	}
	job := h.submit(scriptJob(5, nil))
	tasks := small.claimWith(proto.ClaimRequest{ClaimID: "c1", Free: small.req.Total, Max: 16})
	if len(tasks) != 2 {
		t.Fatalf("Total clamped to 2 inventory cores, got %d tasks", len(tasks))
	}
	if tk := tasks[0]; tk.JobID != job.ID || tk.Attempt != 1 || tk.Count != 5 || tk.Script != "echo 0" ||
		tk.Env["SAVIOR_TASK_INDEX"] != "0" || tk.Env["SAVIOR_TASK_COUNT"] != "5" || tk.Env["SAVIOR_JOB_ID"] != job.ID ||
		tk.Env["SAVIOR_TASK_ID"] != tk.ID || tk.Env["SAVIOR_ATTEMPT"] != "1" || len(tk.Lease) != 32 || tk.TimeoutS != 3600 {
		t.Fatalf("task message: %+v", tk)
	}
	if tasks[1].Script != "echo 1" {
		t.Fatalf("template expansion: %q", tasks[1].Script)
	}
	if more := small.claim(16); len(more) != 0 {
		t.Fatalf("allocated node got more: %d", len(more))
	}
	if v := h.nodeView(small.req.NodeID); v.Allocated.Cores != 2 || len(v.RunningTasks) != 2 {
		t.Fatalf("allocation: %+v", v)
	}

	// The node's own free view clamps too, and max limits the count.
	big := h.newNode(func(r *proto.RegisterRequest) { r.Inventory.Cores = 32; r.Total.Cores = 32 })
	big.register()
	if got := big.claimWith(proto.ClaimRequest{ClaimID: "x", Free: proto.Resources{Cores: 1.5, MemMB: 4096, DiskMB: 1000}, Max: 16}); len(got) != 1 {
		t.Fatalf("free clamp: %d", len(got))
	}
	if got := big.claimWith(proto.ClaimRequest{ClaimID: "y", Free: big.req.Total, Max: 1}); len(got) != 1 {
		t.Fatalf("max=1: %d", len(got))
	}
	h.submit(scriptJob(40, nil))
	if got := big.claimWith(proto.ClaimRequest{ClaimID: "z", Free: big.req.Total, Max: 100}); len(got) != 16 {
		t.Fatalf("max clamps to 16: %d", len(got))
	}
	// Memory is a hard limit as well.
	h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Resources.MemMB = 5000; s.Priority = 10 }))
	if got := big.claimWith(proto.ClaimRequest{ClaimID: "m", Free: proto.Resources{Cores: 8, MemMB: 4096, DiskMB: 1000}, Max: 1}); len(got) == 1 && got[0].Resources.MemMB == 5000 {
		t.Fatal("task exceeding memory dispatched")
	}
}

func TestScratchInRAMCharges(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(func(r *proto.RegisterRequest) {
		r.ScratchInRAM = true
		r.Total = proto.Resources{Cores: 4, MemMB: 1000}
	})
	n.register()
	h.submit(scriptJob(4, func(s *proto.JobSpec) { s.Resources = proto.Resources{Cores: 0.5, MemMB: 200, DiskMB: 200} }))
	// 400 MB per task charged to memory: two fit in 1000 MB.
	if got := n.claimWith(proto.ClaimRequest{ClaimID: "a", Free: n.req.Total, Max: 16}); len(got) != 2 {
		t.Fatalf("scratch in RAM: %d tasks", len(got))
	}
	if v := h.nodeView(n.req.NodeID); v.Allocated.MemMB != 800 || v.Allocated.DiskMB != 0 {
		t.Fatalf("allocated: %+v", v.Allocated)
	}
}

func TestRequirements(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(func(r *proto.RegisterRequest) { r.Name = "alpha"; r.Labels = map[string]string{"room": "a"} })
	n.register()
	weak := h.newNode(func(r *proto.RegisterRequest) {
		r.Name = "weak"
		r.SandboxCaps = []string{"nnp", "privdrop"}
		r.Sandbox = "none"
	})
	weak.register()
	display := h.newNode(func(r *proto.RegisterRequest) { r.Roles = []proto.Role{proto.RoleDisplay} })
	display.register()

	cases := []struct {
		name  string
		mod   func(*proto.JobSpec)
		alpha bool
		weak  bool
	}{
		{"arch mismatch", func(s *proto.JobSpec) { s.Requirements.Arch = []string{"386"} }, false, false},
		{"arch match", func(s *proto.JobSpec) { s.Requirements.Arch = []string{"386", "amd64"} }, true, false},
		{"min mem", func(s *proto.JobSpec) { s.Requirements.MinMemMB = 16384 }, false, false},
		{"cpu flags", func(s *proto.JobSpec) { s.Requirements.CPUFlags = []string{"sse2", "avx"} }, false, false},
		{"labels", func(s *proto.JobSpec) { s.Requirements.Labels = map[string]string{"room": "a"} }, true, false},
		{"isolation any", func(s *proto.JobSpec) { s.Requirements.Isolation = proto.IsolationAny }, true, true},
		{"nodes by name", func(s *proto.JobSpec) {
			s.Requirements.Nodes = []string{"weak"}
			s.Requirements.Isolation = proto.IsolationAny
		}, false, true},
	}
	for _, c := range cases {
		job := h.submit(scriptJob(2, c.mod))
		got := n.claim(1)
		if (len(got) == 1) != c.alpha {
			t.Fatalf("%s: alpha got %d tasks", c.name, len(got))
		}
		wgot := weak.claim(1)
		if (len(wgot) == 1) != c.weak {
			t.Fatalf("%s: weak got %d tasks", c.name, len(wgot))
		}
		if len(display.claim(1)) != 0 {
			t.Fatalf("%s: node without compute role got a task", c.name)
		}
		for _, tk := range append(got, wgot...) {
			if tk.JobID != job.ID {
				t.Fatalf("%s: wrong job", c.name)
			}
		}
		h.mustAdmin("POST", "jobs/"+job.ID+"/cancel", nil, nil)
	}
	// Names resolve to IDs at submit; unknown names are refused.
	d := h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Requirements.Nodes = []string{"alpha"} }))
	if len(d.Spec.Requirements.Nodes) != 1 || d.Spec.Requirements.Nodes[0] != n.req.NodeID {
		t.Fatalf("nodes not resolved: %v", d.Spec.Requirements.Nodes)
	}
	if st := h.admin("POST", "jobs", scriptJob(1, func(s *proto.JobSpec) { s.Requirements.Nodes = []string{"nobody"} }), nil); st != http.StatusBadRequest {
		t.Fatalf("unknown node: %d", st)
	}
	h.mustAdmin("POST", "jobs/"+d.ID+"/cancel", nil, nil)

	// Admin labels override config labels key by key.
	h.mustAdmin("PATCH", "nodes/alpha", proto.NodePatch{Labels: &map[string]string{"room": "b"}}, nil)
	job := h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Requirements.Labels = map[string]string{"room": "a"} }))
	if len(n.claim(1)) != 0 {
		t.Fatal("admin label should override the config label")
	}
	if hb := n.heartbeat(); hb.Directives.Labels["room"] != "b" {
		t.Fatalf("effective labels: %v", hb.Directives.Labels)
	}
	h.mustAdmin("POST", "jobs/"+job.ID+"/cancel", nil, nil)

	// Draining nodes get nothing.
	h.submit(scriptJob(1, nil))
	h.mustAdmin("PATCH", "nodes/alpha", proto.NodePatch{Drain: ptr(true)}, nil)
	if len(n.claim(1)) != 0 {
		t.Fatal("draining node got a task")
	}
	if !n.heartbeat().Directives.Drain {
		t.Fatal("drain directive")
	}
	h.mustAdmin("PATCH", "nodes/alpha", proto.NodePatch{Drain: ptr(false)}, nil)
	if len(n.claim(1)) != 1 {
		t.Fatal("undrained node got nothing")
	}
}

func TestPriorityOrder(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	low := h.submit(scriptJob(3, nil))
	high := h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Priority = 5 }))
	low2 := h.submit(scriptJob(1, nil))
	var got []string
	for i := 0; i < 5; i++ {
		ts := n.claim(1)
		if len(ts) != 1 {
			t.Fatalf("claim %d: %d", i, len(ts))
		}
		got = append(got, ts[0].JobID)
		n.succeed(ts[0])
	}
	want := []string{high.ID, low.ID, low.ID, low.ID, low2.ID}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue order %v, want %v", got, want)
		}
	}
}

func TestAntiAffinity(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	a := h.newNode(nil)
	a.register()
	b := h.newNode(nil)
	b.register()
	h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Retries = ptr(0) }))
	ta := a.claim(1)
	if len(ta) != 1 {
		t.Fatal("no task")
	}
	if code := a.report(ta[0], proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrInput, Error: "download failed"}); code != 200 {
		t.Fatalf("report: %d", code)
	}
	v := h.task(ta[0].ID)
	if v.State != proto.TaskPending || v.NodeErrors != 1 || v.Failures != 0 || len(v.FailedNodes) != 1 || v.FailedNodes[0] != a.req.NodeID {
		t.Fatalf("node error requeue: %+v", v)
	}
	if got := a.claim(1); len(got) != 0 {
		t.Fatal("failed node got the task back while another node is eligible")
	}
	tb := b.claim(1)
	if len(tb) != 1 || tb[0].ID != ta[0].ID || tb[0].Attempt != 2 {
		t.Fatalf("other node: %+v", tb)
	}
	b.report(tb[0], proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrSandbox})
	// Every eligible node has failed it: anti-affinity yields.
	if got := a.claim(1); len(got) != 1 {
		t.Fatal("anti-affinity must yield when no other node is eligible")
	}
}

func TestClaimIDIdempotent(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(6, nil))
	req := proto.ClaimRequest{ClaimID: auth.NewID(8), Free: proto.Resources{Cores: 2, MemMB: 4096, DiskMB: 1000}, Max: 2}
	first := n.claimWith(req)
	again := n.claimWith(req)
	if len(first) != 2 || len(again) != 2 || first[0].Lease != again[0].Lease || first[1].ID != again[1].ID {
		t.Fatalf("replay: %v vs %v", first, again)
	}
	req.ClaimID = auth.NewID(8)
	third := n.claimWith(req)
	if len(third) != 2 || third[0].ID == first[0].ID {
		t.Fatalf("new claim id must get new tasks: %v", third)
	}
	if d := h.job(first[0].JobID); d.Counts.Assigned != 4 || d.Counts.Pending != 2 {
		t.Fatalf("counts after replay: %+v", d.Counts)
	}
}

func TestClaimLongPollAndConcurrencyLimit(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	start := time.Now()
	go func() {
		time.Sleep(150 * time.Millisecond)
		h.submit(scriptJob(1, nil))
	}()
	got := n.claimWith(proto.ClaimRequest{ClaimID: "lp", Free: n.req.Total, Max: 1, WaitS: 10})
	if len(got) != 1 || time.Since(start) > 3*time.Second {
		t.Fatalf("long poll: %d tasks after %v", len(got), time.Since(start))
	}
	// Four concurrent long polls are allowed, the fifth is refused.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.claimWith(proto.ClaimRequest{ClaimID: auth.NewID(4), Free: proto.Resources{Cores: 0.01}, Max: 1, WaitS: 2})
		}()
	}
	eventually(t, "4 claims in flight", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return h.s.nodes[n.req.NodeID].claims == 4
	})
	if code, _ := n.api("POST", "claim", proto.ClaimRequest{ClaimID: "5th", Max: 1}, nil); code != http.StatusTooManyRequests {
		t.Fatalf("fifth claim: %d", code)
	}
	wg.Wait()
}

func TestReservationForWideTask(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.ReserveAfter = 150 * time.Millisecond })
	n := h.newNode(func(r *proto.RegisterRequest) { r.Name = "quad" })
	n.register()
	narrow := h.submit(scriptJob(3, nil))
	busy := n.claim(3)
	if len(busy) != 3 {
		t.Fatalf("fill: %d", len(busy))
	}
	hbStatus := func(free float64, rt []proto.Task) {
		n.heartbeatStatus(proto.NodeStatus{State: proto.NodeBusy, Total: n.req.Total,
			Free: proto.Resources{Cores: free, MemMB: 4096, DiskMB: 10000}, RunningTasks: running(rt...)})
	}
	hbStatus(1, busy)
	wide := h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Priority = 10; s.Resources.Cores = 4 }))
	backfill := h.submit(scriptJob(10, nil))
	_ = narrow
	// Without a reservation the free core would be backfilled; wait for it.
	var wideTask string
	eventually(t, "reservation", func() bool {
		v := h.nodeView("quad")
		wideTask = v.ReservedFor
		return wideTask != ""
	})
	if tv := h.task(wideTask); tv.JobID != wide.ID || tv.WaitReason != "reserved for quad" {
		t.Fatalf("reserved task: %+v", tv)
	}
	// While reserved the node gets nothing else, even with free capacity.
	for i, tk := range busy {
		if got := n.claim(4); len(got) != 0 {
			t.Fatalf("reserved node backfilled after %d finished: %v", i, got)
		}
		n.succeed(tk)
		hbStatus(float64(2+i), busy[i+1:])
	}
	got := n.claim(4)
	if len(got) != 1 || got[0].ID != wideTask || got[0].Resources.Cores != 4 {
		t.Fatalf("wide task not dispatched: %+v", got)
	}
	if v := h.nodeView("quad"); v.ReservedFor != "" {
		t.Fatal("reservation not cleared on dispatch")
	}
	n.succeed(got[0])
	if got := n.claim(4); len(got) != 4 || got[0].JobID != backfill.ID {
		t.Fatalf("after the wide task: %v", got)
	}
}

func TestJobStatesAndCounts(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(3, func(s *proto.JobSpec) { s.Retries = ptr(0) }))
	if d.State != proto.JobQueued || d.Counts.Pending != 3 || d.Spec.Kind != proto.KindScript || d.Spec.TimeoutS != 3600 ||
		*d.Spec.Retries != 0 || d.Spec.Requirements.Isolation != proto.IsolationFull || d.Seq == 0 {
		t.Fatalf("submitted: %+v", d)
	}
	tasks := n.claim(3)
	if j := h.job(d.ID); j.State != proto.JobRunning || j.Counts.Assigned != 3 || j.StartedAt == nil {
		t.Fatalf("running: %+v", j)
	}
	n.report(tasks[0], proto.TaskReport{State: proto.TaskRunning})
	if j := h.job(d.ID); j.Counts.Running != 1 || j.Counts.Assigned != 2 {
		t.Fatalf("running counts: %+v", j.Counts)
	}
	n.succeed(tasks[0])
	n.succeed(tasks[1])
	n.report(tasks[2], proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrExit, ExitCode: 3, Error: "boom"})
	j := h.job(d.ID)
	if j.State != proto.JobFailed || j.Counts.Succeeded != 2 || j.Counts.Failed != 1 || j.FinishedAt == nil {
		t.Fatalf("finished: %+v", j)
	}
	tv := h.task(tasks[2].ID)
	if tv.ExitCode == nil || *tv.ExitCode != 3 || tv.ErrorKind != proto.ErrExit || tv.Failures != 1 ||
		len(tv.History) != 1 || tv.History[0].Outcome != "failed" || tv.History[0].ExitCode != 3 {
		t.Fatalf("failed task: %+v", tv)
	}
	ok := h.submit(scriptJob(1, nil))
	n.succeed(n.claim(1)[0])
	if j := h.job(ok.ID); j.State != proto.JobSucceeded {
		t.Fatalf("succeeded: %+v", j)
	}
	// Listing: newest first, state filter, before.
	var list []proto.JobView
	h.mustAdmin("GET", "jobs", nil, &list)
	if len(list) != 2 || list[0].ID != ok.ID || list[1].ID != d.ID {
		t.Fatalf("list: %+v", list)
	}
	h.mustAdmin("GET", "jobs?state=failed", nil, &list)
	if len(list) != 1 || list[0].ID != d.ID {
		t.Fatalf("filter: %+v", list)
	}
	h.mustAdmin("GET", "jobs?before="+itoa(ok.Seq), nil, &list)
	if len(list) != 1 || list[0].ID != d.ID {
		t.Fatalf("before: %+v", list)
	}
	// Invalid specs are 400 with a reason; unknown fields are refused.
	if st := h.admin("POST", "jobs", proto.JobSpec{Script: " "}, nil); st != http.StatusBadRequest {
		t.Fatalf("empty script: %d", st)
	}
	if st := h.admin("POST", "jobs", map[string]any{"script": "true", "comand": "x"}, nil); st != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", st)
	}
	if st := h.admin("POST", "jobs", scriptJob(1, func(s *proto.JobSpec) {
		s.Inputs = []proto.Input{{Name: "in", Blob: sha([]byte("missing"))}}
	}), nil); st != http.StatusBadRequest {
		t.Fatalf("missing input blob: %d", st)
	}
	// Delete: finished jobs only.
	running := h.submit(scriptJob(1, nil))
	if st := h.admin("DELETE", "jobs/"+running.ID, nil, nil); st != http.StatusConflict {
		t.Fatalf("delete unfinished: %d", st)
	}
	h.mustAdmin("DELETE", "jobs/"+d.ID, nil, nil)
	if st := h.admin("GET", "tasks/"+tasks[0].ID, nil, nil); st != http.StatusNotFound {
		t.Fatalf("task of deleted job: %d", st)
	}
}

func TestLazyTaskRecords(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	d := h.submit(scriptJob(10000, nil))
	var page proto.TaskPage
	h.mustAdmin("GET", "jobs/"+d.ID+"/tasks", nil, &page)
	if page.Total != 0 || page.Undispatched != 10000 || len(page.Tasks) != 0 {
		t.Fatalf("page: %+v", page)
	}
	h.s.mu.Lock()
	recs := h.s.taskRecords
	h.s.mu.Unlock()
	if recs != 0 {
		t.Fatalf("records created eagerly: %d", recs)
	}
	n := h.newNode(func(r *proto.RegisterRequest) { r.Inventory.Cores = 32; r.Total.Cores = 32 })
	n.register()
	got := n.claim(16)
	if len(got) != 16 || got[15].Index != 15 {
		t.Fatalf("claim: %d", len(got))
	}
	h.mustAdmin("GET", "jobs/"+d.ID+"/tasks?limit=10&offset=5", nil, &page)
	if page.Total != 16 || page.Undispatched != 9984 || len(page.Tasks) != 10 || page.Tasks[0].Index != 5 || page.NextOffset != 15 {
		t.Fatalf("page 2: total %d undispatched %d len %d next %d", page.Total, page.Undispatched, len(page.Tasks), page.NextOffset)
	}
	if j := h.job(d.ID); j.Counts.Pending != 9984 || j.Counts.Assigned != 16 {
		t.Fatalf("counts: %+v", j.Counts)
	}
	// Cancel: undispatched tasks become canceled without records.
	var jv proto.JobView
	h.mustAdmin("POST", "jobs/"+d.ID+"/cancel", nil, &jv)
	if jv.State != proto.JobCanceled || jv.Counts.Canceled != 10000 || jv.Counts.Pending != 0 {
		t.Fatalf("cancel: %+v", jv.Counts)
	}
	h.s.mu.Lock()
	recs = h.s.taskRecords
	h.s.mu.Unlock()
	if recs != 16 {
		t.Fatalf("records: %d", recs)
	}
}

func TestJobWarning(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Resources.Cores = 8 }))
	if d.Warning == "" {
		t.Fatal("expected a warning for a task that fits no node")
	}
	if ok := h.submit(scriptJob(1, nil)); ok.Warning != "" {
		t.Fatalf("unexpected warning: %s", ok.Warning)
	}
}

func itoa(n uint64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}

func TestRetention(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	addFinished := func(tasks int) *job {
		j := &job{jobRecord: jobRecord{ID: "j" + itoa(s.nextSeq), Seq: s.nextSeq, Spec: proto.JobSpec{Count: 1}}}
		s.nextSeq++
		j.FinishedAt = timePtr(time.Now())
		s.markDoneLocked(j)
		j.counts.Succeeded = 1
		for i := 0; i < tasks; i++ {
			tk := &task{taskRecord: taskRecord{ID: j.ID + "-t" + itoa(uint64(i)), State: proto.TaskSucceeded}, job: j}
			j.tasks = append(j.tasks, tk)
			s.tasks[tk.ID] = tk
		}
		s.taskRecords += tasks
		s.jobs[j.ID] = j
		return j
	}
	first := addFinished(1)
	for i := 0; i < maxFinishedJobs+4; i++ {
		addFinished(1)
	}
	s.enforceRetentionLocked(nil)
	if len(s.jobs) != maxFinishedJobs || s.jobs[first.ID] != nil || s.taskRecords != maxFinishedJobs {
		t.Fatalf("finished-job cap: %d jobs, %d records", len(s.jobs), s.taskRecords)
	}
	// The task-record cap removes the oldest finished jobs too.
	big := addFinished(maxTaskRecords)
	s.enforceRetentionLocked(nil)
	if s.taskRecords > maxTaskRecords || s.jobs[big.ID] == nil {
		t.Fatalf("record cap: %d records, newest kept %v", s.taskRecords, s.jobs[big.ID] != nil)
	}
	if len(s.jobs) != 1 {
		t.Fatalf("expected only the newest big job to remain, have %d", len(s.jobs))
	}
	// The job that has just finished is never deleted by its own retention
	// pass, even when it alone is over the record cap.
	s.deleteJobLocked(big)
	earlier := addFinished(1)
	huge := addFinished(maxTaskRecords + 1)
	s.enforceRetentionLocked(huge)
	if s.jobs[huge.ID] == nil || s.jobs[earlier.ID] != nil {
		t.Fatalf("just-finished job deleted: huge kept %v, earlier kept %v", s.jobs[huge.ID] != nil, s.jobs[earlier.ID] != nil)
	}
}

// A job submitted before 501 short ones that finishes after them keeps its
// results: retention deletes the jobs that finished first, not the oldest
// submitted, and never the job that has just finished.
func TestRetentionKeepsLongRunningJob(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	long := h.submit(scriptJob(1, nil))
	lt := n.claim(1)
	if len(lt) != 1 {
		t.Fatal("no task for the long job")
	}
	s := h.s
	var short []*job
	// Run the short jobs through the scheduler without HTTP (and without a
	// synchronous save per submit).
	err := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		nd := s.nodes[n.req.NodeID]
		for i := 0; i < maxFinishedJobs+1; i++ {
			j := &job{jobRecord: jobRecord{ID: fmt.Sprintf("jshort%04d", i), Seq: s.nextSeq, Spec: scriptJob(1, nil), CreatedAt: time.Now()}}
			proto.ApplyJobDefaults(&j.Spec)
			s.nextSeq++
			j.counts.Pending = 1
			s.jobs[j.ID] = j
			s.queue = append(s.queue, j)
			short = append(short, j)
			got := s.dispatchLocked(nd, n.req.Total, 1, time.Now())
			if len(got) != 1 || got[0].JobID != j.ID {
				return fmt.Errorf("short job %d: dispatched %+v", i, got)
			}
			if code, msg := s.applyReportLocked(nd, got[0].ID, proto.TaskReport{Lease: got[0].Lease, State: proto.TaskSucceeded}, time.Now()); code != http.StatusOK {
				return fmt.Errorf("short job %d: report %d %s", i, code, msg)
			}
		}
		if len(s.jobs) != maxFinishedJobs+1 || s.jobs[short[0].ID] != nil || s.jobs[long.ID] == nil {
			return fmt.Errorf("after the short jobs: %d jobs, first kept %v", len(s.jobs), s.jobs[short[0].ID] != nil)
		}
		return nil
	}()
	if err != nil {
		t.Fatal(err)
	}
	n.succeed(lt[0])
	if d := h.job(long.ID); d.State != proto.JobSucceeded {
		t.Fatalf("long job: %s", d.State)
	}
	s.mu.Lock()
	jobs, secondKept, done := len(s.jobs), s.jobs[short[1].ID] != nil, s.jobs[long.ID].DoneSeq
	s.mu.Unlock()
	if jobs != maxFinishedJobs || secondKept || done != maxFinishedJobs+2 {
		t.Fatalf("after the long job: %d jobs, second short job kept %v, done_seq %d", jobs, secondKept, done)
	}
	// DoneSeq and its counter survive a restart.
	h.stop()
	h2 := startHive(t, h.dir, nil)
	h2.s.mu.Lock()
	j, next := h2.s.jobs[long.ID], h2.s.nextDoneSeq
	h2.s.mu.Unlock()
	if j == nil || j.DoneSeq != done || next != done+1 {
		t.Fatalf("after restart: job %v, next done_seq %d", j != nil, next)
	}
}

// Free disk of zero on a node with a disk quota means full, not unlimited
// (Fits treats zero disk as unlimited, which only suits a Total).
func TestFullDiskIsNotUnlimited(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	n := h.newNode(func(r *proto.RegisterRequest) { r.Total = proto.Resources{Cores: 4, MemMB: 4096, DiskMB: 100} })
	n.register()
	n.heartbeat() // reports Free = Total, for the wait reason
	h.submit(scriptJob(1, func(s *proto.JobSpec) { s.Resources.DiskMB = 100 }))
	fill := n.claim(16)
	if len(fill) != 1 {
		t.Fatalf("fill: %d", len(fill))
	}
	h.submit(scriptJob(2, func(s *proto.JobSpec) { s.Resources.DiskMB = 50 }))
	if got := n.claim(16); len(got) != 0 {
		t.Fatalf("a node with a fully allocated disk got %d tasks", len(got))
	}
	// Give one task a record (dispatched elsewhere and preempted) to see
	// its wait reason.
	other := h.newNode(nil)
	other.register()
	tk := other.claim(1)
	if len(tk) != 1 || tk[0].Resources.DiskMB != 50 {
		t.Fatalf("other node: %+v", tk)
	}
	other.report(tk[0], proto.TaskReport{State: proto.TaskPreempted})
	h.mustAdmin("PATCH", "nodes/"+other.req.NodeID, proto.NodePatch{Drain: ptr(true)}, nil)
	if v := h.task(tk[0].ID); v.State != proto.TaskPending || v.WaitReason != "waiting for 50 MB disk" {
		t.Fatalf("wait reason: %s %q", v.State, v.WaitReason)
	}
	if got := n.claim(16); len(got) != 0 {
		t.Fatalf("full disk, second claim: %d tasks", len(got))
	}
	// Freeing the disk lets both 50 MB tasks in, and no more.
	n.succeed(fill[0])
	if got := n.claim(16); len(got) != 2 {
		t.Fatalf("after freeing the disk: %d tasks", len(got))
	}
}
