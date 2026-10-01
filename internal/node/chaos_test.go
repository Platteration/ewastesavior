//go:build linux

package node

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// TestChaos is a fault-injection soak: a real hive and real node agents
// (sandbox = none) in this process, a workload of jobs whose outputs can be
// verified, and random faults (agent crashes and restarts, hive restarts,
// lost, delayed and duplicated requests and responses, partitions, power
// pause and preemption, drain, clock jumps, replayed reports, job
// cancellations), with the scheduler's invariants checked all along and
// once everything has settled.
//
//	go test ./internal/node/ -run TestChaos                     # short run, fixed seed (~75 s)
//	SAVIOR_CHAOS=10m SAVIOR_CHAOS_SEED=7 go test ./internal/node/ -run TestChaos -timeout 30m
//
// Environment:
//
//	SAVIOR_CHAOS         soak length (Go duration); unset = short run
//	SAVIOR_CHAOS_SEED    seed (default 1 for the short run, random for soaks)
//	SAVIOR_CHAOS_NODES   number of node agents (default 4)
//	SAVIOR_CHAOS_LOGDIR  write hive, node and event logs there
//	SAVIOR_CHAOS_FAULTS  comma-separated fault kinds to enable (default all):
//	                     node,hive,net,partition,storm,dup,replay,power,drain,clock,cancel;
//	                     crash (simulated power cut of the hive) is off by
//	                     default; SAVIOR_CHAOS_CRASH=1 adds it
//	SAVIOR_CHAOS_UIDBASE first task uid (default 20000); give parallel runs
//	                     ranges at least 100 x nodes apart
func TestChaos(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos soak")
	}
	cfg := chaosConfigFromEnv(t)
	runChaos(t, cfg)
}

type chaosConfig struct {
	seed     int64
	total    time.Duration
	faultFor time.Duration
	quiesce  time.Duration
	nodes    int
	short    bool
	logDir   string
	faults   map[string]bool
	uidBase  int // node k runs tasks as uids uidBase+100k...
}

