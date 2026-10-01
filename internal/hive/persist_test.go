package hive

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func TestPersistenceRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := startHive(t, dir, nil)
	n := h.newNode(func(r *proto.RegisterRequest) { r.Name = "desk" })
	n.register()
	img := h.putBlob(testPNG(t, 4, 4))
	h.mustAdmin("PATCH", "nodes/desk", proto.NodePatch{Labels: &map[string]string{"gpu": "none"}, DisplayRotate: ptr(90),
		Display: &proto.DisplaySpec{Mode: proto.DisplayImage, Image: &proto.Media{Blob: img}}}, nil)
	d := h.submit(scriptJob(3, func(s *proto.JobSpec) { s.Name = "render" }))
	tasks := n.claim(2)
	n.succeed(tasks[0])
	other := h.newNode(func(r *proto.RegisterRequest) { r.Name = "wall-a" })
	other.register()
	var wall proto.WallSpec
	h.mustAdmin("POST", "walls", proto.WallSpec{Name: "lobby", Rows: 1, Cols: 1, Cells: []proto.WallCell{{Node: "wall-a"}},
		Content: proto.DisplaySpec{Mode: proto.DisplayTest}}, &wall)
	fp, hiveID := h.s.Fingerprint(), h.s.HiveID()
	h.stop()

	h2 := startHive(t, dir, nil)
	if h2.s.Fingerprint() != fp || h2.s.HiveID() != hiveID {
		t.Fatal("identity changed across restart")
	}
	v := h2.nodeView("desk")
	if v.Liveness != proto.NodeOffline || v.AdminLabels["gpu"] != "none" || v.DisplayRotate != 90 ||
		v.Display == nil || v.Display.Image.Blob != img || v.Display.Image.Width != 4 {
		t.Fatalf("node after restart: %+v", v)
	}
	j := h2.job(d.ID)
	if j.Name != "render" || j.Counts.Succeeded != 1 || j.Counts.Assigned != 1 || j.Counts.Pending != 1 || j.State != proto.JobRunning {
		t.Fatalf("job after restart: %+v", j)
	}
	var w2 proto.WallSpec
	h2.mustAdmin("GET", "walls/"+wall.ID, nil, &w2)
	if w2.Name != "lobby" || h2.nodeView("wall-a").WallID != wall.ID {
		t.Fatalf("wall after restart: %+v", w2)
	}
	var blobs []proto.BlobInfo
	h2.mustAdmin("GET", "blobs", nil, &blobs)
	if len(blobs) != 1 || blobs[0].SHA256 != img || !blobs[0].Referenced {
		t.Fatalf("blobs after restart: %+v", blobs)
	}
	// Node tokens live only in memory.
	n.h = h2
	if code, _ := n.api("POST", "heartbeat", proto.HeartbeatRequest{}, nil); code != http.StatusUnauthorized {
		t.Fatalf("old token after restart: %d", code)
	}
	// New jobs continue the sequence.
	if d2 := h2.submit(scriptJob(1, nil)); d2.Seq <= d.Seq {
		t.Fatalf("seq went backwards: %d <= %d", d2.Seq, d.Seq)
	}
}

func TestJobSubmitPersistedSynchronously(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.tune.minPersist = time.Hour })
	d := h.submit(scriptJob(1, nil))
	data, err := os.ReadFile(filepath.Join(h.dir, "state.json"))
	if err != nil || !bytes.Contains(data, []byte(d.ID)) {
		t.Fatalf("job not persisted before the response: %v", err)
	}
	fi, _ := os.Stat(filepath.Join(h.dir, "state.json"))
	if posixModes() && fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json mode %v", fi.Mode())
	}
	// Liveness-only changes wait for the liveness interval.
	n := h.newNode(nil)
	n.register() // structural: node record
	h.s.persist(false)
	before, _ := os.Stat(filepath.Join(h.dir, "state.json"))
	n.heartbeat()
	time.Sleep(100 * time.Millisecond)
	after, _ := os.Stat(filepath.Join(h.dir, "state.json"))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("heartbeat caused an immediate state write")
	}
}

