package hive

// Load and scale harness for the hive (DESIGN 8-10).
//
// The hive runs in the test process with production timings (1 s loop,
// 2 s minimum persist gap, 60 s liveness persists, 5/20/60 s heartbeat,
// offline and lost timers) and serves real HTTPS through Server.Run. The
// simulated node agents and a simulated dashboard run in a child process
// (load_swarm_test.go), so this process's CPU time, heap and goroutines are
// the hive's own. While a job runs, the harness samples the heap
// (runtime.MemStats), goroutines, every state.json rewrite (size and write
// time), how long it takes to acquire the state mutex (a probe every 50 ms:
// what a heartbeat waits for), and counts "node offline" events.
//
// TestLoadSmall runs in every `go test` (8 nodes, 240 tasks, a few seconds)
// and checks the invariants. The big runs are opt-in:
//
//	SAVIOR_LOAD=1 GOTOOLCHAIN=local go test -v -timeout 0 -run '^TestLoad$' ./internal/hive/
//
// Environment (defaults in brackets):
//
//	SAVIOR_LOAD_SCENARIOS  NODESxTASKS list [50x1000,200x10000,500x100000]
//	SAVIOR_LOAD_PROCS      hive GOMAXPROCS list, 0 = all CPUs [0]
//	SAVIOR_LOAD_HISTORY    finished task records retained before each run [0]
//	SAVIOR_LOAD_HISTORY_JOBS  finished jobs holding them [499]
//	SAVIOR_LOAD_SLOTS      task slots per node [4]
//	SAVIOR_LOAD_TASK_S     mean task run time, uniform 0.5-1.5x [5]
//	SAVIOR_LOAD_LOG_BPS    log bytes/s per running task, sent every 2 s [256]
//	SAVIOR_LOAD_OUT_BYTES  output blob per task [2048]
//	SAVIOR_LOAD_HB_S       heartbeat interval [5]
//	SAVIOR_LOAD_DASH_S     dashboard poll interval, 0 = off [5]
//	SAVIOR_LOAD_IDLE_S     idle observation after the job [65]
//	SAVIOR_LOAD_TIMEOUT    per-run limit [30m]
//	SAVIOR_LOAD_DIR        parent of the hive data dirs (e.g. a USB stick) [temp]
//	SAVIOR_LOAD_HIVE_RAM_MB  apply main.go's soft memory limit for a hive
//	                       machine with this much RAM (MemTotal/4) [none]
//	SAVIOR_LOAD_PROFILE    write cpu/heap/mutex profiles of each run here
//	SAVIOR_LOAD_OUT        append the result tables to this file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

type loadScenario struct {
	Name     string
	Nodes    int
	Tasks    int
	Procs    int // hive GOMAXPROCS; 0 = all CPUs
	Slots    int
	TaskS    float64
	LogBPS   int
	OutBytes int
	HBS      float64
	DashS    float64
	History  int // finished task records in the hive before the run
	HistJobs int
	IdleS    float64
	Timeout  time.Duration
	BaseDir  string
	ProfDir  string
	RAMMB    int // emulate main.go's memory limit for this much RAM
}

// loadReport is what one run measured.
type loadReport struct {
	sc        loadScenario
	finished  bool
	submitLat time.Duration
	wall      time.Duration // submit -> job finished
	done      int
	rate      float64   // tasks/s over the whole job
	rateFirst float64   // first 10 % of the tasks
	rateLast  float64   // last 10 %
	deciles   []float64 // tasks/s in each successive 10 % of the tasks
	peakDisp  float64   // best 5 s window of dispatches/s
	ideal     float64   // nodes x slots / mean task time
	hiveCPU   float64   // s, during the job
	gcCPU     float64   // s of it spent in the garbage collector
	idleCPU   float64   // s per minute, after the job
	diskBytes int64     // written by the hive process during the job
	heapMax   uint64
	inuseMax  uint64
	sysMax    uint64
	heapEnd   uint64 // live heap after a GC, job done
	gorMax    int
	ioMax     int          // longest io queue (log tail writes, deletions)
	writes    persistStats // during the job
	idle      persistStats // after it
	lockP50   time.Duration
	lockP99   time.Duration
	lockMax   time.Duration
	offline   int            // "node offline" during the job
	lost      int            // attempts that ended "lost"
	lostWhy   map[string]int // ... by reason
	extra     int            // attempts beyond the first
	noOutput  int            // succeeded tasks without outputs
	records   int            // task records after the job (retention applied)
	blobs     int
	outputsN  int           // GET jobs/{id}/outputs
	outputsB  int           // its size
	outputsL  time.Duration // its latency
	blobListB int
	blobListL time.Duration
	shutdown  time.Duration // Run return after cancel (includes the final save)
	child     loadResult
	logs      map[string]int
	hist      *histStats
	failures  []string
}

type persistStats struct {
	n        int
	bytes    int64
	maxSize  int64
	maxDur   time.Duration
	totalDur time.Duration
	perMin   float64
}