func chaosConfigFromEnv(t *testing.T) chaosConfig {
	cfg := chaosConfig{seed: 1, total: 75 * time.Second, nodes: 4, short: true, faults: map[string]bool{}}
	if v := os.Getenv("SAVIOR_CHAOS"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 30*time.Second {
			t.Fatalf("SAVIOR_CHAOS=%q: want a duration of at least 30s", v)
		}
		cfg.total, cfg.short = d, false
		cfg.seed = time.Now().UnixNano() % 1000000
	}
	if v := os.Getenv("SAVIOR_CHAOS_SEED"); v != "" {
		s, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("SAVIOR_CHAOS_SEED=%q", v)
		}
		cfg.seed = s
	}
	if v := os.Getenv("SAVIOR_CHAOS_NODES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 2 || n > 32 {
			t.Fatalf("SAVIOR_CHAOS_NODES=%q: want 2..32", v)
		}
		cfg.nodes = n
	}
	cfg.uidBase = 20000
	if v := os.Getenv("SAVIOR_CHAOS_UIDBASE"); v != "" {
		// Runs in parallel need their own uid ranges: a starting runner
		// kills every process of its slot uids.
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 || n > 1<<30 {
			t.Fatalf("SAVIOR_CHAOS_UIDBASE=%q", v)
		}
		cfg.uidBase = n
	}
	cfg.logDir = os.Getenv("SAVIOR_CHAOS_LOGDIR")
	if cfg.logDir != "" {
		if err := os.MkdirAll(cfg.logDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	all := []string{"node", "hive", "net", "partition", "storm", "dup", "replay", "power", "drain", "clock", "cancel"}
	if v := os.Getenv("SAVIOR_CHAOS_FAULTS"); v != "" {
		for _, f := range strings.Split(v, ",") {
			cfg.faults[strings.TrimSpace(f)] = true
		}
	} else {
		for _, f := range all {
			cfg.faults[f] = true
		}
	}
	if os.Getenv("SAVIOR_CHAOS_CRASH") == "1" {
		cfg.faults["crash"] = true
	}
	// The faults stop with enough time left for the system to settle.
	cfg.quiesce = 180 * time.Second
	if cfg.short {
		cfg.quiesce = 90 * time.Second
		cfg.faultFor = 40 * time.Second
	} else {
		cfg.faultFor = cfg.total - 60*time.Second
	}
	return cfg
}

func (cfg chaosConfig) replay() string {
	env := fmt.Sprintf("SAVIOR_CHAOS_SEED=%d", cfg.seed)
	if !cfg.short {
		env = fmt.Sprintf("SAVIOR_CHAOS=%s %s", cfg.total, env)
	}
	if cfg.nodes != 4 {
		env += fmt.Sprintf(" SAVIOR_CHAOS_NODES=%d", cfg.nodes)
	}
	if v := os.Getenv("SAVIOR_CHAOS_FAULTS"); v != "" {
		env += " SAVIOR_CHAOS_FAULTS=" + v
	}
	if os.Getenv("SAVIOR_CHAOS_CRASH") == "1" {
		env += " SAVIOR_CHAOS_CRASH=1"
	}
	return env + " go test ./internal/node/ -run 'TestChaos$' -count=1 -v -timeout 60m"
}

func runChaos(t *testing.T, cfg chaosConfig) {
	t.Logf("chaos: seed %d, %s total, faults for %s, %d nodes; replay with: %s", cfg.seed, cfg.total, cfg.faultFor, cfg.nodes, cfg.replay())
	g0, fd0 := settleCounts(0, 0, 0)

	c := newChaosCluster(t, cfg)
	ck := newChaosChecker(c)
	c.checker = ck
	failed := true
	defer func() {
		if failed {
			t.Logf("chaos: seed %d; last events:\n%s", cfg.seed, strings.Join(c.ev.tail(80), "\n"))
		}
	}()
	for _, n := range c.nodes {
		if err := n.startAgent(); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "all nodes connected", 60*time.Second, func() bool {
		for _, n := range c.nodes {
			a, _ := n.current()
			if _, _, _, _, link := a.chaosCapacity(); link != proto.LinkConnected {
				return false
			}
		}
		return true
	})

	rng := rand.New(rand.NewSource(cfg.seed))
	// An input blob for some jobs (fetched through the proxy).
	input := []byte(fmt.Sprintf("chaos-input-%d", cfg.seed))
	inputBlob := ""
	if code, err := c.adminRetry(http.MethodPut, "/api/v1/blobs/"+sha256hex(input), rawBody(input), nil, 30*time.Second); err == nil && code == 200 {
		inputBlob = sha256hex(input)
	} else {
		t.Fatalf("upload input: %d %v", code, err)
	}

	faultEnd := time.Now().Add(cfg.faultFor)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// The checker polls until the end.
	checkerDone := make(chan struct{})
	go func() {
		defer close(checkerDone)
		for {
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
			if c.isHiveUp() {
				ck.round(false) // errors: the hive is restarting; try again
			}
		}
	}()

	// Workload: jobs submitted while the faults run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		wrng := rand.New(rand.NewSource(cfg.seed*7 + 1))
		gap := 4 * time.Second
		if !cfg.short {
			gap = max(4*time.Second, cfg.faultFor/300)
		}
		submitEnd := faultEnd.Add(-8 * time.Second)
		for seq := 0; ; seq++ {
			j := genJob(wrng, seq, cfg.short, cfg.nodes)
			if cfg.short {
				// Make sure the short run has cancels and timeouts.
				switch seq {
				case 2, 6:
					j.makeCancelTarget(wrng)
				case 4:
					j = genTimeoutJob(wrng, seq)
				}
			}
			if wrng.Intn(4) == 0 {
				j.input, j.inputBlob = input, inputBlob
			}
			if !cfg.faults["cancel"] {
				j.cancelIn = 0
			}
			if err := submitChaosJob(c, ck, j); err != nil {
				c.viol.add("submit", "%v", err)
			} else if j.cancelIn > 0 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					time.Sleep(j.cancelIn)
					cancelChaosJob(c, ck, j)
				}()
			}
			if seq < 2 {
				continue // a backlog from the start
			}
			if time.Now().Add(gap).After(submitEnd) {
				return
			}
			time.Sleep(time.Duration(float64(gap) * (0.5 + wrng.Float64())))
			// Backpressure: strict priorities starve low-priority jobs while
			// higher ones keep coming, so keep the backlog within what the
			// swarm can finish once the faults stop.
			for time.Now().Before(submitEnd) && backlog(c) > 3*4*cfg.nodes {
				time.Sleep(time.Second)
			}
		}
	}()

	// An admin who clears quarantines. A quarantine is a violation (only
	// healthy machines run here); clearing it keeps a soak from running out
	// of nodes after one.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(faultEnd) {
			time.Sleep(15 * time.Second)
			clearQuarantines(c)
		}
	}()

	// Faults.
	runFaults(c, rng, faultEnd)
	c.ev.add("faults stopped")

	// Quiesce: everything back to normal, then wait for the jobs.
	c.proxy.setRates(noFaults)
	quiesceStart := time.Now()
	restoreAll(c)
	wg.Wait() // pending cancels and submissions
	settled := waitSettled(c, ck, cfg.quiesce)
	settleTime := time.Since(quiesceStart)
	c.ev.add("settled=%v after %s", settled, settleTime.Round(time.Millisecond))
	close(stop)
	<-checkerDone
	if err := ck.round(true); err != nil {
		c.viol.add("final", "final round: %v", err)
	}
	ck.finalChecks()
	finalCapacityChecks(c, settled)

	// Teardown: agents first (their goroutines must all end), then the rest.
	for _, n := range c.nodes {
		n.stopAgent()
	}
	checkAgentGoroutines(c)
	stateSize := fileSize(filepath.Join(c.hiveDir, "state.json"))
	c.stopHive()
	c.proxy.close()
	c.admin.CloseIdleConnections()
	c.closeLogs()
	c.ev.close()
	g1, fd1 := settleCounts(g0, fd0, 20*time.Second)
	// Agent goroutines left over are reported above (agent-goroutine-leak).
	if left := len(agentGoroutines()); g1-left > g0+2 {
		c.viol.add("goroutine-leak", "%d goroutines before the run, %d after (%d of them agent goroutines):\n%s", g0, g1, left, goroutineDump())
	}
	if fd1 > fd0+2 {
		c.viol.add("fd-leak", "%d open files before the run, %d after: %s", fd0, fd1, strings.Join(openFDs(), ", "))
	}

	// Summary.
	stats := c.proxy.statsSnapshot()
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, " %s=%d", k, stats[k])
	}
	ntasks, nsucc := 0, 0
	ck.mu.Lock()
	for _, tv := range ck.views {
		ntasks++
		if tv.State == proto.TaskSucceeded {
			nsucc++
		}
	}
	soft := len(ck.softFail)
	var softList []string
	for id, why := range ck.softFail {
		softList = append(softList, id+": "+why)
	}
	rounds := ck.rounds
	ck.mu.Unlock()
	restarts := 0
	for _, n := range c.nodes {
		restarts += n.starts - 1
	}
	t.Logf("chaos: seed %d: %d jobs, %d task records (%d succeeded), %d checker rounds, hive epochs %d, agent restarts %d, settled in %s, state.json %d bytes, admin 429s %d, cancel stop latency max %s; proxy:%s",
		cfg.seed, len(ck.jobList()), ntasks, nsucc, rounds, c.epoch(), restarts, settleTime.Round(time.Millisecond), stateSize,
		c.admin429.Load(), ck.maxCancelLatency().Round(time.Millisecond), sb.String())
	if soft > 0 {
		sort.Strings(softList)
		t.Logf("chaos: %d tasks hit the interruption cap (allowed by DESIGN 8.3):\n  %s", soft, strings.Join(softList, "\n  "))
	}
	if n := c.admin429.Load(); n > 0 {
		c.viol.add("admin-rate-limited", "%d admin requests with the right token were refused as rate limited", n)
	}
	list, counts := c.viol.snapshot()
	bad := 0
	for _, vl := range list {
		bad++
		t.Errorf("chaos: VIOLATION [%s at %s, %d total] %s", vl.id, vl.at.Round(time.Millisecond), counts[vl.id], vl.msg)
	}
	if bad > 0 {
		t.Errorf("chaos: %d violations with seed %d; replay with: %s", bad, cfg.seed, cfg.replay())
		return
	}
	failed = false
}

