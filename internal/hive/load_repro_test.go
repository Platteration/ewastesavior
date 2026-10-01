package hive

// Regression tests for the scalability problems the load harness
// (load_test.go) found. Each states the property the hive must keep and ran
// in a few seconds; they run in every `go test`:
//
//	GOTOOLCHAIN=local go test -v -run '^TestScale' ./internal/hive/

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// putOutputCost is the median time of a node's output upload (PUT of a new
// 2 KiB blob, handled without TLS) on a hive that retains records task
// records with one output each. The handler holds the state mutex for
// almost all of it.
func putOutputCost(t *testing.T, records int) time.Duration {
	s, err := New(loadHiveConfig(t.TempDir(), quietLog(), nil))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if records > 0 {
		prefillHistory(t, s, max(records/400, 1), records, []string{loadNodeID(0)}, 2048, false)
	}
	// One online node holding one running task (uploads need one).
	tok := auth.NewToken()
	s.mu.Lock()
	n := newNode(nodeRecord{ID: loadNodeID(1), Name: "uploader", Roles: []proto.Role{proto.RoleCompute}, Approved: true,
		Sandbox: "strict", SandboxCaps: fullIsolationCaps, Total: proto.Resources{Cores: 4, MemMB: 2048, DiskMB: 10000},
		Inventory: proto.Inventory{Arch: "amd64", Cores: 4, MemTotalMB: 4096}})
	n.tokenHash = auth.HashToken(tok)
	n.lastHB, n.online = time.Now(), true
	s.nodes[n.ID], s.tokens[n.tokenHash] = n, n.ID
	spec := loadJobSpec("upload", 1)
	proto.ApplyJobDefaults(&spec)
	j := &job{jobRecord: jobRecord{ID: "j" + auth.NewID(8), Seq: s.nextSeq, Spec: spec, CreatedAt: s.now()}}
	s.nextSeq++
	j.counts.Pending = 1
	s.jobs[j.ID] = j
	s.queue = append(s.queue, j)
	if got := s.dispatchLocked(n, n.Total, 1, time.Now()); len(got) != 1 {
		s.mu.Unlock()
		t.Fatalf("dispatched %d tasks", len(got))
	}
	s.mu.Unlock()
	h := s.Handler()
	var d []time.Duration
	for i := 0; i < 7; i++ {
		data := loadOutput(proto.Task{ID: fmt.Sprintf("t%d", i), JobID: "jx", Index: i, Attempt: 1}, 2048)
		req := httptest.NewRequest("PUT", "https://hive/api/v1/blobs/"+sha(data), bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		t0 := time.Now()
		h.ServeHTTP(rec, req)
		d = append(d, time.Since(t0))
		if rec.Code != 200 {
			t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
		}
	}
	sort.Slice(d, func(a, b int) bool { return d[a] < d[b] })
	return d[len(d)/2]
}

// TestScaleOutputUploadIsO1: the cost of one output upload must not grow
// with the number of retained task records. Every blob PUT answers
// BlobInfo.Referenced under the state mutex; it used to walk every output
// of every retained task and format a string for each (21x the cost at 50k
// records than at 1k), which collapsed a 500-node lab's throughput from
// 255 to 35 tasks/s and let heartbeats wait past OfflineAfter.
func TestScaleOutputUploadIsO1(t *testing.T) {
	records := 50000
	if raceEnabled {
		records = 20000 // the old walk still cost 9x as much at 20k
	}
	small := putOutputCost(t, 1000)
	big := putOutputCost(t, records)
	t.Logf("output upload: %s with 1k retained records, %s with %d", small, big, records)
	if big > 3*small+2*time.Millisecond {
		t.Errorf("an output upload costs %.0fx more with %d retained records than with 1k (%s vs %s); want O(1)",
			float64(big)/float64(small), records, big, small)
	}
}

// TestScaleClaimResponseSkipsMutex: once a claim has dispatched tasks
// (they are assigned, and their MissingAfter clock runs), the response must
// reach the node without waiting for the state mutex again. handleClaim's
// deferred `node.claims--` used to take s.mu after writeJSON, and net/http
// sends a small body only when the handler returns: the node got its tasks
// only after one more mutex acquisition. Under a lock convoy (seconds per
// acquisition) that delay passed MissingAfter (20 s) and the hive requeued
// the tasks as lost ("task missing from the node's running tasks").
func TestScaleClaimResponseSkipsMutex(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS >= 2")
	}
	const hold = 2 * time.Second
	h := newHive(t, nil)
	n := h.newNode(nil)
	n.register()
	n.heartbeat()
	s := h.s
	for attempt := 0; attempt < 40; attempt++ {
		d := h.submit(scriptJob(1, nil))
		caught := make(chan bool, 1)
		stop := make(chan struct{})
		go func() {
			// Grab the mutex right after the claim dispatched, while the
			// handler has not yet run its deferred claims--.
			for {
				select {
				case <-stop:
					caught <- false
					return
				default:
				}
				if !s.mu.TryLock() {
					continue
				}
				j, nd := s.jobs[d.ID], s.nodes[n.req.NodeID]
				if j != nil && nd != nil && j.counts.Assigned == 1 {
					if nd.claims.Load() == 1 {
						time.Sleep(hold)
						s.mu.Unlock()
						caught <- true
						return
					}
					s.mu.Unlock()
					caught <- false
					return
				}
				s.mu.Unlock()
			}
		}()
		t0 := time.Now()
		tasks := n.claim(1)
		lat := time.Since(t0)
		close(stop)
		ok := <-caught
		if len(tasks) != 1 {
			t.Fatalf("claim got %d tasks", len(tasks))
		}
		n.heartbeat(running(tasks[0])...)
		n.succeed(tasks[0])
		if !ok {
			continue // missed the window between dispatch and claims--; try again
		}
		t.Logf("attempt %d: the claim dispatched its task, the mutex was then held %s, the response took %s", attempt, hold, lat)
		if lat >= hold {
			t.Errorf("the claim response waited for the state mutex after the task was dispatched (%s); a node gets its tasks only when the hive's mutex is free again", lat)
		}
		return
	}
	t.Skip("never caught the window between dispatch and the deferred claims-- (timing)")
}