type histStats struct {
	records   int
	prefill   time.Duration
	hold      time.Duration // snapshotLocked under the mutex
	persist   time.Duration // writeSnapshot
	size      int64
	alloc     uint64 // bytes allocated by one persist
	peak      uint64 // peak heap during it
	base      uint64 // live heap before it
	restart   time.Duration
	restPeak  uint64
	afterLoad uint64
}

// --- environment ---

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func loadScenariosFromEnv(t *testing.T) []loadScenario {
	list := os.Getenv("SAVIOR_LOAD_SCENARIOS")
	if list == "" {
		list = "50x1000,200x10000,500x100000"
	}
	procs := os.Getenv("SAVIOR_LOAD_PROCS")
	if procs == "" {
		procs = "0"
	}
	timeout := 30 * time.Minute
	if v := os.Getenv("SAVIOR_LOAD_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("SAVIOR_LOAD_TIMEOUT: %v", err)
		}
		timeout = d
	}
	base := os.Getenv("SAVIOR_LOAD_DIR")
	if base == "" {
		base = t.TempDir()
	}
	var out []loadScenario
	for _, item := range strings.Split(list, ",") {
		a, b, ok := strings.Cut(strings.TrimSpace(item), "x")
		nodes, err1 := strconv.Atoi(a)
		tasks, err2 := strconv.Atoi(b)
		if !ok || err1 != nil || err2 != nil || nodes < 1 || tasks < 1 {
			t.Fatalf("SAVIOR_LOAD_SCENARIOS: bad item %q (want NODESxTASKS)", item)
		}
		for _, p := range strings.Split(procs, ",") {
			np, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				t.Fatalf("SAVIOR_LOAD_PROCS: %v", err)
			}
			sc := loadScenario{
				Nodes: nodes, Tasks: tasks, Procs: np,
				Slots: envInt("SAVIOR_LOAD_SLOTS", 4), TaskS: envFloat("SAVIOR_LOAD_TASK_S", 5),
				LogBPS: envInt("SAVIOR_LOAD_LOG_BPS", 256), OutBytes: envInt("SAVIOR_LOAD_OUT_BYTES", 2048),
				HBS: envFloat("SAVIOR_LOAD_HB_S", 5), DashS: envFloat("SAVIOR_LOAD_DASH_S", 5),
				History: envInt("SAVIOR_LOAD_HISTORY", 0), HistJobs: envInt("SAVIOR_LOAD_HISTORY_JOBS", 499),
				IdleS: envFloat("SAVIOR_LOAD_IDLE_S", 65), Timeout: timeout, BaseDir: base, ProfDir: os.Getenv("SAVIOR_LOAD_PROFILE"),
				RAMMB: envInt("SAVIOR_LOAD_HIVE_RAM_MB", 0),
			}
			sc.Name = fmt.Sprintf("n%d-t%d-p%d-h%d", nodes, tasks, np, sc.History)
			if sc.RAMMB > 0 {
				sc.Name += fmt.Sprintf("-ram%d", sc.RAMMB)
			}
			out = append(out, sc)
		}
	}
	return out
}

// TestLoad runs the opt-in big load runs and prints the result tables.
func TestLoad(t *testing.T) {
	if os.Getenv("SAVIOR_LOAD") == "" {
		t.Skip("set SAVIOR_LOAD=1 for the big load runs (see load_test.go)")
	}
	var reps []*loadReport
	for _, sc := range loadScenariosFromEnv(t) {
		t.Logf("load run %s: %d nodes x %d slots, %d tasks of ~%gs, history %d records, GOMAXPROCS %d",
			sc.Name, sc.Nodes, sc.Slots, sc.Tasks, sc.TaskS, sc.History, sc.Procs)
		r := runLoad(t, sc)
		reps = append(reps, r)
		printLoadReports(t, []*loadReport{r}, true)
	}
	if len(reps) > 1 {
		printLoadReports(t, reps, false)
	}
}

// TestLoadSmall is the CI-sized run: real HTTPS, real hive, 8 simulated
// nodes in a child process, 240 tasks with logs and outputs, the dashboard
// polling every 250 ms. Every task must succeed on its first attempt with
// its output recorded, no node may go offline, and no request may fail.
func TestLoadSmall(t *testing.T) {
	if os.Getenv("SAVIOR_LOAD_CHILD") != "" {
		t.Skip("child process")
	}
	sc := loadScenario{Name: "small", Nodes: 8, Tasks: 240, Slots: 2, TaskS: 0.05, LogBPS: 1024, OutBytes: 512,
		HBS: 5, DashS: 0.25, Timeout: 90 * time.Second, BaseDir: t.TempDir()}
	r := runLoad(t, sc)
	printLoadReports(t, []*loadReport{r}, true)
	for _, f := range r.failures {
		t.Error(f)
	}
	if !r.finished || r.done != sc.Tasks {
		t.Fatalf("job did not finish: %d of %d tasks succeeded", r.done, sc.Tasks)
	}
	if r.extra != 0 || r.lost != 0 || r.noOutput != 0 {
		t.Errorf("extra attempts %d, lost %d, succeeded without outputs %d; want 0", r.extra, r.lost, r.noOutput)
	}
	if r.offline != 0 {
		t.Errorf("%d node offline events during the job; want 0", r.offline)
	}
	if got := r.child.Counters["tasks_succeeded"]; got != int64(sc.Tasks) {
		t.Errorf("nodes saw %d successful reports, want %d", got, sc.Tasks)
	}
	for ep, s := range r.child.Endpoints {
		if s.Errs > 0 || len(s.Codes) > 0 {
			t.Errorf("%s: %d transport errors, non-2xx statuses %v", ep, s.Errs, s.Codes)
		}
	}
	for _, ep := range []string{"heartbeat", "claim (got tasks)", "log chunk", "blob put (output)", "report", "admin nodes", "admin tasks page 1"} {
		if r.child.Endpoints[ep].N == 0 {
			t.Errorf("no %s requests were measured", ep)
		}
	}
	if len(r.child.Errors) > 0 {
		t.Errorf("swarm errors: %q", r.child.Errors)
	}
	if r.writes.n == 0 {
		t.Errorf("state.json was not written for the job submission")
	}
}