type rawBody []byte

// backlog counts the unfinished tasks of all jobs (-1 when unknown).
func backlog(c *chaosCluster) int {
	var jobs []proto.JobView
	if err := c.adminGet("/api/v1/admin/jobs?limit=1000", &jobs); err != nil {
		return -1
	}
	n := 0
	for _, jv := range jobs {
		n += jv.Counts.Pending + jv.Counts.Assigned + jv.Counts.Running
	}
	return n
}

func submitChaosJob(c *chaosCluster, ck *chaosChecker, j *chaosJob) error {
	var jd proto.JobDetail
	if _, err := c.adminRetry(http.MethodPost, "/api/v1/admin/jobs", j.spec(), &jd, 60*time.Second); err != nil {
		return fmt.Errorf("submit %s: %w", j.name, err)
	}
	j.mu.Lock()
	j.id, j.submittedAt = jd.ID, time.Now()
	j.mu.Unlock()
	ck.addJob(j)
	c.ev.add("submitted %s %s: %d tasks %s prio %d timeout %d cores %g pin %q", j.id, j.name, j.count(), string(j.kinds), j.priority, j.timeoutS, j.cores, j.pinTo)
	return nil
}

func cancelChaosJob(c *chaosCluster, ck *chaosChecker, j *chaosJob) {
	var jv proto.JobView
	j.mu.Lock()
	j.cancelSent = true
	j.mu.Unlock()
	if _, err := c.adminRetry(http.MethodPost, "/api/v1/admin/jobs/"+j.id+"/cancel", nil, &jv, 60*time.Second); err != nil {
		j.mu.Lock()
		j.cancelSent = false
		j.mu.Unlock()
		c.viol.add("cancel", "cancel %s: %v", j.id, err)
		return
	}
	ep := map[string]int{}
	e := c.epoch()
	for _, n := range c.nodes {
		ep[n.id] = n.disturbance(e)
	}
	j.mu.Lock()
	j.canceledAt, j.cancelEpoch = time.Now(), ep
	j.mu.Unlock()
	c.ev.add("canceled %s (%s): %+v", j.id, jv.State, jv.Counts)
}

