package hive

// Benchmarks of the hive's per-request and per-save costs at the retention
// limits (DESIGN 8.5: 500 finished jobs, 200k task records), with the
// state built by prefillHistory through the real dispatch and report code.
//
//	GOTOOLCHAIN=local go test -run '^$' -bench 'Load' -benchtime 5x ./internal/hive/
//	SAVIOR_BENCH_RECORDS=200000 ... (one size instead of 10k, 50k, 200k)

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// prefillHistory fills s with finished jobs holding records task records,
// the way a busy lab's hive looks at the retention limits: one big job (half
// the records, at most MaxTaskCount) and jobs-1 small ones. Every task ran
// through dispatchOneLocked and applyReportLocked on one of nodeIDs, 2 %
// after a failed first attempt, and has one output blob of outBytes. With
// writeBlobs the blob files are written too, so a restart keeps them.
func prefillHistory(tb testing.TB, s *Server, jobs, records int, nodeIDs []string, outBytes int, writeBlobs bool) {
	tb.Helper()
	jobs = max(min(jobs, records), 1)
	sizes := make([]int, jobs)
	sizes[0] = min(records, proto.MaxTaskCount)
	if jobs > 1 {
		sizes[0] = min(records/2, proto.MaxTaskCount)
		rest := records - sizes[0]
		for k := 1; k < jobs; k++ {
			sizes[k] = rest / (jobs - 1)
			if k <= rest%(jobs-1) {
				sizes[k]++
			}
		}
	}
	if len(nodeIDs) == 0 {
		nodeIDs = []string{loadNodeID(0)}
	}
	nodes := make([]*node, len(nodeIDs))
	for i, id := range nodeIDs {
		nodes[i] = newNode(nodeRecord{ID: id, Total: proto.Resources{Cores: 4, MemMB: 2048, DiskMB: 10000}})
	}
	if writeBlobs {
		for i := 0; i < 256; i++ {
			if err := os.MkdirAll(filepath.Join(s.blobs.dir, fmt.Sprintf("%02x", i)), 0o700); err != nil {
				tb.Fatal(err)
			}
		}
	}
	type blobFile struct {
		t    proto.Task
		path string
	}
	now := time.Now()
	for k, count := range sizes {
		if count <= 0 {
			continue
		}
		spec := loadJobSpec(fmt.Sprintf("history-%03d", k), count)
		proto.ApplyJobDefaults(&spec)
		var files []blobFile
		var fail string
		s.mu.Lock()
		wall := s.now()
		j := &job{jobRecord: jobRecord{ID: "j" + auth.NewID(8), Seq: s.nextSeq, Spec: spec, CreatedAt: wall}}
		s.nextSeq++
		j.counts.Pending = count
		s.jobs[j.ID] = j
		s.queue = append(s.queue, j)
		for i := 0; i < count && fail == ""; i++ {
			n := nodes[(k*7+i)%len(nodes)]
			t := s.newTaskLocked(j)
			s.dispatchOneLocked(n, t, now)
			s.markRunningLocked(t)
			if i%50 == 7 {
				if code, msg := s.applyReportLocked(n, t.ID, proto.TaskReport{Lease: t.Lease, State: proto.TaskFailed, ErrorKind: proto.ErrExit,
					ExitCode: 1, Error: "exit status 1", RunS: 42, CPUSeconds: 40}, now); code != http.StatusOK {
					fail = fmt.Sprintf("failed report: %d %s", code, msg)
					break
				}
				n = nodes[(k*7+i+1)%len(nodes)]
				s.removeRequeuedLocked(t)
				s.dispatchOneLocked(n, t, now)
				s.markRunningLocked(t)
			}
			msg := proto.Task{ID: t.ID, JobID: j.ID, Index: t.Index, Attempt: t.Attempt}
			data := loadOutput(msg, outBytes)
			sum := sha(data)
			s.blobMeta[sum] = &blob{blobRecord: blobRecord{SHA256: sum, Size: int64(len(data)), CreatedAt: wall, LastTouched: wall, NodeUpload: true},
				touchedMono: now, uploadedMono: now}
			if writeBlobs {
				files = append(files, blobFile{msg, s.blobs.path(sum)})
			}
			rep := proto.TaskReport{Lease: t.Lease, State: proto.TaskSucceeded, RunS: 30 + float64(i%60), CPUSeconds: 27 + float64(i%60),
				MaxMemMB: 37 + i%50, Outputs: []proto.Output{{Name: "result.json", Blob: sum, Size: int64(len(data))}}}
			if code, msg := s.applyReportLocked(n, t.ID, rep, now); code != http.StatusOK {
				fail = fmt.Sprintf("report: %d %s", code, msg)
			}
		}
		s.mu.Unlock()
		if fail != "" {
			tb.Fatal(fail)
		}
		if len(files) > 0 {
			var wg sync.WaitGroup
			ch := make(chan blobFile, 256)
			errc := make(chan error, 8)
			for w := 0; w < 8; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for f := range ch {
						if err := os.WriteFile(f.path, loadOutput(f.t, outBytes), 0o600); err != nil {
							select {
							case errc <- err:
							default:
							}
						}
					}
				}()
			}
			for _, f := range files {
				ch <- f
			}
			close(ch)
			wg.Wait()
			select {
			case err := <-errc:
				tb.Fatal(err)
			default:
			}
		}
	}
}