// TestScaleLivenessPersistSize: a heartbeat-only change (last_seen,
// metrics) must not rewrite every retained task record. Heartbeats used to
// make maybePersist rewrite the whole state.json every 60 s, forever, while
// any node was online: with 200k retained records that was 172 MB a minute
// on the hive's USB stick. Liveness now goes to live.json.
func TestScaleLivenessPersistSize(t *testing.T) {
	h := newHive(t, func(c *Config) { c.tune.livenessPersist = 300 * time.Millisecond })
	prefillHistory(t, h.s, 20, 5000, []string{loadNodeID(0)}, 2048, false)
	if err := h.s.persist(true); err != nil {
		t.Fatal(err)
	}
	n := h.newNode(nil)
	n.register()
	n.heartbeat()
	time.Sleep(500 * time.Millisecond) // the registration's structural write
	path := filepath.Join(h.dir, stateFile)
	fi0, _ := os.Stat(path)
	var written int64
	last := fi0.ModTime()
	end := time.Now().Add(4 * time.Second)
	for time.Now().Before(end) {
		n.heartbeat()
		time.Sleep(50 * time.Millisecond)
		if fi, err := os.Stat(path); err == nil && !fi.ModTime().Equal(last) {
			last = fi.ModTime()
			written += fi.Size()
		}
	}
	t.Logf("state.json is %d bytes; heartbeats alone made the hive write %d bytes in 4 s", fi0.Size(), written)
	if written > 1<<20 {
		t.Errorf("heartbeat-only changes rewrote %d bytes of state (5k retained records) in 4 s; want liveness saved without rewriting task records", written)
	}
}