// --- faults ---

// runFaults injects faults until end. Faults with a duration run in their
// own goroutines and may overlap; runFaults returns when all are over.
func runFaults(c *chaosCluster, rng *rand.Rand, end time.Time) {
	f := c.cfg.faults
	base := faultRates{}
	if f["net"] {
		base.dropReq, base.dropResp, base.delay, base.maxDelay = 0.03, 0.03, 0.10, 1500*time.Millisecond
	}
	if f["dup"] {
		base.dupReport, base.dupClaim = 0.15, 0.15
	}
	c.proxy.setRates(base)
	var wg sync.WaitGroup
	var storm atomic.Bool
	type fault struct {
		name   string
		weight int
		run    func(r *rand.Rand)
	}
	pickNode := func(r *rand.Rand) *chaosNode { return c.nodes[r.Intn(len(c.nodes))] }
	faults := []fault{
		{"node", 3, func(r *rand.Rand) {
			n := pickNode(r)
			if _, up := n.current(); !up {
				return
			}
			down := time.Duration(500+r.Intn(8000)) * time.Millisecond
			c.ev.add("FAULT node-restart %s (down %s)", n.id, down)
			n.disturb.Add(1)
			n.stopAgent()
			time.Sleep(down)
			if err := n.startAgent(); err != nil {
				c.viol.add("agent-start", "restart %s: %v", n.id, err)
			}
			n.disturb.Add(1)
		}},
		{"hive", 2, func(r *rand.Rand) {
			if !c.isHiveUp() {
				return
			}
			down := time.Duration(r.Intn(5000)) * time.Millisecond
			c.ev.add("FAULT hive-restart (down %s)", down)
			acked := c.proxy.acceptedReports(false)
			c.stopHive()
			time.Sleep(down)
			if err := c.startHive(); err != nil {
				c.viol.add("hive-start", "restart hive: %v", err)
				return
			}
			checkAckedAfterRestart(c, acked)
		}},
		{"crash", 2, func(r *rand.Rand) {
			if !c.isHiveUp() {
				return
			}
			down := time.Duration(r.Intn(4000)) * time.Millisecond
			c.ev.add("FAULT hive-crash (down %s)", down)
			cr, err := c.crashHive(down)
			if err != nil {
				c.viol.add("hive-start", "crash restart: %v", err)
				return
			}
			// An acknowledged cancel survives (it was saved before it was
			// answered): the checker's job-state invariant holds across it.
			c.ev.add("hive came back from state saved %s before the crash (saved %s, crash %s)", cr.at.Sub(cr.persisted).Round(time.Millisecond),
				cr.persisted.Format("15:04:05.000"), cr.at.Format("15:04:05.000"))
		}},
		{"partition", 2, func(r *rand.Rand) {
			n := pickNode(r)
			d := time.Duration(2000+r.Intn(13000)) * time.Millisecond
			c.ev.add("FAULT partition %s for %s", n.id, d)
			n.disturb.Add(1)
			c.proxy.setPartition(n.id, time.Now().Add(d))
			time.Sleep(d)
			c.proxy.setPartition(n.id, time.Time{})
			n.disturb.Add(1)
			c.ev.add("partition %s healed", n.id)
		}},
		{"storm", 1, func(r *rand.Rand) {
			if !storm.CompareAndSwap(false, true) {
				return
			}
			defer storm.Store(false)
			d := time.Duration(3000+r.Intn(5000)) * time.Millisecond
			c.ev.add("FAULT storm for %s", d)
			for _, n := range c.nodes {
				n.disturb.Add(1)
			}
			defer func() {
				for _, n := range c.nodes {
					n.disturb.Add(1)
				}
			}()
			s := base
			s.dropReq, s.dropResp, s.delay = 0.25, 0.25, 0.3
			s.maxDelay = 3 * time.Second
			c.proxy.setRates(s)
			time.Sleep(d)
			c.proxy.setRates(base)
			c.ev.add("storm over")
		}},
		{"replay", 3, func(r *rand.Rand) {
			for i := 0; i < 1+r.Intn(4); i++ {
				c.proxy.replay(r)
			}
		}},
		{"power", 3, func(r *rand.Rand) {
			n := pickNode(r)
			n.mu.Lock()
			busy := n.power != "ac"
			n.mu.Unlock()
			if busy {
				return
			}
			state := []string{"hot", "lowbatt", "battery"}[r.Intn(3)]
			d := time.Duration(4000+r.Intn(12000)) * time.Millisecond
			c.ev.add("FAULT power %s %s for %s", n.id, state, d)
			n.setPower(state)
			time.Sleep(d)
			n.setPower("ac")
			c.ev.add("power %s ac", n.id)
		}},
		{"drain", 2, func(r *rand.Rand) {
			n := pickNode(r)
			d := time.Duration(3000+r.Intn(10000)) * time.Millisecond
			c.ev.add("FAULT drain %s for %s", n.id, d)
			setDrain(c, n, true)
			time.Sleep(d)
			setDrain(c, n, false)
		}},
		{"clock", 1, func(r *rand.Rand) {
			off := time.Duration(r.Intn(4*3600)-2*3600) * time.Second
			if r.Intn(2) == 0 {
				c.ev.add("FAULT hive clock %+v", off)
				c.hiveClockOff.Store(int64(off))
			} else {
				n := pickNode(r)
				c.ev.add("FAULT node clock %s %+v", n.id, off)
				n.clockOff.Store(int64(off))
			}
		}},
	}
	var enabled []fault
	total := 0
	for _, fl := range faults {
		if f[fl.name] {
			enabled = append(enabled, fl)
			total += fl.weight
		}
	}
	// The short run plays each enabled fault once early on, in a seeded
	// order, then draws at random like the soak.
	var first []fault
	if c.cfg.short {
		first = append(first, enabled...)
		rng.Shuffle(len(first), func(i, k int) { first[i], first[k] = first[k], first[i] })
	}
	mean := 2500 * time.Millisecond
	for i := 0; total > 0; i++ {
		gap := time.Duration(rng.ExpFloat64() * float64(mean))
		if i < len(first) {
			gap = time.Duration(1500+rng.Intn(2500)) * time.Millisecond
		}
		if time.Now().Add(gap).After(end) {
			break
		}
		time.Sleep(gap)
		var fl fault
		if i < len(first) {
			fl = first[i]
		} else {
			x := rng.Intn(total)
			for _, cand := range enabled {
				if x < cand.weight {
					fl = cand
					break
				}
				x -= cand.weight
			}
		}
		r := rand.New(rand.NewSource(rng.Int63()))
		wg.Add(1)
		go func() {
			defer wg.Done()
			fl.run(r)
		}()
	}
	if d := time.Until(end); d > 0 {
		time.Sleep(d)
	}
	wg.Wait()
}

