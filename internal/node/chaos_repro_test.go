//go:build linux

package node

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Focused reproductions of bugs the chaos soak found, kept as regression
// tests. Each one is small and deterministic and failed before its fix:
//
//	go test ./internal/node/ -run 'TestChaosRepro' -count=1 -v

// reproCluster is a chaos cluster without faults, with n nodes (agents
// not started) and its own uid range.
func reproCluster(t *testing.T, nodes int) *chaosCluster {
	base := 30000
	if v := os.Getenv("SAVIOR_CHAOS_UIDBASE"); v != "" {
		base, _ = strconv.Atoi(v)
	}
	cfg := chaosConfig{seed: 1, nodes: nodes, uidBase: base, faults: map[string]bool{}, logDir: os.Getenv("SAVIOR_CHAOS_LOGDIR")}
	c := newChaosCluster(t, cfg)
	c.checker = newChaosChecker(c)
	t.Cleanup(func() {
		for _, n := range c.nodes {
			n.stopAgent()
		}
		c.stopHive()
		c.proxy.close()
		c.closeLogs()
		c.ev.close()
	})
	return c
}

func (c *chaosCluster) waitConnected(t *testing.T, nodes ...*chaosNode) {
	t.Helper()
	waitFor(t, "nodes connected", 60*time.Second, func() bool {
		for _, n := range nodes {
			a, up := n.current()
			if !up {
				return false
			}
			if _, _, _, _, link := a.chaosCapacity(); link != proto.LinkConnected {
				return false
			}
		}
		return true
	})
}

func (c *chaosCluster) tasksOf(t *testing.T, job string) []proto.TaskView {
	t.Helper()
	var page proto.TaskPage
	if err := c.adminGet("/api/v1/admin/jobs/"+job+"/tasks?limit=1000", &page); err != nil {
		t.Fatal(err)
	}
	return page.Tasks
}

func (c *chaosCluster) jobState(job string) proto.JobState {
	var jv proto.JobDetail
	if err := c.adminGet("/api/v1/admin/jobs/"+job, &jv); err != nil {
		return ""
	}
	return jv.State
}

// reproJob is a job of count normal chaos tasks (verifiable outputs).
func reproJob(name string, count int, input []byte) *chaosJob {
	j := &chaosJob{name: name, timeoutS: 600, cores: 0.5, memMB: 16}
	for i := 0; i < count; i++ {
		j.kinds = append(j.kinds, kindNormal)
		j.durs = append(j.durs, "0.5")
	}
	if input != nil {
		j.input, j.inputBlob = input, sha256hex(input)
	}
	return j
}

// dropFirst drops the first n requests of a kind from a node.
func dropFirst(kind, node string, n int) func(string, string, *http.Request) bool {
	var mu sync.Mutex
	return func(k, nd string, _ *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if k == kind && nd == node && n > 0 {
			n--
			return true
		}
		return false
	}
}

// Chaos "quarantined", "output" node errors: a single lost blob PUT while a
// task uploaded its outputs failed the whole task as an `output` node error
// (one upload attempt), which the hive booked against the node
// (FailedNodes, NodeErrors, quarantine evidence) before running the task
// again. A transient transfer error is retried (transfer.retry), and the
// task succeeds on its first attempt.
func TestChaosReproUploadBlipFailsTask(t *testing.T) {
	c := reproCluster(t, 1)
	a := c.nodes[0]
	c.proxy.setDropIf(dropFirst("blobput", a.id, 1))
	a.startAgent()
	c.waitConnected(t, a)
	j := reproJob("blip", 1, nil)
	if err := submitChaosJob(c, c.checker, j); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job done", 60*time.Second, func() bool { return c.jobState(j.id).Terminal() })
	tv := c.tasksOf(t, j.id)[0]
	if tv.State != proto.TaskSucceeded || tv.Attempt != 1 || tv.NodeErrors != 0 {
		t.Fatalf("one dropped upload request: task %s attempt %d node_errors %d failed_nodes %v history %s",
			tv.State, tv.Attempt, tv.NodeErrors, tv.FailedNodes, historyString(tv.History))
	}
}