// An acknowledged cancel or delete is on disk before the answer (DESIGN
// 9): a hive that loses power right after must not come back running the
// canceled job (chaos soak: a crash undid the cancel and the restored hive
// re-adopted and finished the job's tasks) or showing the deleted one.
func TestJobCancelAndDeletePersistedSynchronously(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.tune.minPersist = time.Hour })
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(2, nil))
	if got := n.claim(1); len(got) != 1 {
		t.Fatalf("claimed %d tasks", len(got))
	}
	saved := func() *jobSnapshot {
		t.Helper()
		snap, err := readSnapshot(filepath.Join(h.dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		for i := range snap.Jobs {
			if snap.Jobs[i].ID == d.ID {
				return &snap.Jobs[i]
			}
		}
		return nil
	}
	if js := saved(); js == nil || js.Canceled {
		t.Fatalf("before the cancel: saved job %+v", js)
	}
	h.mustAdmin("POST", "jobs/"+d.ID+"/cancel", nil, nil)
	if js := saved(); js == nil || !js.Canceled {
		t.Fatal("the cancel was answered before it was saved")
	}
	h.mustAdmin("DELETE", "jobs/"+d.ID, nil, nil)
	if js := saved(); js != nil {
		t.Fatal("the delete was answered before it was saved")
	}
}

func TestPrevFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := startHive(t, dir, nil)
	d1 := h.submit(scriptJob(1, nil))
	d2 := h.submit(scriptJob(1, nil)) // second write: state.json.prev now has d1
	h.stop()
	if _, err := os.Stat(filepath.Join(dir, "state.json.prev")); err != nil {
		t.Fatal("no .prev kept")
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	h2 := startHive(t, dir, nil)
	if st := h2.admin("GET", "jobs/"+d1.ID, nil, nil); st != 200 {
		t.Fatalf("job from .prev: %d", st)
	}
	_ = d2
	info := h2.s.Info()
	found := false
	for _, w := range info.Warnings {
		found = found || bytes.Contains([]byte(w), []byte("state.json.prev"))
	}
	if !found {
		t.Fatalf("no warning about the fallback: %v", info.Warnings)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "state.json.corrupt-*"))
	if len(matches) != 1 {
		t.Fatalf("corrupt file not kept aside: %v", matches)
	}
}

func TestRestartRecoveryAndReadoption(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := startHive(t, dir, nil)
	n1 := h.newNode(nil)
	n1.register()
	n2 := h.newNode(nil)
	n2.register()
	h.submit(scriptJob(4, nil))
	t12 := n1.claim(2)
	t3 := n2.claim(1)
	n1.heartbeat(running(t12...)...)
	n2.heartbeat(running(t3...)...)
	h.stop()

	h2 := startHive(t, dir, func(c *Config) { c.RecoveryWindow = 1500 * time.Millisecond })
	n1.h, n2.h = h2, h2
	// n1 comes back with t1 (right lease), t2 (wrong lease) and a task the
	// hive never heard of.
	n1.req.RunningTasks = []proto.RunningTask{
		{ID: t12[0].ID, Lease: t12[0].Lease, Phase: proto.PhaseRunning, RunS: 5},
		{ID: t12[1].ID, Lease: "not-the-lease", Phase: proto.PhaseRunning},
		{ID: "tunknown", Lease: "x", Phase: proto.PhaseRunning},
	}
	resp := n1.register()
	if len(resp.AdoptedTasks) != 1 || resp.AdoptedTasks[0] != t12[0].ID {
		t.Fatalf("adopted: %v", resp.AdoptedTasks)
	}
	if v := h2.task(t12[1].ID); v.State != proto.TaskPending {
		t.Fatalf("mismatched lease must be requeued: %s", v.State)
	}
	if hb := n1.heartbeat(running(t12[0])...); len(hb.Directives.CancelTasks) != 0 {
		t.Fatalf("adopted task canceled: %v", hb.Directives.CancelTasks)
	}
	// During the window n2's restored task is not redispatched.
	for _, tk := range n1.claim(4) {
		if tk.ID == t3[0].ID {
			t.Fatal("unconfirmed task redispatched during the recovery window")
		}
	}
	if v := h2.task(t3[0].ID); v.State != proto.TaskAssigned && v.State != proto.TaskRunning {
		t.Fatalf("restored task state: %s", v.State)
	}
	eventually(t, "recovery window end", func() bool { return h2.task(t3[0].ID).State == proto.TaskPending })
	if v := h2.task(t3[0].ID); v.History[len(v.History)-1].Outcome != "lost" {
		t.Fatalf("history: %+v", v.History)
	}
	// The adopted task finishes normally with its old lease.
	n1.succeed(t12[0])
	// A late n2 is told to drop its stale copy.
	n2.req.RunningTasks = running(t3...)
	if resp := n2.register(); len(resp.AdoptedTasks) != 0 {
		t.Fatalf("stale task adopted: %v", resp.AdoptedTasks)
	}
}