func setDrain(c *chaosCluster, n *chaosNode, on bool) {
	if _, err := c.adminRetry(http.MethodPatch, "/api/v1/admin/nodes/"+n.id, proto.NodePatch{Drain: &on}, nil, 60*time.Second); err != nil {
		c.viol.add("drain", "drain %s=%v: %v", n.id, on, err)
	}
}

// restoreAll ends every fault: hive and agents up, no partitions, mains
// power, no drain, clocks right, quarantines cleared.
func restoreAll(c *chaosCluster) {
	if !c.isHiveUp() {
		if err := c.startHive(); err != nil {
			c.viol.add("hive-start", "restart hive: %v", err)
		}
	}
	c.hiveClockOff.Store(0)
	for _, n := range c.nodes {
		c.proxy.setPartition(n.id, time.Time{})
		n.setPower("ac")
		n.clockOff.Store(0)
		if _, up := n.current(); !up {
			n.startAgent()
		}
		setDrain(c, n, false)
	}
	clearQuarantines(c)
}

// clearQuarantines records and clears every node quarantine.
func clearQuarantines(c *chaosCluster) {
	var nodes []proto.NodeView
	if err := c.adminGet("/api/v1/admin/nodes", &nodes); err != nil {
		return
	}
	for _, nv := range nodes {
		if nv.Quarantine != "" {
			c.ev.add("node %s was quarantined: %s", nv.ID, nv.Quarantine)
			c.viol.add("quarantined", "node %s was quarantined during the run: %s", nv.ID, nv.Quarantine)
			c.adminRetry(http.MethodPatch, "/api/v1/admin/nodes/"+nv.ID, proto.NodePatch{ClearQuarantine: true}, nil, 30*time.Second)
		}
	}
}