// --- the run ---

// countingLog counts hive log messages by text ("node offline", ...).
type countingLog struct {
	mu sync.Mutex
	n  map[string]int
}

func (h *countingLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingLog) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.n[r.Message]++
	h.mu.Unlock()
	return nil
}
func (h *countingLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingLog) WithGroup(string) slog.Handler      { return h }
func (h *countingLog) get(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n[msg]
}
func (h *countingLog) copy() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int, len(h.n))
	for k, v := range h.n {
		out[k] = v
	}
	return out
}

// loadHiveConfig is a production hive config: default timings (zero tune).
func loadHiveConfig(dir string, log *slog.Logger, ln net.Listener) Config {
	return Config{DataDir: dir, SwarmKey: testKey, AdminToken: testAdmin, Log: log, StatusFile: "-", Listener: ln}
}

func loadJobSpec(name string, count int) proto.JobSpec {
	return proto.JobSpec{Name: name, Script: "render-frame {{index}} > result.json", Count: count, Outputs: []string{"result.json"},
		Resources: proto.Resources{Cores: 1, MemMB: 128, DiskMB: 64}, TimeoutS: 3600}
}

func runLoad(t *testing.T, sc loadScenario) *loadReport {
	r := &loadReport{sc: sc, ideal: float64(sc.Nodes*max(sc.Slots, 1)) / sc.TaskS}
	if sc.Procs > 0 {
		old := runtime.GOMAXPROCS(sc.Procs)
		defer runtime.GOMAXPROCS(old)
	}
	if sc.RAMMB > 0 {
		if limit, ok := setMemoryLimit(sc.RAMMB, "", debug.SetMemoryLimit); ok {
			defer debug.SetMemoryLimit(math.MaxInt64)
			t.Logf("soft memory limit %d MiB, as main.go sets it on a %d MB hive", limit>>20, sc.RAMMB)
		}
	}
	if sc.ProfDir != "" {
		runtime.SetMutexProfileFraction(10)
		defer runtime.SetMutexProfileFraction(0)
	}
	dir := filepath.Join(sc.BaseDir, sc.Name)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logs := &countingLog{n: map[string]int{}}
	log := slog.New(logs)
	ids := make([]string, sc.Nodes)
	for i := range ids {
		ids[i] = loadNodeID(i)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if sc.History > 0 {
		r.hist = buildHistory(t, dir, log, sc, ids)
	}
	var s *Server
	if r.hist != nil {
		runtime.GC()
		pk := startPeakSampler(5 * time.Millisecond)
		t0 := time.Now()
		s, err = New(loadHiveConfig(dir, log, ln))
		r.hist.restart = time.Since(t0)
		r.hist.restPeak = pk.stop()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		r.hist.afterLoad = ms.HeapAlloc
	} else {
		s, err = New(loadHiveConfig(dir, log, ln))
	}
	if err != nil {
		t.Fatalf("hive: %v", err)
	}
	ctx, stopHive := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	var shutOnce sync.Once
	shutdownHive := func() {
		shutOnce.Do(func() {
			t0 := time.Now()
			stopHive()
			select {
			case err := <-runDone:
				if err != nil {
					r.failures = append(r.failures, "hive Run: "+err.Error())
				}
			case <-time.After(5 * time.Minute):
				r.failures = append(r.failures, "hive did not stop within 5 minutes")
			}
			r.shutdown = time.Since(t0)
		})
	}
	t.Cleanup(shutdownHive)
	url := "https://" + ln.Addr().String()

	smp := newLoadSampler(s)
	smp.start()
	defer smp.stop()

	// The swarm.
	params := loadParams{HiveURL: url, Fingerprint: s.Fingerprint(), SwarmKey: s.SwarmKey(), AdminToken: s.AdminToken(),
		Nodes: sc.Nodes, Slots: sc.Slots, HBSeconds: sc.HBS, TaskSeconds: sc.TaskS, LogBPS: sc.LogBPS, OutBytes: sc.OutBytes,
		DashSeconds: sc.DashS, SpreadS: min(float64(sc.Nodes)/100, 10), ResultPath: filepath.Join(dir, "swarm-result.json")}
	child := startLoadChild(t, dir, params)
	defer child.stop()

	// Wait until every node is online.
	deadline := time.Now().Add(2*time.Minute + time.Duration(params.SpreadS*float64(time.Second)))
	for {
		if online := loadOnline(s); online >= sc.Nodes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d nodes came online", loadOnline(s), sc.Nodes)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The job.
	offline0 := logs.get("node offline")
	cpu0, disk0, gc0 := loadCPUSeconds(), loadDiskWrites(), gcCPUSeconds()
	smp.resetPeaks()
	var cpuProf *os.File
	if sc.ProfDir != "" {
		_ = os.MkdirAll(sc.ProfDir, 0o755)
		if f, err := os.Create(filepath.Join(sc.ProfDir, "cpu-"+sc.Name+".pprof")); err == nil {
			if pprof.StartCPUProfile(f) == nil {
				cpuProf = f
			} else {
				f.Close()
			}
		}
	}
	hc := pinnedClient(s.Fingerprint())
	defer hc.CloseIdleConnections()
	spec := loadJobSpec("load-"+sc.Name, sc.Tasks)
	body, _ := json.Marshal(spec)
	submitAt := time.Now()
	child.send(fmt.Sprintf("submit %d\n", submitAt.UnixNano()))
	st, raw, lat := loadAdmin(hc, "POST", url+"/api/v1/admin/jobs", body)
	r.submitLat = lat
	var jd proto.JobDetail
	if st != 200 || json.Unmarshal(raw, &jd) != nil {
		t.Fatalf("submit: %d %s", st, raw)
	}
	jobStart := time.Now()
	type point struct {
		at         time.Time
		done, disp int
	}
	var tl []point
	lastLog := time.Now()
	for {
		s.mu.Lock()
		j := s.jobs[jd.ID]
		var c proto.TaskCounts
		fin := j == nil || j.finished()
		if j != nil {
			c = j.counts
		}
		s.mu.Unlock()
		now := time.Now()
		tl = append(tl, point{now, c.Succeeded, c.Succeeded + c.Failed + c.Running + c.Assigned + c.Canceled})
		r.done = c.Succeeded
		if fin {
			r.finished = c.Succeeded+c.Failed+c.Canceled >= sc.Tasks
			break
		}
		if now.Sub(lastLog) >= 30*time.Second && len(tl) > 1 {
			// Progress, for long runs: rate over the last 30 s and the
			// worst state-mutex wait seen in it.
			k := len(tl) - 1
			for k > 0 && now.Sub(tl[k].at) < 30*time.Second {
				k--
			}
			_, _, worst := smp.lockWaits(lastLog, now)
			t.Logf("%s: %ds: %d/%d done, %.1f tasks/s, mutex wait max %s ms, online %d, offline events %d",
				sc.Name, int(now.Sub(submitAt).Seconds()), c.Succeeded, sc.Tasks, float64(c.Succeeded-tl[k].done)/now.Sub(tl[k].at).Seconds(),
				ms(worst), loadOnline(s), logs.get("node offline")-offline0)
			lastLog = now
		}
		if now.Sub(jobStart) > sc.Timeout {
			r.failures = append(r.failures, fmt.Sprintf("timeout after %s: %d of %d tasks done", sc.Timeout, c.Succeeded, sc.Tasks))
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	jobEnd := time.Now()
	r.wall = jobEnd.Sub(submitAt)
	r.hiveCPU = loadCPUSeconds() - cpu0
	r.gcCPU = gcCPUSeconds() - gc0
	if d1 := loadDiskWrites(); disk0 >= 0 && d1 >= 0 {
		r.diskBytes = d1 - disk0
	}
	if cpuProf != nil {
		pprof.StopCPUProfile()
		cpuProf.Close()
	}
	r.offline = logs.get("node offline") - offline0
	r.rate = float64(r.done) / r.wall.Seconds()
	// Decile rates and the best 5 s dispatch window.
	at := func(frac float64) time.Time {
		for _, p := range tl {
			if float64(p.done) >= frac*float64(sc.Tasks) {
				return p.at
			}
		}
		return jobEnd
	}
	if t10 := at(0.1); t10.After(submitAt) {
		r.rateFirst = 0.1 * float64(sc.Tasks) / t10.Sub(submitAt).Seconds()
	}
	if t90, t100 := at(0.9), at(1.0); t100.After(t90) && r.finished {
		r.rateLast = 0.1 * float64(sc.Tasks) / t100.Sub(t90).Seconds()
	}
	prev := submitAt
	for d := 1; d <= 10; d++ {
		if float64(r.done) < float64(d)/10*float64(sc.Tasks) {
			break // not reached
		}
		at := at(float64(d) / 10)
		if at.After(prev) {
			r.deciles = append(r.deciles, 0.1*float64(sc.Tasks)/at.Sub(prev).Seconds())
		}
		prev = at
	}
	for i := range tl {
		for k := i; k < len(tl); k++ {
			if d := tl[k].at.Sub(tl[i].at); d >= 5*time.Second {
				r.peakDisp = max(r.peakDisp, float64(tl[k].disp-tl[i].disp)/d.Seconds())
				break
			}
		}
	}
	r.heapMax, r.inuseMax, r.sysMax, r.gorMax, r.ioMax = smp.peaks()
	r.writes = smp.persists(submitAt, jobEnd)
	r.lockP50, r.lockP99, r.lockMax = smp.lockWaits(submitAt, jobEnd)

	// Idle: heartbeats only.
	if sc.IdleS > 0 && r.finished {
		cpuI := loadCPUSeconds()
		idleStart := time.Now()
		time.Sleep(time.Duration(sc.IdleS * float64(time.Second)))
		r.idleCPU = (loadCPUSeconds() - cpuI) / time.Since(idleStart).Minutes()
		r.idle = smp.persists(idleStart, time.Now())
	}

	// One-shot admin views that are not polled.
	if st, raw, lat := loadAdmin(hc, "GET", url+"/api/v1/admin/jobs/"+jd.ID+"/outputs", nil); st == 200 {
		var outs []proto.OutputEntry
		_ = json.Unmarshal(raw, &outs)
		r.outputsN, r.outputsB, r.outputsL = len(outs), len(raw), lat
	}
	if st, raw, lat := loadAdmin(hc, "GET", url+"/api/v1/admin/blobs", nil); st == 200 {
		r.blobListB, r.blobListL = len(raw), lat
	}

	r.child = child.stop()
	r.logs = logs.copy()

	// Hive-side accounting of the job.
	s.mu.Lock()
	if j := s.jobs[jd.ID]; j != nil {
		for _, tk := range j.tasks {
			r.extra += max(tk.Attempt-1, 0)
			for _, h := range tk.History {
				if h.Outcome == "lost" {
					r.lost++
					if r.lostWhy == nil {
						r.lostWhy = map[string]int{}
					}
					r.lostWhy[h.Error]++
				}
			}
			if tk.State == proto.TaskSucceeded && len(tk.Outputs) == 0 && sc.OutBytes > 0 {
				r.noOutput++
			}
		}
	}
	r.records, r.blobs = s.taskRecords, len(s.blobMeta)
	s.mu.Unlock()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	r.heapEnd = ms.HeapAlloc
	if sc.ProfDir != "" {
		if f, err := os.Create(filepath.Join(sc.ProfDir, "heap-"+sc.Name+".pprof")); err == nil {
			_ = pprof.WriteHeapProfile(f)
			f.Close()
		}
		if f, err := os.Create(filepath.Join(sc.ProfDir, "mutex-"+sc.Name+".pprof")); err == nil {
			_ = pprof.Lookup("mutex").WriteTo(f, 0)
			f.Close()
		}
	}
	shutdownHive()

	// What the hive saved must hold the finished job with every task, as a
	// restarted hive loads it (state.json and the settled jobs' files).
	if s2, err := New(loadHiveConfig(dir, quietLog(), nil)); err != nil {
		r.failures = append(r.failures, "saved state: "+err.Error())
	} else {
		s2.mu.Lock()
		if j := s2.jobs[jd.ID]; j != nil {
			ok := 0
			for _, tk := range j.tasks {
				if tk.State == proto.TaskSucceeded && (len(tk.Outputs) == 1 || sc.OutBytes == 0) {
					ok++
				}
			}
			if r.finished && ok != sc.Tasks {
				r.failures = append(r.failures, fmt.Sprintf("saved state: %d of %d tasks succeeded (with their output)", ok, sc.Tasks))
			}
		} else if r.records >= sc.Tasks {
			r.failures = append(r.failures, "saved state: job "+jd.ID+" missing")
		}
		s2.mu.Unlock()
		if err := s2.Close(); err != nil {
			r.failures = append(r.failures, "saved state: closing the reloaded hive: "+err.Error())
		}
	}
	return r
}

// buildHistory makes a hive in dir hold sc.History finished task records,
// measures one full persist of it and closes it again.
func buildHistory(t *testing.T, dir string, log *slog.Logger, sc loadScenario, ids []string) *histStats {
	h := &histStats{records: sc.History}
	s0, err := New(loadHiveConfig(dir, log, nil))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	prefillHistory(t, s0, sc.HistJobs, sc.History, ids, sc.OutBytes, true)
	h.prefill = time.Since(t0)
	h.hold, h.persist, h.size, h.alloc, h.peak, h.base = measurePersist(t, s0)
	if err := s0.Close(); err != nil {
		t.Fatal(err)
	}
	return h
}

// measurePersist times one full state save and the memory it takes (alloc:
// bytes allocated by it), then how long the snapshot of a save holds the
// state mutex once it is done (jobs that settled before it have their own
// files by then).
func measurePersist(tb testing.TB, s *Server) (hold, write time.Duration, size int64, alloc, peak, base uint64) {
	s.io.flush()
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	base = m0.HeapAlloc
	pk := startPeakSampler(2 * time.Millisecond)
	t0 := time.Now()
	if err := s.persist(true); err != nil {
		tb.Fatal(err)
	}
	write = time.Since(t0)
	peak = pk.stop()
	runtime.ReadMemStats(&m1)
	alloc = m1.TotalAlloc - m0.TotalAlloc
	s.mu.Lock()
	t0 = time.Now()
	snap := s.snapshotLocked()
	hold = time.Since(t0)
	s.mu.Unlock()
	_ = snap
	if fi, err := os.Stat(filepath.Join(s.DataDir(), stateFile)); err == nil {
		size = fi.Size()
	}
	return hold, write, size, alloc, peak, base
}

func loadOnline(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now, k := time.Now(), 0
	for _, n := range s.nodes {
		if n.isOnline(now, s.cfg.OfflineAfter) {
			k++
		}
	}
	return k
}

func loadAdmin(hc *http.Client, method, url string, body []byte) (int, []byte, time.Duration) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Authorization", "Bearer "+testAdmin)
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return 0, []byte(err.Error()), time.Since(start)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, time.Since(start)
}

// --- the child process ---

type loadChild struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bytes.Buffer
	result string
	once   sync.Once
	res    loadResult
	mu     sync.Mutex
}

func startLoadChild(t *testing.T, dir string, p loadParams) *loadChild {
	pf := filepath.Join(dir, "swarm-params.json")
	raw, _ := json.Marshal(p)
	if err := os.WriteFile(pf, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoadSwarmChild$", "-test.count=1", "-test.timeout=0")
	env := []string{"SAVIOR_LOAD_CHILD=" + pf}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOMAXPROCS=") && !strings.HasPrefix(e, "SAVIOR_LOAD_CHILD=") {
			env = append(env, e)
		}
	}
	cmd.Env = env
	c := &loadChild{t: t, cmd: cmd, out: &bytes.Buffer{}, result: p.ResultPath}
	cmd.Stdout = &lockedWriter{w: c.out, mu: &c.mu}
	cmd.Stderr = cmd.Stdout
	var err error
	if c.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.stop() })
	return c
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w.Len() < 1<<20 {
		l.w.Write(p)
	}
	return len(p), nil
}

