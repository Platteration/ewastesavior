package hive

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
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
	if fi.Mode().Perm() != 0o600 {
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

func TestLivenessPersistedLazily(t *testing.T) {
	t.Parallel()
	h := newHive(t, func(c *Config) { c.tune.livenessPersist = 50 * time.Millisecond })
	n := h.newNode(nil)
	n.register()
	h.s.persist(false)
	n.heartbeatStatus(proto.NodeStatus{State: proto.NodeIdle, Metrics: proto.Metrics{Load1: 3.25}})
	eventually(t, "liveness write", func() bool {
		data, _ := os.ReadFile(filepath.Join(h.dir, "state.json"))
		return bytes.Contains(data, []byte(`"load1":3.25`))
	})
}

func TestLoadSanitizesDamagedState(t *testing.T) {
	t.Parallel()
	h := newHive(t, nil)
	s := h.s
	snap := &snapshot{
		Format:  snapshotFormat,
		NextSeq: 1,
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