func benchSizes() []int {
	if v := os.Getenv("SAVIOR_BENCH_RECORDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return []int{n}
		}
	}
	return []int{10000, 50000, 200000}
}

// benchNode is a node registered directly in a bench hive.
type benchNode struct {
	id, token string
	total     proto.Resources
	running   []proto.RunningTask
}

// benchHive is a hive (no listener) with records finished task records in
// min(records/400, 499) jobs, plus nodes online compute nodes that each
// hold busy running tasks of one more job.
func benchHive(b *testing.B, records, nodes, busy int) (*Server, []*benchNode) {
	b.Helper()
	s, err := New(loadHiveConfig(b.TempDir(), quietLog(), nil))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	ids := make([]string, max(nodes, 1))
	for i := range ids {
		ids[i] = loadNodeID(i)
	}
	if records > 0 {
		prefillHistory(b, s, min(max(records/400, 1), 499), records, ids, 2048, false)
	}
	var out []*benchNode
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for i := 0; i < nodes; i++ {
		tok := auth.NewToken()
		n := newNode(nodeRecord{ID: ids[i], Name: fmt.Sprintf("lab-pc-%03d", i), Roles: []proto.Role{proto.RoleCompute}, Approved: true,
			Sandbox: "strict", SandboxCaps: fullIsolationCaps, Total: proto.Resources{Cores: 4, MemMB: 2048, DiskMB: 10000},
			Inventory: proto.Inventory{Arch: "amd64", Cores: 4, MemTotalMB: 3072, CPUFlags: []string{"sse2", "lm"}}})
		n.tokenHash = auth.HashToken(tok)
		n.lastHB, n.online = now, true
		s.nodes[n.ID] = n
		s.tokens[n.tokenHash] = n.ID
		out = append(out, &benchNode{id: n.ID, token: tok, total: n.Total})
	}
	if busy > 0 && nodes > 0 {
		spec := loadJobSpec("busy", nodes*busy)
		proto.ApplyJobDefaults(&spec)
		j := &job{jobRecord: jobRecord{ID: "j" + auth.NewID(8), Seq: s.nextSeq, Spec: spec, CreatedAt: s.now()}}
		s.nextSeq++
		j.counts.Pending = spec.Count
		s.jobs[j.ID] = j
		s.queue = append(s.queue, j)
		for _, bn := range out {
			n := s.nodes[bn.id]
			for _, t := range s.dispatchLocked(n, n.Total, busy, now) {
				bn.running = append(bn.running, proto.RunningTask{ID: t.ID, Lease: t.Lease, Phase: proto.PhaseRunning, RunS: 3})
			}
		}
	}
	return s, out
}

func mustJSON(tb testing.TB, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// BenchmarkLoadPersist: one full state save (snapshot under the mutex, JSON
// encode, write, fsync, rename) with records task records retained.
func BenchmarkLoadPersist(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			s, _ := benchHive(b, n, 0, 0)
			var size int64
			var hold time.Duration
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.mu.Lock()
				t0 := time.Now()
				_ = s.snapshotLocked()
				hold += time.Since(t0)
				s.mu.Unlock()
				if err := s.persist(true); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if fi, err := os.Stat(filepath.Join(s.DataDir(), stateFile)); err == nil {
				size = fi.Size()
			}
			b.ReportMetric(float64(size)/(1<<20), "MB-state.json")
			b.ReportMetric(float64(hold.Microseconds())/1000/float64(b.N), "ms-lock-held")
		})
	}
}