// checkAckedAfterRestart: what the hive acknowledged before a (graceful)
// restart is still there after it (DESIGN 8.6, 9).
func checkAckedAfterRestart(c *chaosCluster, acked []*reportRec) {
	deadline := time.Now().Add(30 * time.Second)
	for _, r := range acked {
		if r.state != proto.TaskSucceeded {
			continue
		}
		var tv proto.TaskView
		var err error
		for {
			if err = c.adminGet("/api/v1/admin/tasks/"+r.task, &tv); err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			c.viol.add("restart-lost", "task %s (succeeded before the restart): %v", r.task, err)
			continue
		}
		// A success reported for a cancel-requested assignment is accepted
		// with its outputs, but the task stays canceled (DESIGN 8.4).
		want := tv.State == proto.TaskSucceeded
		if j := c.checker.jobByID(tv.JobID); j != nil {
			if _, canceled := j.canceled(); canceled || j.cancelPending() {
				want = tv.State == proto.TaskSucceeded || tv.State == proto.TaskCanceled
			}
		}
		if !want || !sameOutputs(tv.Outputs, r.outputs) {
			c.viol.add("restart-lost", "task %s was acknowledged succeeded (attempt %d) before the hive restart; after it: %s attempt %d outputs %v",
				r.task, r.attempt, tv.State, tv.Attempt, tv.Outputs)
		}
	}
}

// waitSettled waits until every job is terminal and every agent and the
// hive hold nothing.
func waitSettled(c *chaosCluster, ck *chaosChecker, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		var jobs []proto.JobView
		if err := c.adminGet("/api/v1/admin/jobs?limit=1000", &jobs); err != nil {
			continue
		}
		views := map[string]proto.JobView{}
		for _, jv := range jobs {
			views[jv.ID] = jv
		}
		done := true
		for _, j := range ck.jobList() {
			if !views[j.id].State.Terminal() {
				done = false
			}
		}
		for _, n := range c.nodes {
			a, up := n.current()
			if !up {
				done = false
				continue
			}
			if _, _, _, _, link := a.chaosCapacity(); link != proto.LinkConnected || len(a.chaosHeld()) > 0 {
				done = false
			}
		}
		var nodes []proto.NodeView
		if err := c.adminGet("/api/v1/admin/nodes", &nodes); err != nil {
			continue
		}
		for _, nv := range nodes {
			if nv.Allocated != (proto.Resources{}) || len(nv.RunningTasks) > 0 || nv.Liveness != proto.NodeOnline {
				done = false
			}
		}
		if done {
			return true
		}
	}
	return false
}