// A hive crash lost a dispatch: the restored task is pending again with
// the attempt number it had, while the node still tears down the
// assignment the hive forgot. The hive must not give the task back to that
// node while it lists the old lease (DESIGN 8.2): the same attempt would
// collide with the old run's <task_id>.<attempt> workdir (chaos soak:
// "workdir: mkdirat <task>.2: file exists", an internal node error).
func TestCrashLostDispatchNotReissuedToListingNode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := startHive(t, dir, func(c *Config) { c.tune.minPersist = time.Hour })
	n := h.newNode(nil)
	n.register()
	h.submit(scriptJob(1, nil)) // retries 1 (default)
	first := n.claim(1)
	if len(first) != 1 {
		t.Fatalf("claimed %d", len(first))
	}
	if code := n.report(first[0], proto.TaskReport{State: proto.TaskFailed, ErrorKind: proto.ErrExit, ExitCode: 3, RunS: 20}); code != 200 {
		t.Fatalf("report: %d", code)
	}
	if err := h.s.persist(true); err != nil { // the requeue is saved
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	saved, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	second := n.claim(1) // attempt 2, never saved
	if len(second) != 1 || second[0].ID != first[0].ID || second[0].Attempt != 2 {
		t.Fatalf("second claim: %+v", second)
	}
	h.stop()
	// The machine lost power: the hive comes back from the earlier file.
	if err := os.WriteFile(statePath, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	h2 := startHive(t, dir, nil)
	n.h = h2
	n.req.RunningTasks = running(second...)
	if resp := n.register(); len(resp.AdoptedTasks) != 0 {
		t.Fatalf("adopted a lease the hive never saved: %v", resp.AdoptedTasks)
	}
	if v := h2.task(second[0].ID); v.State != proto.TaskPending || v.Attempt != 1 {
		t.Fatalf("restored task: %s attempt %d", v.State, v.Attempt)
	}
	for i := 0; i < 2; i++ {
		if got := n.claim(1); len(got) != 0 {
			t.Fatalf("task %s attempt %d given to the node that still runs attempt %d of it (lease %s)",
				got[0].ID, got[0].Attempt, second[0].Attempt, second[0].Lease)
		}
		if a := h2.nodeView(n.req.NodeID).Allocated; a.Cores != 1 {
			t.Fatalf("the old run still uses the node, allocated %+v", a)
		}
		hb := n.heartbeat(running(second...)...) // the node is still killing it
		if len(hb.Directives.CancelTasks) != 1 || hb.Directives.CancelTasks[0].Lease != second[0].Lease {
			t.Fatalf("cancel directives: %+v", hb.Directives.CancelTasks)
		}
	}
	// Once the node stops listing it, the task may run there again.
	n.heartbeat()
	if a := h2.nodeView(n.req.NodeID).Allocated; a != (proto.Resources{}) {
		t.Fatalf("allocated after the node let go: %+v", a)
	}
	got := n.claim(1)
	if len(got) != 1 || got[0].ID != second[0].ID || got[0].Attempt != 2 || got[0].Lease == second[0].Lease {
		t.Fatalf("claim after the node let go: %+v", got)
	}
}

// Liveness (last_seen, metrics, address) goes to live.json, never to
// state.json: with the retained history at its limits a heartbeat-driven
// rewrite of state.json every minute was 172 MB a minute on the hive's USB
// stick (load harness). After a crash the hive gets it back from live.json.
func TestLivenessPersistedLazily(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := startHive(t, dir, func(c *Config) { c.tune.livenessPersist = 50 * time.Millisecond })
	n := h.newNode(nil)
	n.register()
	h.s.persist(false)
	statePath := filepath.Join(dir, stateFile)
	saved, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(statePath)
	n.heartbeatStatus(proto.NodeStatus{State: proto.NodeIdle, Metrics: proto.Metrics{Load1: 3.25}})
	eventually(t, "liveness write", func() bool {
		data, _ := os.ReadFile(filepath.Join(dir, liveFile))
		return bytes.Contains(data, []byte(`"load1":3.25`))
	})
	time.Sleep(100 * time.Millisecond)
	if after, _ := os.Stat(statePath); !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("a heartbeat rewrote state.json")
	}
	// Power cut: state.json is still the one without the metrics.
	h.stop()
	if err := os.WriteFile(statePath, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	h2 := startHive(t, dir, nil)
	if v := h2.nodeView(n.req.NodeID); v.Status.Metrics.Load1 != 3.25 || v.LastSeen.IsZero() {
		t.Fatalf("liveness after the restart: %+v, last seen %s", v.Status.Metrics, v.LastSeen)
	}
}

func TestLoadSanitizesDamagedState(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	s := h.s
	snap := &snapshot{
		snapshotHeader: snapshotHeader{Format: snapshotFormat, NextSeq: 1},
		Nodes: []nodeRecord{
			{ID: "nvalid000001", Name: "dup", WallID: "wmissing"},
			{ID: "nvalid000002", Name: "dup"},
			{ID: "BAD ID", Name: "x"},
		},
		Walls: []proto.WallSpec{{ID: "wbad", Rows: 0}},
		Jobs: []jobSnapshot{{
			jobRecord: jobRecord{ID: "j1", Seq: 7, Spec: proto.JobSpec{Count: 2, Script: "true"}, NextIndex: 5},
			Tasks: []taskRecord{
				{ID: "t1", Index: 0, State: proto.TaskRunning, Node: "ngone", Lease: "l"},
				{ID: "t2", Index: 9, State: proto.TaskPending},
				{ID: "t3", Index: 1, State: "weird"},
			},
		}},
	}
	s.mu.Lock()
	s.nodes, s.walls, s.jobs, s.tasks, s.queue, s.taskRecords = map[string]*node{}, map[string]*proto.WallSpec{}, map[string]*job{}, map[string]*task{}, nil, 0
	s.applySnapshotLocked(snap)
	defer s.mu.Unlock()
	if len(s.nodes) != 2 || s.nodes["nvalid000001"].WallID != "" || s.nodes["nvalid000002"].Name == "dup" {
		t.Fatalf("nodes: %+v", s.nodes)
	}
	if len(s.walls) != 0 {
		t.Fatal("invalid wall loaded")
	}
	j := s.jobs["j1"]
	if j == nil || j.NextIndex != 2 || len(j.tasks) != 2 || s.nextSeq != 8 {
		t.Fatalf("job: %+v", j)
	}
	// The orphaned running task was requeued; the unknown state became pending.
	if s.tasks["t1"].State != proto.TaskPending || s.tasks["t3"].State != proto.TaskPending || j.counts.Pending != 2 {
		t.Fatalf("tasks: %s %s %+v", s.tasks["t1"].State, s.tasks["t3"].State, j.counts)
	}
}

// A job that has finished for good is written once, to jobs/<id>.json, and
// state.json leaves it out from then on: with 200k retained records every
// save rewrote 172 MB of finished jobs and held the state mutex while it
// copied them (load harness). A restart loads it back with its outputs
// counted as blob references; deleting the job removes its file before
// the answer.
func TestSettledJobsSavedOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := startHive(t, dir, func(c *Config) { c.tune.minPersist = time.Hour })
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(2, nil))
	tasks := n.claim(2)
	out := []byte("frame 0")
	if code, _ := n.api("PUT", "blobs/"+sha(out), out, nil); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	if code := n.report(tasks[0], proto.TaskReport{State: proto.TaskSucceeded,
		Outputs: []proto.Output{{Name: "f.txt", Blob: sha(out), Size: int64(len(out))}}}); code != http.StatusOK {
		t.Fatalf("report: %d", code)
	}
	n.succeed(tasks[1])
	if err := h.s.persist(false); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, jobsDir, d.ID+".json")
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatalf("settled job not saved to its own file: %v", err)
	}
	statePath := filepath.Join(dir, stateFile)
	if data, _ := os.ReadFile(statePath); bytes.Contains(data, []byte(d.ID)) || bytes.Contains(data, []byte(tasks[0].ID)) {
		t.Fatal("state.json still holds the settled job")
	}
	h.s.mu.Lock()
	snap := h.s.snapshotLocked()
	h.s.mu.Unlock()
	if len(snap.Jobs) != 0 {
		t.Fatalf("the snapshot taken under the state mutex copies %d settled jobs", len(snap.Jobs))
	}
	// Later saves leave the file alone.
	h.submit(scriptJob(1, nil))
	if fi2, err := os.Stat(file); err != nil || !fi2.ModTime().Equal(fi.ModTime()) || fi2.Size() != fi.Size() {
		t.Fatalf("the settled job's file was rewritten: %v", err)
	}
	h.stop()

	h2 := startHive(t, dir, nil)
	j := h2.job(d.ID)
	if j.State != proto.JobSucceeded || j.Counts.Succeeded != 2 {
		t.Fatalf("job after restart: %+v", j)
	}
	if v := h2.task(tasks[0].ID); len(v.Outputs) != 1 || v.Outputs[0].Blob != sha(out) {
		t.Fatalf("task after restart: %+v", v)
	}
	var blobs []proto.BlobInfo
	h2.mustAdmin("GET", "blobs", nil, &blobs)
	if len(blobs) != 1 || !blobs[0].Referenced {
		t.Fatalf("output after restart: %+v", blobs)
	}
	h2.mustAdmin("DELETE", "jobs/"+d.ID, nil, nil)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the deleted job's file was still there when the delete was answered: %v", err)
	}
	h2.stop()
	h3 := startHive(t, dir, nil)
	if code := h3.admin("GET", "jobs/"+d.ID, nil, nil); code != http.StatusNotFound {
		t.Fatalf("deleted job after restart: %d", code)
	}
}