func (c *loadChild) send(line string) { _, _ = io.WriteString(c.stdin, line) }

// stop closes the child's stdin (it stops its nodes and writes its result)
// and returns the result.
func (c *loadChild) stop() loadResult {
	c.once.Do(func() {
		c.stdin.Close()
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		var err error
		select {
		case err = <-done:
		case <-time.After(60 * time.Second):
			_ = c.cmd.Process.Kill()
			err = <-done
		}
		raw, rerr := os.ReadFile(c.result)
		if rerr == nil {
			rerr = json.Unmarshal(raw, &c.res)
		}
		if err != nil || rerr != nil {
			c.mu.Lock()
			out := c.out.String()
			c.mu.Unlock()
			c.t.Errorf("load swarm: exit %v, result %v; output:\n%s", err, rerr, out)
		}
	})
	return c.res
}

// gcCPUSeconds is the CPU time the Go runtime estimates for GC so far.
func gcCPUSeconds() float64 {
	s := []metrics.Sample{{Name: "/cpu/classes/gc/total:cpu-seconds"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64 {
		return 0
	}
	return s[0].Value.Float64()
}

// --- sampling ---

type persistEvent struct {
	at   time.Time
	size int64
	dur  time.Duration
}

type lockWait struct {
	at time.Time
	d  time.Duration
}

type loadSampler struct {
	s     *Server
	path  string
	stopc chan struct{}
	wg    sync.WaitGroup

	mu                        sync.Mutex
	heapMax, inuseMax, sysMax uint64
	gorMax, ioMax             int
	writes                    []persistEvent
	waits                     []lockWait
}

func newLoadSampler(s *Server) *loadSampler {
	return &loadSampler{s: s, path: filepath.Join(s.DataDir(), stateFile), stopc: make(chan struct{})}
}

func (m *loadSampler) start() {
	m.wg.Add(3)
	go m.memLoop()
	go m.fileLoop()
	go m.probeLoop()
}

func (m *loadSampler) stop() {
	select {
	case <-m.stopc:
	default:
		close(m.stopc)
	}
	m.wg.Wait()
}

func (m *loadSampler) every(d time.Duration, fn func()) {
	defer m.wg.Done()
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		fn()
		select {
		case <-m.stopc:
			return
		case <-t.C:
		}
	}
}

func (m *loadSampler) memLoop() {
	m.every(250*time.Millisecond, func() {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		g := runtime.NumGoroutine()
		m.s.io.mu.Lock()
		q := len(m.s.io.jobs)
		m.s.io.mu.Unlock()
		m.mu.Lock()
		m.ioMax = max(m.ioMax, q)
		m.heapMax = max(m.heapMax, ms.HeapAlloc)
		m.inuseMax = max(m.inuseMax, ms.HeapInuse)
		m.sysMax = max(m.sysMax, ms.Sys)
		m.gorMax = max(m.gorMax, g)
		m.mu.Unlock()
	})
}

// fileLoop notices every state.json rewrite (a new file is renamed into
// place each time) and reads the write duration the hive measured.
func (m *loadSampler) fileLoop() {
	var lastMod time.Time
	var lastSize int64 = -1
	sampled := false
	m.every(50*time.Millisecond, func() {
		fi, err := os.Stat(m.path)
		first := !sampled
		sampled = true
		if err != nil || (fi.ModTime().Equal(lastMod) && fi.Size() == lastSize) {
			return
		}
		lastMod, lastSize = fi.ModTime(), fi.Size()
		if first {
			return // written before sampling started
		}
		m.s.persistMu.Lock()
		d := m.s.lastWriteDur
		m.s.persistMu.Unlock()
		m.mu.Lock()
		m.writes = append(m.writes, persistEvent{at: time.Now(), size: fi.Size(), dur: d})
		m.mu.Unlock()
	})
}

// probeLoop measures how long acquiring the state mutex takes: what every
// heartbeat, claim and report waits for before it is handled.
func (m *loadSampler) probeLoop() {
	m.every(50*time.Millisecond, func() {
		t0 := time.Now()
		m.s.mu.Lock()
		d := time.Since(t0)
		m.s.mu.Unlock()
		m.mu.Lock()
		m.waits = append(m.waits, lockWait{t0, d})
		m.mu.Unlock()
	})
}

func (m *loadSampler) resetPeaks() {
	m.mu.Lock()
	m.heapMax, m.inuseMax, m.sysMax, m.gorMax, m.ioMax = 0, 0, 0, 0, 0
	m.mu.Unlock()
}

func (m *loadSampler) peaks() (uint64, uint64, uint64, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.heapMax, m.inuseMax, m.sysMax, m.gorMax, m.ioMax
}

func (m *loadSampler) persists(from, to time.Time) persistStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	var p persistStats
	for _, w := range m.writes {
		if w.at.Before(from) || w.at.After(to) {
			continue
		}
		p.n++
		p.bytes += w.size
		p.maxSize = max(p.maxSize, w.size)
		p.maxDur = max(p.maxDur, w.dur)
		p.totalDur += w.dur
	}
	if mins := to.Sub(from).Minutes(); mins > 0 {
		p.perMin = float64(p.n) / mins
	}
	return p
}