// finalCapacityChecks: idle nodes have all their capacity back, on both
// sides, and no task process is left.
func finalCapacityChecks(c *chaosCluster, settled bool) {
	v := c.viol
	if !settled {
		v.add("not-settled", "the swarm did not settle within %s after the faults stopped", c.cfg.quiesce)
	}
	var nodes []proto.NodeView
	if err := c.adminGet("/api/v1/admin/nodes", &nodes); err != nil {
		v.add("final", "nodes: %v", err)
	}
	for _, nv := range nodes {
		if nv.Allocated != (proto.Resources{}) || len(nv.RunningTasks) > 0 {
			v.add("capacity-leak", "hive: idle node %s has allocated %+v, running %v", nv.ID, nv.Allocated, nv.RunningTasks)
		}
		if nv.Liveness != proto.NodeOnline {
			v.add("final-offline", "node %s is %s at the end", nv.ID, nv.Liveness)
		}
	}
	for _, n := range c.nodes {
		a, up := n.current()
		if !up {
			continue
		}
		if held := a.chaosHeld(); len(held) > 0 {
			v.add("capacity-leak", "node %s still holds %+v", n.id, held)
		}
		total, free, freeSlots, slots, _ := a.chaosCapacity()
		if free != total || freeSlots != slots {
			v.add("capacity-leak", "node %s: free %+v of %+v, %d of %d slots free", n.id, free, total, freeSlots, slots)
		}
	}
	procs := taskProcs()
	for _, j := range ckJobs(c) {
		if pids := procs[j]; len(pids) > 0 {
			v.add("task-procs-left", "task processes of job %s still running at the end: %v", j, pids)
		}
	}
}

func ckJobs(c *chaosCluster) []string {
	var out []string
	for _, j := range c.checker.jobList() {
		out = append(out, j.id)
	}
	return out
}

// --- leak checks ---

func countFDs() int {
	ents, _ := os.ReadDir("/proc/self/fd")
	return len(ents)
}

func openFDs() []string {
	ents, _ := os.ReadDir("/proc/self/fd")
	var out []string
	for _, e := range ents {
		l, _ := os.Readlink("/proc/self/fd/" + e.Name())
		out = append(out, e.Name()+"->"+l)
	}
	return out
}

// settleCounts returns the goroutine and FD counts once they stop falling
// to the targets (or after wait).
func settleCounts(gWant, fdWant int, wait time.Duration) (int, int) {
	deadline := time.Now().Add(wait)
	for {
		runtime.GC()
		g, fd := runtime.NumGoroutine(), countFDs()
		if (g <= gWant+2 && fd <= fdWant+2) || time.Now().After(deadline) {
			return g, fd
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func goroutineDump() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}

// checkAgentGoroutines: once every agent stopped, none of their goroutines
// (node agent, runner) may be left.
func checkAgentGoroutines(c *chaosCluster) {
	var left []string
	deadline := time.Now().Add(20 * time.Second)
	for {
		left = agentGoroutines()
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(left) > 0 {
		c.viol.add("agent-goroutine-leak", "%d agent goroutines still run after every agent stopped:\n%s", len(left), strings.Join(left, "\n\n"))
	}
}

func agentGoroutines() []string {
	var out []string
	for _, g := range strings.Split(goroutineDump(), "\n\n") {
		if strings.Contains(g, "_test.go") {
			continue // the harness itself
		}
		if strings.Contains(g, "ewastesavior/internal/node.") || strings.Contains(g, "ewastesavior/internal/runner.") {
			out = append(out, g)
		}
	}
	return out
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return fi.Size()
}

func ioCopy(w io.Writer, r io.Reader) (int64, error) { return io.Copy(w, r) }