// A job settles only when no task of it is still stopping: a canceled
// job's file is written once its nodes have let go of its tasks; until
// then state.json keeps it.
func TestCanceledJobSavedOnceSettled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := startHive(t, dir, func(c *Config) { c.tune.minPersist = time.Hour })
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(2, nil))
	tasks := n.claim(2)
	n.heartbeat(running(tasks...)...)
	h.mustAdmin("POST", "jobs/"+d.ID+"/cancel", nil, nil)
	file := filepath.Join(dir, jobsDir, d.ID+".json")
	statePath := filepath.Join(dir, stateFile)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("a canceled job with tasks still stopping was saved as settled")
	}
	if data, _ := os.ReadFile(statePath); !bytes.Contains(data, []byte(d.ID)) {
		t.Fatal("the canceled job is not in state.json")
	}
	for _, tk := range tasks {
		if code := n.report(tk, proto.TaskReport{State: proto.TaskCanceled, RunS: 2, CPUSeconds: 1}); code != http.StatusOK {
			t.Fatalf("report: %d", code)
		}
	}
	if err := h.s.persist(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("settled canceled job: %v", err)
	}
	if data, _ := os.ReadFile(statePath); bytes.Contains(data, []byte(d.ID)) {
		t.Fatal("state.json still holds the settled job")
	}
	h.stop()
	h2 := startHive(t, dir, nil)
	if j := h2.job(d.ID); j.State != proto.JobCanceled || j.Counts.Canceled != 2 {
		t.Fatalf("job after restart: %+v", j)
	}
}