// Same for inputs: a lost blob GET failed the task as an `input` node error
// (one attempt). net/http itself sends a GET whose reused keep-alive
// connection dies before any response byte once more, so this drops two
// in a row (the request and that automatic retry).
func TestChaosReproInputBlipFailsTask(t *testing.T) {
	c := reproCluster(t, 1)
	a := c.nodes[0]
	input := []byte("repro-input")
	if _, err := c.adminRetry(http.MethodPut, "/api/v1/blobs/"+sha256hex(input), rawBody(input), nil, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	c.proxy.setDropIf(dropFirst("blobget", a.id, 2))
	a.startAgent()
	c.waitConnected(t, a)
	j := reproJob("inblip", 1, input)
	if err := submitChaosJob(c, c.checker, j); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job done", 60*time.Second, func() bool { return c.jobState(j.id).Terminal() })
	tv := c.tasksOf(t, j.id)[0]
	if tv.State != proto.TaskSucceeded || tv.Attempt != 1 || tv.NodeErrors != 0 {
		t.Fatalf("one dropped input download: task %s attempt %d node_errors %d failed_nodes %v history %s",
			tv.State, tv.Attempt, tv.NodeErrors, tv.FailedNodes, historyString(tv.History))
	}
}

// A hive restart while a task uploads: the upload reaches the new hive
// with the old node token and gets 401. The task failed as an `output` node
// error although the node re-registers within a second and the hive
// re-adopts the task; now the upload waits for the new session and is
// sent again.
func TestChaosReproHiveRestartDuringUpload(t *testing.T) {
	c := reproCluster(t, 1)
	a := c.nodes[0]
	var once sync.Once
	c.proxy.setDropIf(func(kind, node string, _ *http.Request) bool {
		if kind == "blobput" && node == a.id {
			once.Do(func() {
				c.stopHive()
				if err := c.startHive(); err != nil {
					t.Error(err)
				}
			})
		}
		return false
	})
	a.startAgent()
	c.waitConnected(t, a)
	j := reproJob("restartup", 1, nil)
	if err := submitChaosJob(c, c.checker, j); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job done", 90*time.Second, func() bool { return c.jobState(j.id).Terminal() })
	tv := c.tasksOf(t, j.id)[0]
	if tv.State != proto.TaskSucceeded || tv.Attempt != 1 || tv.NodeErrors != 0 {
		t.Fatalf("hive restart during the upload: task %s attempt %d node_errors %d history %s",
			tv.State, tv.Attempt, tv.NodeErrors, historyString(tv.History))
	}
}

// The consequence: three such blips on one node, on tasks that then
// succeeded elsewhere, quarantined a healthy node (DESIGN 8.3 quarantine
// rule), which then got no work until an admin cleared it. Now a network
// outage while three tasks upload only delays them: once it is over they
// succeed where they ran, on their first attempt, with no node errors.
func TestChaosReproUploadBlipsQuarantineNode(t *testing.T) {
	c := reproCluster(t, 1)
	a := c.nodes[0]
	var mu sync.Mutex
	outage := true // node A's uploads fail during a network outage
	dropped := 0
	c.proxy.setDropIf(func(kind, node string, _ *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if outage && kind == "blobput" && node == a.id {
			dropped++
			return true
		}
		return false
	})
	a.startAgent()
	c.waitConnected(t, a)
	j := reproJob("quar", 3, nil)
	if err := submitChaosJob(c, c.checker, j); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "two lost uploads per task", 60*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return dropped >= 6
	})
	mu.Lock()
	outage = false
	mu.Unlock()
	waitFor(t, "job done", 90*time.Second, func() bool { return c.jobState(j.id).Terminal() })
	var nv proto.NodeView
	if err := c.adminGet("/api/v1/admin/nodes/"+a.id, &nv); err != nil {
		t.Fatal(err)
	}
	for _, tv := range c.tasksOf(t, j.id) {
		t.Logf("task %d: %s %s", tv.Index, tv.State, historyString(tv.History))
		if tv.State != proto.TaskSucceeded || tv.Attempt != 1 || tv.NodeErrors != 0 {
			t.Errorf("task %d after a network outage during its upload: %s attempt %d node_errors %d", tv.Index, tv.State, tv.Attempt, tv.NodeErrors)
		}
	}
	if nv.Quarantine != "" {
		t.Fatalf("node %s was quarantined by a network outage during uploads: %s", a.id, nv.Quarantine)
	}
}