// BenchmarkLoadBlobRefs: the reference check every blob PUT (each task
// output upload) makes under the state mutex to answer
// BlobInfo.Referenced. It used to walk every retained output.
func BenchmarkLoadBlobRefs(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			s, _ := benchHive(b, n, 0, 0)
			s.mu.Lock()
			if len(s.blobRefs) < n {
				b.Fatalf("%d counted references", len(s.blobRefs))
			}
			var sha string
			for k := range s.blobRefs {
				sha = k
				break
			}
			s.mu.Unlock()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.mu.Lock()
				ok := s.blobReferencedLocked(sha)
				s.mu.Unlock()
				if !ok {
					b.Fatal("an output blob is not referenced")
				}
			}
		})
	}
}

// BenchmarkLoadRequests: per-request hive cost of the node and admin
// endpoints (handler only, no TLS), with records retained and 500 nodes
// holding 4 running tasks each.
func BenchmarkLoadRequests(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			s, tns := benchHive(b, n, 500, 4)
			handler := s.Handler()
			// The big finished job of the history, for the job views.
			var bigJob string
			s.mu.Lock()
			for _, j := range s.jobs {
				if bigJob == "" || j.Spec.Count > s.jobs[bigJob].Spec.Count {
					bigJob = j.ID
				}
			}
			s.mu.Unlock()
			tn := tns[0]
			hb := proto.HeartbeatRequest{Status: proto.NodeStatus{State: proto.NodeBusy, Total: tn.total, RunningTasks: tn.running}}
			hbBody := mustJSON(b, hb)
			put := loadOutput(proto.Task{ID: "tbench", JobID: "jbench", Index: 1, Attempt: 1}, 2048)
			putSum := sha(put)
			cases := []struct {
				name, method, path, tok string
				body                    []byte
			}{
				{"heartbeat", "POST", "/api/v1/heartbeat", tn.token, hbBody},
				{"claim-nothing-fits", "POST", "/api/v1/claim", tn.token, mustJSON(b, proto.ClaimRequest{ClaimID: "x", Free: proto.Resources{Cores: 4, MemMB: 2048, DiskMB: 10000}, Max: 4, WaitS: 0})},
				{"blob-put-existing", "PUT", "/api/v1/blobs/" + putSum, tn.token, put},
				{"admin-nodes", "GET", "/api/v1/admin/nodes", testAdmin, nil},
				{"admin-jobs-50", "GET", "/api/v1/admin/jobs?limit=50", testAdmin, nil},
				{"admin-stats", "GET", "/api/v1/stats", testAdmin, nil},
				{"admin-job", "GET", "/api/v1/admin/jobs/" + bigJob, testAdmin, nil},
				{"admin-tasks-p1", "GET", "/api/v1/admin/jobs/" + bigJob + "/tasks?offset=0&limit=100", testAdmin, nil},
				{"admin-tasks-deep-filter", "GET", "/api/v1/admin/jobs/" + bigJob + "/tasks?state=succeeded&offset=50000&limit=100", testAdmin, nil},
				{"admin-outputs", "GET", "/api/v1/admin/jobs/" + bigJob + "/outputs", testAdmin, nil},
				{"admin-blobs", "GET", "/api/v1/admin/blobs", testAdmin, nil},
			}
			for _, c := range cases {
				b.Run(c.name, func(b *testing.B) {
					var size int
					for i := 0; i < b.N; i++ {
						req := httptest.NewRequest(c.method, "https://hive"+c.path, bytes.NewReader(c.body))
						req.Header.Set("Authorization", "Bearer "+c.tok)
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, req)
						if rec.Code != 200 {
							b.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body.String())
						}
						size = rec.Body.Len()
					}
					b.ReportMetric(float64(size), "resp-bytes")
				})
			}
		})
	}
}

// BenchmarkLoadBlobGC: the automatic blob GC (every 5 min) after a big
// finished job was deleted by retention. One op = the part of one GC pass
// that holds the state mutex (gcLocked); the files are removed afterwards,
// outside it (unlinkBlobs, not timed).
func BenchmarkLoadBlobGC(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("outputs=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s, _ := benchHive(b, 0, 0, 0)
				prefillHistory(b, s, 1, n, nil, 2048, true)
				s.mu.Lock()
				for _, j := range s.jobs {
					s.deleteJobLocked(j)
				}
				old := time.Now().Add(-2 * time.Hour)
				for _, bl := range s.blobMeta {
					bl.touchedMono, bl.uploadedMono = old, old
				}
				s.mu.Unlock()
				s.io.flush()
				b.StartTimer()
				s.mu.Lock()
				victims, _ := s.gcLocked(true, time.Now())
				s.mu.Unlock()
				b.StopTimer()
				if len(victims) != n {
					b.Fatalf("gc deleted %d of %d", len(victims), n)
				}
				s.unlinkBlobs(victims)
				s.Close()
				b.StartTimer()
			}
		})
	}
}