// A settled job's file is on disk before state.json drops the job. After a
// power cut that kept an older state.json, where the job still runs, the
// file wins: the job is not restored as running, nor its task held on the
// node.
func TestSettledJobFileWinsAfterCrash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := startHive(t, dir, func(c *Config) { c.tune.minPersist = time.Hour })
	n := h.newNode(nil)
	n.register()
	d := h.submit(scriptJob(1, nil))
	tk := n.claim(1)[0]
	if err := h.s.persist(true); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, stateFile)
	older, err := os.ReadFile(statePath)
	if err != nil || !bytes.Contains(older, []byte(tk.Lease)) {
		t.Fatalf("assignment not saved: %v", err)
	}
	n.succeed(tk)
	if err := h.s.persist(false); err != nil {
		t.Fatal(err)
	}
	h.stop()
	if err := os.WriteFile(statePath, older, 0o600); err != nil {
		t.Fatal(err)
	}
	h2 := startHive(t, dir, nil)
	if j := h2.job(d.ID); j.State != proto.JobSucceeded || j.Counts.Succeeded != 1 {
		t.Fatalf("job after the crash: %+v", j)
	}
	h2.s.mu.Lock()
	defer h2.s.mu.Unlock()
	for _, nd := range h2.s.nodes {
		if _, ok := nd.held[tk.ID]; ok {
			t.Fatal("the finished task is held on its node again")
		}
	}
	if !h2.s.jobs[d.ID].archived {
		t.Fatal("the job was not loaded from its file")
	}
}