// Chaos "agent-goroutine-leak": a task's log shipper outlived the task and
// the agent. runTask starts logStream.run with the task's own context,
// which nothing canceled once the final report was acknowledged, and run
// only returns when that context ends or the buffer drains. If the last
// log chunk didn't get through before the report did, run kept trying
// every 2 s, still after Agent.Run returned, with whatever client a.hc
// last held. A stopped agent leaves no goroutines and sends nothing more.
func TestChaosReproLogStreamOutlivesAgent(t *testing.T) {
	c := reproCluster(t, 1)
	a := c.nodes[0]
	var mu sync.Mutex
	var stopped bool
	posts := 0 // log uploads by the agent after it stopped
	c.proxy.setDropIf(func(kind, node string, _ *http.Request) bool {
		if kind != "log" {
			return false
		}
		mu.Lock()
		if stopped {
			posts++
		}
		mu.Unlock()
		return true // the log never gets through; reports do
	})
	a.startAgent()
	c.waitConnected(t, a)
	j := reproJob("logleak", 1, nil)
	if err := submitChaosJob(c, c.checker, j); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job succeeded", 60*time.Second, func() bool { return c.jobState(j.id) == proto.JobSucceeded })
	a.stopAgent()
	mu.Lock()
	stopped = true
	mu.Unlock()
	time.Sleep(5 * time.Second) // the shipper posts every 2 s
	left := agentGoroutines()
	mu.Lock()
	n := posts
	mu.Unlock()
	if len(left) > 0 || n > 0 {
		t.Fatalf("5 s after the agent stopped: %d log uploads sent, %d agent goroutines left:\n%s", n, len(left), strings.Join(left, "\n\n"))
	}
}

// Chaos crash mode, "job-state ... after its cancel was acknowledged": POST
// jobs/{id}/cancel answered before the cancel was saved. A hive machine
// that lost power within the next persist interval (2 s, 30 s with
// hive_data on vfat or in RAM) came back with the job running, re-adopted
// its tasks and ran the job to the end. An acknowledged cancel is on disk
// and survives a crash.
func TestChaosReproCancelLostInCrash(t *testing.T) {
	c := reproCluster(t, 1)
	a := c.nodes[0]
	a.startAgent()
	c.waitConnected(t, a)
	j := reproJob("cancrash", 2, nil)
	j.kinds = []byte{kindLong, kindLong}
	j.durs = []string{"30", "30"}
	if err := submitChaosJob(c, c.checker, j); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running", 30*time.Second, func() bool {
		var jv proto.JobDetail
		return c.adminGet("/api/v1/admin/jobs/"+j.id, &jv) == nil && jv.Counts.Running == 2
	})
	time.Sleep(2500 * time.Millisecond) // let the running state be saved
	cancelChaosJob(c, c.checker, j)
	if st := c.jobState(j.id); st != proto.JobCanceled {
		t.Fatalf("job %s after cancel", st)
	}
	// The machine loses power right away (state.json as it is on disk).
	b, _ := os.ReadFile(c.hiveDir + "/state.json")
	if !strings.Contains(string(b), `"canceled":true`) {
		t.Errorf("the cancel was answered before it was saved")
	}
	if _, err := c.crashHive(0); err != nil {
		t.Fatal(err)
	}
	var jv proto.JobDetail
	waitFor(t, "hive back", 30*time.Second, func() bool { return c.adminGet("/api/v1/admin/jobs/"+j.id, &jv) == nil })
	if jv.State != proto.JobCanceled {
		time.Sleep(5 * time.Second)
		c.adminGet("/api/v1/admin/jobs/"+j.id, &jv)
		t.Fatalf("acknowledged cancel lost in a crash: job is %s %+v after the restart", jv.State, jv.Counts)
	}
}