func (m *loadSampler) lockWaits(from, to time.Time) (p50, p99, mx time.Duration) {
	m.mu.Lock()
	var v []time.Duration
	for _, w := range m.waits {
		if !w.at.Before(from) && !w.at.After(to) {
			v = append(v, w.d)
		}
	}
	m.mu.Unlock()
	if len(v) == 0 {
		return 0, 0, 0
	}
	sort.Slice(v, func(i, k int) bool { return v[i] < v[k] })
	at := func(q float64) time.Duration { return v[min(int(q*float64(len(v))), len(v)-1)] }
	return at(0.5), at(0.99), v[len(v)-1]
}

// peakSampler records the largest HeapAlloc seen until stop.
type peakSampler struct {
	stopc chan struct{}
	done  chan uint64
}

func startPeakSampler(every time.Duration) *peakSampler {
	p := &peakSampler{stopc: make(chan struct{}), done: make(chan uint64, 1)}
	go func() {
		var peak uint64
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapAlloc)
			select {
			case <-p.stopc:
				p.done <- peak
				return
			case <-t.C:
			}
		}
	}()
	return p
}

func (p *peakSampler) stop() uint64 {
	close(p.stopc)
	return <-p.done
}

// --- reporting ---

func mb(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<20)) }

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }

func printLoadReports(t *testing.T, reps []*loadReport, detail bool) {
	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 2, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(&b)
	fmt.Fprintln(w, "run\tprocs\tnodes\ttasks\tdone\twall s\ttasks/s\tideal/s\tfirst10%/s\tlast10%/s\tpeak disp/s\thive CPU s\tGC CPU s\tCPU pct of 1 core\theap max MB\tlive end MB\tsys MB\tgoroutines\toffline\tlost\textra att\t")
	for _, r := range reps {
		sc := r.sc
		procs := strconv.Itoa(sc.Procs)
		if sc.Procs == 0 {
			procs = strconv.Itoa(runtime.NumCPU())
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%.1f\t%.1f\t%.0f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%.0f\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t\n",
			sc.Name, procs, sc.Nodes, sc.Tasks, r.done, r.wall.Seconds(), r.rate, r.ideal, r.rateFirst, r.rateLast, r.peakDisp,
			r.hiveCPU, r.gcCPU, 100*r.hiveCPU/r.wall.Seconds(), mb(r.heapMax), mb(r.heapEnd), mb(r.sysMax), r.gorMax, r.offline, r.lost, r.extra)
	}
	w.Flush()
	fmt.Fprintln(&b)
	fmt.Fprintln(w, "run\tstate writes (job)\tper min\tavg MB\tmax MB\tMB written\tmax write ms\tavg write ms\tidle writes/min\tidle MB/min\tidle CPU s/min\thive disk MB (job)\tlock wait p50 ms\tp99 ms\tmax ms\tio queue max\tsubmit ms\trecords\tblobs\t")
	for _, r := range reps {
		avg, avgDur := 0.0, time.Duration(0)
		if r.writes.n > 0 {
			avg = float64(r.writes.bytes) / float64(r.writes.n) / (1 << 20)
			avgDur = r.writes.totalDur / time.Duration(r.writes.n)
		}
		idleMB := 0.0
		if r.sc.IdleS > 0 {
			idleMB = float64(r.idle.bytes) / (1 << 20) / (r.sc.IdleS / 60)
		}
		fmt.Fprintf(w, "%s\t%d\t%.1f\t%.2f\t%.2f\t%.1f\t%s\t%s\t%.2f\t%.1f\t%.2f\t%.1f\t%s\t%s\t%s\t%d\t%s\t%d\t%d\t\n",
			r.sc.Name, r.writes.n, r.writes.perMin, avg, float64(r.writes.maxSize)/(1<<20), float64(r.writes.bytes)/(1<<20),
			ms(r.writes.maxDur), ms(avgDur), r.idle.perMin, idleMB, r.idleCPU, float64(r.diskBytes)/(1<<20),
			ms(r.lockP50), ms(r.lockP99), ms(r.lockMax), r.ioMax, ms(r.submitLat), r.records, r.blobs)
	}
	w.Flush()
	for _, r := range reps {
		if r.hist == nil {
			continue
		}
		h := r.hist
		fmt.Fprintln(&b)
		fmt.Fprintln(w, "run\thistory records\tprefill s\tsnapshot hold ms\tpersist ms\tstate.json MB\tpersist alloc MB\tlive heap MB\tpeak during persist MB\trestart s\tpeak during restart MB\tlive after restart MB\t")
		fmt.Fprintf(w, "%s\t%d\t%.1f\t%s\t%s\t%.1f\t%s\t%s\t%s\t%.2f\t%s\t%s\t\n", r.sc.Name, h.records, h.prefill.Seconds(), ms(h.hold), ms(h.persist),
			float64(h.size)/(1<<20), mb(h.alloc), mb(h.base), mb(h.peak), h.restart.Seconds(), mb(h.restPeak), mb(h.afterLoad))
		w.Flush()
	}
	if detail {
		for _, r := range reps {
			fmt.Fprintln(&b)
			fmt.Fprintf(&b, "%s: request latency (ms) as the nodes and the dashboard saw it; swarm CPU %.1f s\n", r.sc.Name, r.child.CPUSeconds)
			fmt.Fprintln(w, "endpoint\tn\tp50\tp90\tp99\tmax\terrors\tnon-2xx\tavg bytes\tmax bytes\t")
			eps := make([]string, 0, len(r.child.Endpoints))
			for ep := range r.child.Endpoints {
				eps = append(eps, ep)
			}
			sort.Strings(eps)
			for _, ep := range eps {
				s := r.child.Endpoints[ep]
				fmt.Fprintf(w, "%s\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t%v\t%d\t%d\t\n", ep, s.N, s.P50, s.P90, s.P99, s.Max, s.Errs, s.Codes, s.AvgBytes, s.MaxBytes)
			}
			fmt.Fprintf(w, "outputs list (once)\t1\t%s\t\t\t\t\t\t%d\t(%d entries)\t\n", ms(r.outputsL), r.outputsB, r.outputsN)
			fmt.Fprintf(w, "blob list (once)\t1\t%s\t\t\t\t\t\t%d\t\t\n", ms(r.blobListL), r.blobListB)
			w.Flush()
			var keys []string
			for k := range r.child.Counters {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if len(r.deciles) > 0 {
				fmt.Fprintf(&b, "tasks/s by decile of the job:")
				for _, d := range r.deciles {
					fmt.Fprintf(&b, " %.1f", d)
				}
				fmt.Fprintln(&b)
			}
			fmt.Fprintf(&b, "swarm counters:")
			for _, k := range keys {
				fmt.Fprintf(&b, " %s=%d", k, r.child.Counters[k])
			}
			fmt.Fprintf(&b, "\nhive shutdown (drain io queue, final save) %s ms; hive log messages:", ms(r.shutdown))
			var msgs []string
			for k := range r.logs {
				msgs = append(msgs, k)
			}
			sort.Strings(msgs)
			for _, k := range msgs {
				fmt.Fprintf(&b, " %q=%d", k, r.logs[k])
			}
			fmt.Fprintln(&b)
			if len(r.lostWhy) > 0 {
				fmt.Fprintf(&b, "lost attempts by reason: %v\n", r.lostWhy)
			}
			if len(r.child.Errors) > 0 {
				fmt.Fprintf(&b, "swarm errors (first %d): %q\n", len(r.child.Errors), r.child.Errors)
			}
			if len(r.failures) > 0 {
				fmt.Fprintf(&b, "failures: %q\n", r.failures)
			}
		}
	}
	t.Log(b.String())
	if out := os.Getenv("SAVIOR_LOAD_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(b.Bytes())
			f.Close()
		}
	}
}