// writeSnapshot encodes record by record. Encoding the whole document in
// one Encode call allocated 4.6x the state's size per save and left a
// buffer of that size in encoding/json's pool (load harness: 320 of 414 MB
// of live heap after a 100k-task run were such buffers).
func TestStateSaveStreams(t *testing.T) {
	// Not parallel: it measures the process's allocations.
	dir := t.TempDir()
	const n = 30000
	now := time.Now()
	exit := 0
	js := jobSnapshot{jobRecord: jobRecord{ID: "jbig", Seq: 1, Spec: proto.JobSpec{Count: n + 1, Script: "true"}, CreatedAt: now, NextIndex: n}}
	for i := 0; i < n; i++ {
		js.Tasks = append(js.Tasks, taskRecord{ID: fmt.Sprintf("t%015x", i), JobID: "jbig", Index: i, State: proto.TaskSucceeded,
			Node: "n0123456789ab", Attempt: 1, ExitCode: &exit, AssignedAt: &now, StartedAt: &now, FinishedAt: &now, RunS: 30, CPUSeconds: 27,
			Outputs: []proto.Output{{Name: "result.json", Blob: strings.Repeat("ab", 32), Size: 2048}},
			History: []proto.AttemptView{{Attempt: 1, Node: "n0123456789ab", AssignedAt: now, FinishedAt: now, Outcome: "succeeded"}}})
	}
	snap := &snapshot{snapshotHeader: snapshotHeader{Format: snapshotFormat, HiveID: "htest", SavedAt: now, NextSeq: 2},
		Jobs: []jobSnapshot{js}}
	runtime.GC()
	var m0, m1, m2 runtime.MemStats
	runtime.ReadMemStats(&m0)
	if err := writeSnapshot(dir, snap); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m1)
	runtime.GC()
	runtime.ReadMemStats(&m2)
	runtime.KeepAlive(snap)
	fi, err := os.Stat(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	alloc, kept := int64(m1.TotalAlloc-m0.TotalAlloc), int64(m2.HeapAlloc)-int64(m0.HeapAlloc)
	t.Logf("state.json %.1f MB: %.1f MB allocated, %.1f MB still on the heap after a GC", float64(fi.Size())/(1<<20),
		float64(alloc)/(1<<20), float64(kept)/(1<<20))
	if alloc > fi.Size() && !raceEnabled {
		t.Errorf("one save allocated %.1fx the size of state.json; want a streaming write (< 1x)", float64(alloc)/float64(fi.Size()))
	}
	if kept > fi.Size()/4 {
		t.Errorf("a save left %.1f MB on the heap (state.json %.1f MB)", float64(kept)/(1<<20), float64(fi.Size())/(1<<20))
	}
	back, err := readSnapshot(filepath.Join(dir, stateFile))
	if err != nil || back.HiveID != "htest" || len(back.Jobs) != 1 || len(back.Jobs[0].Tasks) != n ||
		back.Jobs[0].Tasks[n-1].Outputs[0].Size != 2048 || back.Jobs[0].NextIndex != n || back.NextSeq != 2 {
		t.Fatalf("read back: %v", err)
	}
}