// Crash + attempt reuse: a crash that loses a dispatch makes the hive hand
// out the same attempt number again (DESIGN 8.6 restores Attempt from the
// saved state). The runner names the task directory <task_id>.<attempt>
// and creates it with Mkdir (it must not exist, DESIGN 12 step 1). The
// re-dispatch reached the node that still tore down the lost assignment of
// that attempt, and the new run failed with an `internal` node error. Now
// the hive waits until the node stops listing the old lease (DESIGN 8.2).
func TestChaosReproCrashReusesAttemptWorkdir(t *testing.T) {
	c := reproCluster(t, 1)
	a := c.nodes[0]
	a.startAgent()
	c.waitConnected(t, a)
	retries := 1
	// Attempt 1 fails after 3 s (long enough for the drain below to land
	// while it runs, also on slow builds); later attempts run for a while,
	// with a child that left the process group still holding the output
	// pipe (a task that starts a daemon), so tearing one down takes the 2 s
	// drain grace.
	spec := proto.JobSpec{
		Name:         "reuse",
		Script:       "if [ \"$SAVIOR_ATTEMPT\" = 1 ]; then sleep 3; exit 3; fi\nsetsid sleep 60 &\nsleep 30\necho ok > out.txt\n",
		Outputs:      []string{"out.txt"},
		Resources:    proto.Resources{Cores: 0.5, MemMB: 16, DiskMB: 4},
		Requirements: proto.Requirements{Isolation: proto.IsolationAny},
		TimeoutS:     600,
		Retries:      &retries,
		Count:        1,
	}
	var jd proto.JobDetail
	if _, err := c.adminRetry(http.MethodPost, "/api/v1/admin/jobs", spec, &jd, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	task := func() proto.TaskView {
		ts := c.tasksOf(t, jd.ID)
		if len(ts) == 0 {
			return proto.TaskView{}
		}
		return ts[0]
	}
	waitFor(t, "attempt 1 running", 30*time.Second, func() bool { return task().State == proto.TaskRunning })
	setDrain(c, a, true) // hold attempt 2 back until the requeue is saved
	waitFor(t, "attempt 1 failed", 30*time.Second, func() bool { tv := task(); return tv.Failures == 1 && tv.State == proto.TaskPending })
	setDrain(c, a, false) // saved synchronously, with the task pending
	waitFor(t, "attempt 2 running", 30*time.Second, func() bool { tv := task(); return tv.Attempt == 2 && tv.State == proto.TaskRunning })
	if _, err := c.crashHive(0); err != nil { // before attempt 2 is saved
		t.Fatal(err)
	}
	waitFor(t, "attempt 2 again", 60*time.Second, func() bool {
		tv := task()
		return tv.Attempt >= 2 && len(tv.History) >= 2 && (tv.State == proto.TaskRunning || tv.NodeErrors > 0)
	})
	time.Sleep(3 * time.Second)
	tv := task()
	t.Logf("task: %s attempt %d node_errors %d history %s", tv.State, tv.Attempt, tv.NodeErrors, historyString(tv.History))
	if tv.NodeErrors > 0 {
		t.Fatalf("the re-dispatched attempt failed on the node: %s", historyString(tv.History))
	}
}

// Node requests with a token the hive no longer knows (it restarted; the
// node has not re-registered yet) on node-or-admin endpoints (blob GET and
// PUT, render, stats) were judged as admin logins: "invalid or expired
// admin credentials", and a failed admin login counted for the source
// address. Five within a minute locked that address out of the admin API
// for 60 s, the admin's own valid token included. On a SaviorOS hive the
// hive machine's own node agent shares the address with local admin tools,
// and nodes behind one NAT share one. An unknown node token is answered
// 401 "register again" without counting against admin logins.
func TestChaosReproStaleNodeTokenLocksOutAdmin(t *testing.T) {
	c := reproCluster(t, 1)
	a := c.nodes[0]
	a.startAgent()
	c.waitConnected(t, a)
	c.proxy.mu.Lock()
	var tok string
	for k, id := range c.proxy.tokens {
		if id == a.id {
			tok = k
		}
	}
	c.proxy.mu.Unlock()
	a.stopAgent() // keep the agent from re-registering: only its old token is in play
	c.stopHive()
	if err := c.startHive(); err != nil {
		t.Fatal(err)
	}
	sum := sha256hex([]byte("x"))
	var last string
	for i := 0; i < 5; i++ { // e.g. output uploads finishing right after the restart
		req, _ := http.NewRequest(http.MethodPut, "https://"+c.addr+"/api/v1/blobs/"+sum, strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 200)
		n, _ := resp.Body.Read(b)
		resp.Body.Close()
		last = fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(b[:n])))
	}
	t.Logf("node PUT with its old token: %s", last)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, body, err := c.adminDo(ctx, http.MethodGet, "/api/v1/admin/info", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK {
		t.Fatalf("admin with the right token, same address: %d %s", code, strings.TrimSpace(string(body)))
	}
}

// An agent that restarts quickly (a crash and a respawn by init) was
// refused as a duplicate: its BootID carries a new per-process nonce, the
// hive still counted the old process as online and answered 409
// "duplicate node id" (DESIGN 7.1), and the agent then slept 30 s before
// it tried again. Its machine did no work meanwhile, and its old tasks were
// only requeued once the hive noticed the old process was gone. The same
// boot of the same machine (same kernel boot_id) re-registering replaces
// its old session at once.
func TestChaosReproAgentRespawnLockedOut(t *testing.T) {
	c := reproCluster(t, 1)
	a := c.nodes[0]
	a.startAgent()
	c.waitConnected(t, a)
	a.stopAgent()
	t0 := time.Now()
	a.startAgent()
	c.waitConnected(t, a)
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("a respawned agent took %s to rejoin (a 409 duplicate and a 30 s back-off)", d.Round(100*time.Millisecond))
	}
}