// TestScalePersistMemory: saving the state must not need several times its
// size in memory. writeSnapshot used to marshal the whole document into one
// buffer (json.Encoder.Encode: 4.6x the size of state.json allocated per
// save, buffers kept in encoding/json's pool afterwards), and every save
// rewrote every retained record. Now records are encoded one at a time, and
// a settled job is written once, to its own file.
func TestScalePersistMemory(t *testing.T) {
	cfg := loadHiveConfig(t.TempDir(), quietLog(), nil)
	cfg.tune.loopInterval = time.Hour // no background save in between
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	records := 50000
	if raceEnabled {
		records = 10000 // only the size checks apply
	}
	prefillHistory(t, s, 100, records, []string{loadNodeID(0)}, 2048, false)
	written := func() int64 {
		var n int64
		files, _ := filepath.Glob(filepath.Join(s.DataDir(), jobsDir, "*.json"))
		for _, f := range append(files, filepath.Join(s.DataDir(), stateFile)) {
			if fi, err := os.Stat(f); err == nil {
				n += fi.Size()
			}
		}
		return n
	}
	before := written()
	_, _, size, alloc, peak, base := measurePersist(t, s)
	first := written() - before
	t.Logf("first save: %.1f MB written (state.json %.1f MB, the rest the 100 settled jobs' files); %.1f MB allocated; heap %.1f MB before, peak %.1f MB",
		float64(first)/(1<<20), float64(size)/(1<<20), float64(alloc)/(1<<20), float64(base)/(1<<20), float64(peak)/(1<<20))
	if alloc > 2*uint64(first) && !raceEnabled {
		t.Errorf("one save allocated %.1fx the bytes it wrote; want a streaming write (< 2x)", float64(alloc)/float64(first))
	}
	_, _, size2, alloc2, _, _ := measurePersist(t, s)
	t.Logf("next save: state.json %.1f MB, %.1f MB allocated", float64(size2)/(1<<20), float64(alloc2)/(1<<20))
	if size2 > first/3 {
		t.Errorf("the next save wrote %.1f MB of %.1f MB: settled jobs are rewritten", float64(size2)/(1<<20), float64(first)/(1<<20))
	}
	if alloc2 > 2*uint64(size2) && !raceEnabled {
		t.Errorf("a save of state.json allocated %.1fx its size; want < 2x", float64(alloc2)/float64(size2))
	}
}

// TestScaleLogMemoryBounded: the hive's in-memory task logs must have a
// global budget. Each running task used to keep up to 1.25 MiB with no
// total cap (160 chatty tasks: 242.5 MiB), so a lab with 800 running
// chatty tasks needed 1 GB on the hive for logs alone.
func TestScaleLogMemoryBounded(t *testing.T) {
	tasks, budget := 160, 64<<20
	if raceEnabled {
		tasks, budget = 40, 16<<20 // 40 x 1.25 MiB was 50 MiB
	}
	h := newHive(t, func(c *Config) { c.tune.logBudget = int64(budget) })
	n := h.newNode(func(r *proto.RegisterRequest) {
		r.Inventory.Cores, r.Inventory.MemTotalMB = 256, 1<<20
		r.Total = proto.Resources{Cores: 256, MemMB: 1 << 19, DiskMB: 1 << 20}
	})
	n.register()
	n.heartbeat()
	h.submit(scriptJob(tasks, nil))
	var got []proto.Task
	for len(got) < tasks {
		ts := n.claim(16)
		if len(ts) == 0 {
			t.Fatalf("claimed only %d tasks", len(got))
		}
		got = append(got, ts...)
	}
	n.heartbeat(running(got...)...)
	chunk := bytes.Repeat([]byte("progress 42% frame 1234 of 9999 ok\n"), (256<<10)/36)
	for _, tk := range got {
		var off int64
		for k := 0; k < 5; k++ {
			path := fmt.Sprintf("tasks/%s/log?lease=%s&offset=%d", tk.ID, tk.Lease, off)
			if code, raw := n.api("POST", path, chunk, nil); code != http.StatusOK {
				t.Fatalf("log: %d %s", code, raw)
			}
			off += int64(len(chunk))
		}
	}
	h.s.mu.Lock()
	var held int
	for _, tk := range got {
		lr := h.s.tasks[tk.ID].log
		if lr == nil {
			continue
		}
		held += cap(lr.data)
		// Each ring still has the stream's latest bytes.
		if lr.total != 5*int64(len(chunk)) || len(lr.data) < minLogRing || !bytes.HasSuffix(chunk, lr.data[len(lr.data)-1000:]) {
			t.Errorf("task %s: ring holds %d bytes up to offset %d", tk.ID, len(lr.data), lr.total)
		}
	}
	accounted := h.s.logMem
	h.s.mu.Unlock()
	if accounted != int64(held) {
		t.Errorf("log memory accounted %d bytes, rings hold %d", accounted, held)
	}
	t.Logf("%d running tasks that each wrote 1.25 MiB hold %.1f MiB of log buffers on the hive", tasks, float64(held)/(1<<20))
	if held > budget {
		t.Errorf("log buffers of running tasks hold %.0f MiB; want them within the budget (%d MiB)", float64(held)/(1<<20), budget>>20)
	}
}