// A state.json from before settled jobs had files of their own holds them
// inline; it loads as it is, and the next save moves them out. A damaged
// job file is kept aside with a warning and doesn't stop the hive.
func TestJobFilesFromOlderStateAndDamage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now().UTC()
	exit := 0
	old := &snapshot{snapshotHeader: snapshotHeader{Format: snapshotFormat, HiveID: "hold", SavedAt: now, NextSeq: 8, NextDoneSeq: 3},
		Jobs: []jobSnapshot{{
			jobRecord: jobRecord{ID: "jfinished", Seq: 7, Spec: proto.JobSpec{Count: 1, Script: "true"}, CreatedAt: now, NextIndex: 1,
				FinishedAt: &now, DoneSeq: 2},
			Tasks: []taskRecord{{ID: "tdone", JobID: "jfinished", State: proto.TaskSucceeded, Attempt: 1, ExitCode: &exit, FinishedAt: &now}},
		}}}
	if err := writeSnapshot(dir, old); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, jobsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobsDir, "jdamaged.json"), []byte(`{"format":1,"job":{"id":"jdam`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := startHive(t, dir, nil)
	if j := h.job("jfinished"); j.State != proto.JobSucceeded {
		t.Fatalf("job from the older state: %+v", j)
	}
	found := false
	for _, w := range h.s.Info().Warnings {
		found = found || strings.Contains(w, "jdamaged")
	}
	if !found {
		t.Fatalf("no warning about the damaged job file: %v", h.s.Info().Warnings)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, jobsDir, "jdamaged.json.corrupt-*")); len(m) != 1 {
		t.Fatalf("damaged job file not kept aside: %v", m)
	}
	if err := h.s.persist(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, jobsDir, "jfinished.json")); err != nil {
		t.Fatalf("the finished job was not moved to its file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, stateFile)); bytes.Contains(b, []byte("tdone")) {
		t.Fatal("state.json still holds the finished job")
	}
	h.stop()
	h2 := startHive(t, dir, nil)
	if j := h2.job("jfinished"); j.State != proto.JobSucceeded || j.Seq != 7 {
		t.Fatalf("job after the move: %+v", j)
	}
	if d := h2.submit(scriptJob(1, nil)); d.Seq != 8 {
		t.Fatalf("next seq %d, want 8", d.Seq)
	}
}
