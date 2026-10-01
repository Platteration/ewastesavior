//go:build linux

package node

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// --- workload ---

// Task kinds, one letter per index in a job's KINDS variable.
const (
	kindNormal   = 'n' // sleeps briefly, writes its outputs, exits 0
	kindFlaky    = 'f' // exits 3 on attempt 1, then behaves like n
	kindFail     = 'x' // always exits 7
	kindLong     = 'l' // sleeps longer (exposed to every fault), then like n
	kindTime     = 't' // sleeps far past timeout_s
	kindVeryLong = 'L' // sleeps 120 s; only in jobs that get canceled
)

// chaosScript runs every chaos task. The outputs depend only on the job
// name, the task index and the attempt, so the checker can recompute them.
const chaosScript = `set -u
i=$SAVIOR_TASK_INDEX
a=$SAVIOR_ATTEMPT
k=$(printf '%s' "$KINDS" | cut -c$((i+1)))
set -- $DURS
shift $i
d=$1
echo "chaos start $TAG index=$i attempt=$a kind=$k"
if [ -n "${INPUT_WANT:-}" ]; then
  [ "$(cat in.dat)" = "$INPUT_WANT" ] || { echo "chaos: wrong input"; exit 9; }
fi
if [ "$k" = t ]; then sleep 600; exit 0; fi
if [ "$k" = L ]; then d=120; fi
sleep "$d"
if [ "$k" = x ]; then echo "chaos: failing on purpose"; exit 7; fi
if [ "$k" = f ] && [ "$a" = 1 ]; then echo "chaos: flaky first attempt"; exit 3; fi
mkdir -p data
printf 'chaos %s index=%s attempt=%s\n' "$TAG" "$i" "$a" > out.txt
n=$(( (i * 37) % 50 + 1 ))
j=0
while [ $j -lt $n ]; do printf '%s:%s:%s\n' "$TAG" "$i" "$j"; j=$((j+1)); done > "data/part-$i.bin"
echo "chaos done $TAG index=$i attempt=$a"
`

type chaosJob struct {
	name      string
	kinds     []byte
	durs      []string
	timeoutS  int
	priority  int
	cancelIn  time.Duration // 0 = never canceled
	input     []byte
	inputBlob string
	cores     float64
	memMB     int
	pinTo     string // node ID the job requires (Requirements.Nodes), "" = any

	mu          sync.Mutex
	id          string
	submittedAt time.Time
	canceledAt  time.Time      // when the cancel was acknowledged
	cancelEpoch map[string]int // node -> its disturbance epoch at the cancel
	cancelSent  bool           // a cancel request is on its way
}

func (j *chaosJob) count() int { return len(j.kinds) }

func (j *chaosJob) canceled() (time.Time, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.canceledAt, !j.canceledAt.IsZero()
}

// cancelPending reports whether a cancel was sent and not answered yet:
// the hive may show the job canceled already (it saves the cancel before
// it answers).
func (j *chaosJob) cancelPending() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelSent && j.canceledAt.IsZero()
}

func (j *chaosJob) spec() proto.JobSpec {
	retries := 1
	env := map[string]string{"TAG": j.name, "KINDS": string(j.kinds), "DURS": strings.Join(j.durs, " ")}
	var inputs []proto.Input
	if j.inputBlob != "" {
		env["INPUT_WANT"] = string(j.input)
		inputs = []proto.Input{{Name: "in.dat", Blob: j.inputBlob}}
	}
	return proto.JobSpec{
		Name:         j.name,
		Script:       chaosScript,
		Env:          env,
		Inputs:       inputs,
		Outputs:      []string{"out.txt", "data/*.bin"},
		Resources:    proto.Resources{Cores: j.cores, MemMB: j.memMB, DiskMB: 4},
		Requirements: j.requirements(),
		TimeoutS:     j.timeoutS,
		Retries:      &retries,
		Count:        j.count(),
		Priority:     j.priority,
	}
}

func (j *chaosJob) requirements() proto.Requirements {
	r := proto.Requirements{Isolation: proto.IsolationAny}
	if j.pinTo != "" {
		r.Nodes = []string{j.pinTo}
	}
	return r
}

// expectedOutputs are the outputs of task index after it ran as attempt.
func (j *chaosJob) expectedOutputs(index, attempt int) map[string][]byte {
	out := map[string][]byte{"out.txt": []byte(fmt.Sprintf("chaos %s index=%d attempt=%d\n", j.name, index, attempt))}
	var b strings.Builder
	for k := 0; k < (index*37)%50+1; k++ {
		fmt.Fprintf(&b, "%s:%d:%d\n", j.name, index, k)
	}
	out[fmt.Sprintf("data/part-%d.bin", index)] = []byte(b.String())
	return out
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// genJob draws one job. Timeout jobs mix short tasks with ones that run
// into timeout_s; the others mix normal, flaky, failing and long tasks.
// Some jobs need a whole node (2 cores: the reservation path, DESIGN 8.2)
// or one particular node.
func genJob(rng *rand.Rand, seq int, short bool, nodes int) *chaosJob {
	j := &chaosJob{name: fmt.Sprintf("chaos%03d", seq), timeoutS: 600}
	j.priority = []int{-10, 0, 0, 0, 10, 50}[rng.Intn(6)]
	j.cores = []float64{0.25, 0.5, 0.5}[rng.Intn(3)]
	j.memMB = 16 + 8*rng.Intn(3)
	n := 4 + rng.Intn(12)
	if short {
		n = 3 + rng.Intn(8)
	}
	longMax := 30.0
	if short {
		longMax = 10
	}
	timeoutJob := rng.Intn(6) == 0
	if timeoutJob {
		j.timeoutS = 6
	}
	for i := 0; i < n; i++ {
		var k byte
		d := 0.2 + rng.Float64()*2.3
		switch r := rng.Intn(100); {
		case timeoutJob && r < 30:
			k = kindTime
		case timeoutJob:
			k, d = kindNormal, 0.2+rng.Float64()*1.5
		case r < 55:
			k = kindNormal
		case r < 72:
			k = kindFlaky
		case r < 80:
			k = kindFail
		default:
			k = kindLong
			d = 5 + rng.Float64()*(longMax-5)
		}
		j.kinds = append(j.kinds, k)
		j.durs = append(j.durs, strconv.FormatFloat(d, 'f', 2, 64))
	}
	switch rng.Intn(10) {
	case 0:
		j.cores, j.memMB = 2, 64 // the fixture node's every core
	case 1:
		j.pinTo = fmt.Sprintf("chaos-%d", rng.Intn(nodes))
	}
	if rng.Intn(5) == 0 {
		j.makeCancelTarget(rng)
	}
	return j
}

// makeCancelTarget turns j into a job that gets canceled while it runs:
// first in the queue, with tasks that would run on for minutes, so a cancel
// that doesn't reach the nodes shows.
func (j *chaosJob) makeCancelTarget(rng *rand.Rand) {
	j.cancelIn = time.Duration(2+rng.Intn(8)) * time.Second
	j.priority = 100
	for i := range j.kinds {
		if j.kinds[i] == kindLong || i == 0 {
			j.kinds[i] = kindVeryLong
		}
	}
}

// genTimeoutJob draws a job with timeout_s = 6 where some tasks time out.
func genTimeoutJob(rng *rand.Rand, seq int) *chaosJob {
	j := &chaosJob{name: fmt.Sprintf("chaos%03d", seq), timeoutS: 6, cores: 0.25, memMB: 16}
	for i := 0; i < 4+rng.Intn(4); i++ {
		k, d := byte(kindNormal), 0.2+rng.Float64()*1.5
		if i%2 == 1 {
			k = kindTime
		}
		j.kinds = append(j.kinds, k)
		j.durs = append(j.durs, strconv.FormatFloat(d, 'f', 2, 64))
	}
	return j
}

// --- checker ---

// chaosChecker polls the hive's admin API and the agents and checks the
// invariants continuously.
type chaosChecker struct {
	c *chaosCluster

	mu         sync.Mutex
	jobs       []*chaosJob
	byID       map[string]*chaosJob
	tasks      map[string]*taskTrack
	finalOK    map[string]int // job ID -> hive epoch its final state was last verified in
	mismatch   map[string]int // job ID -> consecutive stable count mismatches
	incons     map[string]*inconsistency
	views      map[string]proto.TaskView // latest view of every task
	rounds     int
	lastOK     time.Time
	cancelStop map[string]time.Duration // job -> how long its tasks ran on after the cancel
	crashEdges []time.Time              // a crashed hive may have lost what happened after these
	crashAts   []time.Time              // ... and before these
	softFail   map[string]string        // task -> why it failed although it was meant to succeed
}

type taskTrack struct {
	job        *chaosJob
	index      int
	terminal   proto.TaskState
	terminalAt time.Time // when it was first seen terminal
	attempt    int
	outputs    []proto.Output
	firstAt    time.Time
}

// forgetAfter: after a crash, tasks that became terminal after the state
// the hive came back from may run again.
func (ck *chaosChecker) forgetAfter(cr chaosCrash) {
	edge := cr.persisted.Add(-time.Second)
	ck.mu.Lock()
	defer ck.mu.Unlock()
	for _, tr := range ck.tasks {
		if tr.terminal != "" && tr.terminalAt.After(edge) {
			tr.terminal, tr.attempt, tr.outputs = "", 0, nil
		}
	}
	ck.crashEdges = append(ck.crashEdges, edge)
	ck.crashAts = append(ck.crashAts, cr.at)
}

// inconsistency is a disagreement between a node and the hive that must
// not outlive the convergence bound.
type inconsistency struct {
	since time.Time
	epoch int // node or hive disturbance epoch it was seen in
	what  string
}

func newChaosChecker(c *chaosCluster) *chaosChecker {
	return &chaosChecker{c: c, byID: map[string]*chaosJob{}, tasks: map[string]*taskTrack{}, finalOK: map[string]int{},
		mismatch: map[string]int{}, incons: map[string]*inconsistency{}, views: map[string]proto.TaskView{}, softFail: map[string]string{},
		cancelStop: map[string]time.Duration{}}
}

func (ck *chaosChecker) addJob(j *chaosJob) {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	ck.jobs = append(ck.jobs, j)
	ck.byID[j.id] = j
}

func (ck *chaosChecker) jobByID(id string) *chaosJob {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	return ck.byID[id]
}

func (ck *chaosChecker) jobList() []*chaosJob {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	return append([]*chaosJob(nil), ck.jobs...)
}

const maxAttempts = 1 + 1 + proto.MaxInterruptions // retries = 1

// round runs one pass. all forces every job's tasks to be fetched.
func (ck *chaosChecker) round(all bool) error {
	c := ck.c
	epoch := c.epoch()
	start := time.Now()
	var jobs1, jobs2 []proto.JobView
	if err := c.adminGet("/api/v1/admin/jobs?limit=1000", &jobs1); err != nil {
		return err
	}
	pages := map[string]proto.TaskPage{}
	ck.mu.Lock()
	ck.rounds++
	round := ck.rounds
	var want []*chaosJob
	for _, j := range ck.jobs {
		if all || ck.finalOK[j.id] != epoch || round%10 == 0 {
			want = append(want, j)
		}
	}
	ck.mu.Unlock()
	for _, j := range want {
		var page proto.TaskPage
		if err := c.adminGet("/api/v1/admin/jobs/"+j.id+"/tasks?limit=1000", &page); err != nil {
			return err
		}
		pages[j.id] = page
	}
	if err := c.adminGet("/api/v1/admin/jobs?limit=1000", &jobs2); err != nil {
		return err
	}
	var nodes []proto.NodeView
	if err := c.adminGet("/api/v1/admin/nodes", &nodes); err != nil {
		return err
	}
	if c.epoch() != epoch {
		return fmt.Errorf("hive restarted during the round")
	}
	ck.analyze(jobs1, jobs2, pages, nodes, epoch, start)
	ck.checkNodes(nodes, epoch)
	ck.mu.Lock()
	ck.lastOK = time.Now()
	ck.mu.Unlock()
	return nil
}

func countPage(page proto.TaskPage, canceled bool) proto.TaskCounts {
	var c proto.TaskCounts
	for _, t := range page.Tasks {
		switch t.State {
		case proto.TaskPending:
			c.Pending++
		case proto.TaskAssigned:
			c.Assigned++
		case proto.TaskRunning:
			c.Running++
		case proto.TaskSucceeded:
			c.Succeeded++
		case proto.TaskFailed:
			c.Failed++
		case proto.TaskCanceled:
			c.Canceled++
		}
	}
	if canceled {
		c.Canceled += page.Undispatched
	} else {
		c.Pending += page.Undispatched
	}
	return c
}

func sumCounts(c proto.TaskCounts) int {
	return c.Pending + c.Assigned + c.Running + c.Succeeded + c.Failed + c.Canceled
}

// analyze checks one round; start is when its first request was sent (a
// cancel acknowledged later may not show in it).
func (ck *chaosChecker) analyze(jobs1, jobs2 []proto.JobView, pages map[string]proto.TaskPage, nodes []proto.NodeView, epoch int, start time.Time) {
	v := ck.c.viol
	before := map[string]proto.JobView{}
	for _, jv := range jobs1 {
		before[jv.ID] = jv
	}
	after := map[string]proto.JobView{}
	for _, jv := range jobs2 {
		after[jv.ID] = jv
	}
	for _, j := range ck.jobList() {
		jv, ok := after[j.id]
		if !ok {
			v.add("job-lost", "job %s (%s), acknowledged at %s, is gone from the hive", j.id, j.name, j.submittedAt.Format(time.TimeOnly))
			continue
		}
		at, canceled := j.canceled()
		if canceled && !at.Before(start) || j.cancelPending() {
			continue // canceled while this round was under way
		}
		ck.checkJobView(j, jv, canceled)
		page, ok := pages[j.id]
		if !ok {
			continue
		}
		if page.Total+page.Undispatched != j.count() || page.NextOffset != 0 {
			v.add("task-page", "job %s: %d records + %d undispatched != count %d (next_offset %d)",
				j.id, page.Total, page.Undispatched, j.count(), page.NextOffset)
		}
		// Job counts against task states, from two job listings that agree
		// around the task page (DESIGN 8.1: maintained counters).
		if b, ok := before[j.id]; ok && b.Counts == jv.Counts {
			got := countPage(page, canceled && jv.State == proto.JobCanceled)
			ck.mu.Lock()
			if got != jv.Counts {
				ck.mismatch[j.id]++
				if ck.mismatch[j.id] == 3 {
					v.add("job-counts", "job %s: JobView counts %+v but its tasks are %+v (3 stable rounds)", j.id, jv.Counts, got)
				}
			} else {
				ck.mismatch[j.id] = 0
			}
			ck.mu.Unlock()
		}
		ck.dropVanished(j, page)
		final := jv.State.Terminal()
		for _, tv := range page.Tasks {
			if !ck.checkTask(j, tv, canceled, epoch) {
				final = false
			}
			if !tv.State.Terminal() {
				final = false
			}
		}
		if final {
			ck.mu.Lock()
			ck.finalOK[j.id] = epoch
			ck.mu.Unlock()
		}
	}
}

// dropVanished forgets tasks of j that are no longer in its task list.
// Task records are never deleted while their job exists, except by a crash:
// a record created (at first dispatch) after the state the hive came back
// from was saved is gone, and the index is dispatched again under a new
// task ID.
func (ck *chaosChecker) dropVanished(j *chaosJob, page proto.TaskPage) {
	present := map[string]bool{}
	for _, tv := range page.Tasks {
		present[tv.ID] = true
	}
	ck.mu.Lock()
	defer ck.mu.Unlock()
	for id, tr := range ck.tasks {
		if tr.job != j || present[id] {
			continue
		}
		explained := false
		for i, edge := range ck.crashEdges {
			if tr.firstAt.After(edge) && tr.firstAt.Before(ck.crashAts[i]) {
				explained = true
			}
		}
		if !explained {
			ck.c.viol.once(id, "task-vanished", "task %s (job %s index %d, first seen %s) is gone from the hive",
				id, j.id, tr.index, tr.firstAt.Format("15:04:05.000"))
		}
		delete(ck.tasks, id)
		delete(ck.views, id)
	}
}

// checkJobView: the job state follows from its counts (DESIGN 8.3).
func (ck *chaosChecker) checkJobView(j *chaosJob, jv proto.JobView, canceled bool) {
	v := ck.c.viol
	c := jv.Counts
	if sumCounts(c) != j.count() || jv.Count != j.count() {
		v.add("job-counts", "job %s: counts %+v sum to %d, count %d", j.id, c, sumCounts(c), j.count())
	}
	done := c.Succeeded + c.Failed + c.Canceled
	switch jv.State {
	case proto.JobSucceeded:
		if c.Succeeded != j.count() {
			v.add("job-state", "job %s is succeeded with counts %+v", j.id, c)
		}
	case proto.JobFailed:
		if done != j.count() || c.Failed == 0 {
			v.add("job-state", "job %s is failed with counts %+v", j.id, c)
		}
	case proto.JobCanceled:
		if c.Pending+c.Assigned+c.Running != 0 {
			v.add("job-state", "job %s is canceled with counts %+v", j.id, c)
		}
		if !canceled && c.Canceled == 0 {
			v.add("job-state", "job %s is canceled but was never canceled (counts %+v)", j.id, c)
		}
	case proto.JobQueued, proto.JobRunning:
		if done == j.count() {
			v.add("job-state", "job %s is %s with all tasks done: %+v", j.id, jv.State, c)
		}
		if canceled {
			v.add("job-state", "job %s is %s after its cancel was acknowledged", j.id, jv.State)
		}
	}
}

// checkTask checks one task view; it returns false when the task's final
// state is not settled yet.
func (ck *chaosChecker) checkTask(j *chaosJob, tv proto.TaskView, canceled bool, epoch int) bool {
	v := ck.c.viol
	ck.mu.Lock()
	ck.views[tv.ID] = tv
	tr := ck.tasks[tv.ID]
	if tr == nil {
		tr = &taskTrack{job: j, index: tv.Index, firstAt: time.Now()}
		ck.tasks[tv.ID] = tr
	}
	prev := *tr
	ck.mu.Unlock()

	if tv.Index < 0 || tv.Index >= j.count() {
		v.add("task-index", "task %s of job %s has index %d", tv.ID, j.id, tv.Index)
		return true
	}
	if tv.Attempt > maxAttempts {
		v.once(tv.ID, "attempt-cap", "task %s: attempt %d > 1 + retries + MaxInterruptions = %d", tv.ID, tv.Attempt, maxAttempts)
	}
	if tv.Failures > 2 {
		v.once(tv.ID, "failure-cap", "task %s: %d failures > 1 + retries", tv.ID, tv.Failures)
	}
	// Failures, node errors and interruptions against the history (DESIGN
	// 8.3: only exit and timeout consume an attempt).
	var hFail, hNodeErr, hInterrupt, hSucc int
	succAttempt := 0
	for _, h := range tv.History {
		switch h.Outcome {
		case "failed", "timeout":
			hFail++
		case "node_error":
			hNodeErr++
		case "lost", "preempted", "canceled":
			hInterrupt++
		case "succeeded":
			hSucc++
			succAttempt = h.Attempt
		}
	}
	if len(tv.History) == tv.Attempt { // nothing truncated
		if tv.Failures != hFail {
			v.once(tv.ID, "failures-vs-history", "task %s: failures %d but %d failed/timeout attempts in %s", tv.ID, tv.Failures, hFail, historyString(tv.History))
		}
		if tv.NodeErrors != hNodeErr {
			v.once(tv.ID, "nodeerrors-vs-history", "task %s: node_errors %d but %d node_error attempts in %s", tv.ID, tv.NodeErrors, hNodeErr, historyString(tv.History))
		}
		if tv.Interruptions > hInterrupt {
			v.once(tv.ID, "interruptions-vs-history", "task %s: interruptions %d but only %d lost/preempted/canceled attempts in %s", tv.ID, tv.Interruptions, hInterrupt, historyString(tv.History))
		}
	}
	if hSucc > 1 {
		v.once(tv.ID, "two-successes", "task %s succeeded twice: %s", tv.ID, historyString(tv.History))
	}
	for i := 1; i < len(tv.History); i++ {
		if tv.History[i].Attempt != tv.History[i-1].Attempt+1 {
			v.once(tv.ID, "history-order", "task %s: history attempts not consecutive: %s", tv.ID, historyString(tv.History))
			break
		}
	}

	// Terminal states are final; a success keeps its attempt and outputs.
	if prev.terminal != "" {
		switch {
		case tv.State != prev.terminal:
			v.once(tv.ID, "terminal-changed", "task %s (job %s index %d) was %s (attempt %d) and is now %s (attempt %d): %s",
				tv.ID, j.id, tv.Index, prev.terminal, prev.attempt, tv.State, tv.Attempt, historyString(tv.History))
		case tv.State == proto.TaskSucceeded && (tv.Attempt != prev.attempt || !sameOutputs(tv.Outputs, prev.outputs)):
			v.once(tv.ID, "success-changed", "task %s: succeeded as attempt %d with %v, now attempt %d with %v",
				tv.ID, prev.attempt, prev.outputs, tv.Attempt, tv.Outputs)
		}
	} else if tv.State.Terminal() {
		ck.mu.Lock()
		tr.terminal, tr.attempt, tr.outputs, tr.terminalAt = tv.State, tv.Attempt, tv.Outputs, time.Now()
		ck.mu.Unlock()
	}

	if tv.State == proto.TaskSucceeded {
		if succAttempt != tv.Attempt {
			v.once(tv.ID, "success-attempt", "task %s is succeeded at attempt %d but its history says %s", tv.ID, tv.Attempt, historyString(tv.History))
		}
		ck.checkOutputs(j, tv, tv.Attempt)
	}
	if !tv.State.Terminal() {
		return false
	}
	if canceled {
		return true
	}
	// Uncanceled jobs: each kind has one right outcome.
	kind := j.kinds[tv.Index]
	capped := tv.State == proto.TaskFailed && strings.HasPrefix(tv.Error, "too many interruptions")
	if capped {
		if tv.Attempt < maxAttempts {
			v.once(tv.ID, "interruption-cap-early", "task %s failed with %q at attempt %d < %d", tv.ID, tv.Error, tv.Attempt, maxAttempts)
		}
		ck.mu.Lock()
		ck.softFail[tv.ID] = fmt.Sprintf("kind %c: %s; %s", kind, tv.Error, historyString(tv.History))
		ck.mu.Unlock()
		return true
	}
	switch kind {
	case kindNormal, kindLong, kindFlaky, kindVeryLong:
		maxFail := 0
		if kind == kindFlaky {
			maxFail = 1
		}
		if tv.State != proto.TaskSucceeded {
			v.once(tv.ID, "wrong-outcome", "task %s (job %s index %d kind %c) ended %s (%s: %s) instead of succeeded: %s",
				tv.ID, j.id, tv.Index, kind, tv.State, tv.ErrorKind, tv.Error, historyString(tv.History))
		} else if tv.Failures > maxFail {
			v.once(tv.ID, "unexpected-failure", "task %s (kind %c) succeeded after %d counted failures (max %d): %s",
				tv.ID, kind, tv.Failures, maxFail, historyString(tv.History))
		}
	case kindFail, kindTime:
		wantKind, wantExit := proto.ErrExit, 7
		if kind == kindTime {
			wantKind, wantExit = proto.ErrTimeout, -1
		}
		exit := -1
		if tv.ExitCode != nil {
			exit = *tv.ExitCode
		}
		if tv.State != proto.TaskFailed || tv.ErrorKind != wantKind || (wantExit >= 0 && exit != wantExit) || tv.Failures != 2 {
			v.once(tv.ID, "wrong-outcome", "task %s (job %s index %d kind %c) ended %s kind %q exit %d failures %d (%s): %s",
				tv.ID, j.id, tv.Index, kind, tv.State, tv.ErrorKind, exit, tv.Failures, tv.Error, historyString(tv.History))
		}
	}
	return true
}

// attemptOutcome is how attempt ended in the task's history ("" if not
// there).
func attemptOutcome(tv proto.TaskView, attempt int) string {
	for _, h := range tv.History {
		if h.Attempt == attempt {
			return h.Outcome
		}
	}
	return ""
}

func historyString(h []proto.AttemptView) string {
	var parts []string
	for _, a := range h {
		s := fmt.Sprintf("#%d@%s:%s", a.Attempt, a.Node, a.Outcome)
		if a.Error != "" {
			s += "(" + a.Error + ")"
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func sameOutputs(a, b []proto.Output) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkOutputs: a succeeded task has exactly its expected outputs, with
// the content of the attempt that succeeded.
func (ck *chaosChecker) checkOutputs(j *chaosJob, tv proto.TaskView, attempt int) {
	want := j.expectedOutputs(tv.Index, attempt)
	got := map[string]proto.Output{}
	for _, o := range tv.Outputs {
		got[o.Name] = o
	}
	ok := len(got) == len(want) && len(tv.Outputs) == len(want)
	for name, data := range want {
		o, has := got[name]
		if !has || o.Blob != sha256hex(data) || o.Size != int64(len(data)) {
			ok = false
		}
	}
	if !ok {
		var names []string
		for _, o := range tv.Outputs {
			names = append(names, fmt.Sprintf("%s=%s/%d", o.Name, short(o.Blob), o.Size))
		}
		var wnames []string
		for name, data := range want {
			wnames = append(wnames, fmt.Sprintf("%s=%s/%d", name, short(sha256hex(data)), len(data)))
		}
		sort.Strings(wnames)
		ck.c.viol.once(tv.ID, "outputs", "task %s (job %s index %d, state %s attempt %d): outputs %v, want %v",
			tv.ID, j.id, tv.Index, tv.State, attempt, names, wnames)
	}
}

// --- node side ---

// checkNodes compares every running agent's held tasks with the hive and
// checks canceled jobs stopped running.
func (ck *chaosChecker) checkNodes(nodes []proto.NodeView, epoch int) {
	c := ck.c
	now := time.Now()
	seen := map[string]bool{}
	views := map[string]proto.NodeView{}
	for _, nv := range nodes {
		views[nv.ID] = nv
	}
	for _, n := range c.nodes {
		a, up := n.current()
		if !up || a == nil {
			continue
		}
		disturb := n.disturbance(epoch)
		for _, h := range a.chaosHeld() {
			ck.mu.Lock()
			tv, known := ck.views[h.id]
			j := ck.byID[h.job]
			ck.mu.Unlock()
			if j != nil {
				if at, ok := j.canceled(); ok && !h.reporting {
					j.mu.Lock()
					calm := j.cancelEpoch[n.id] == disturb
					j.mu.Unlock()
					switch {
					case now.Sub(at) > cancelBound:
						c.viol.once(h.lease, "cancel-not-stopped", "node %s still runs task %s (job %s, attempt %d, phase %s) %s after the job's cancel was acknowledged",
							n.id, h.id, j.id, h.attempt, h.phase, now.Sub(at).Round(time.Second))
					case calm && now.Sub(at) > cancelQuick:
						c.viol.once(h.lease, "cancel-slow", "node %s still runs task %s (job %s, attempt %d, phase %s) %s after the job's cancel, with neither it nor the hive disturbed since",
							n.id, h.id, j.id, h.attempt, h.phase, now.Sub(at).Round(time.Second))
					}
				}
			}
			// The node holds what the hive has assigned to it, or lets go.
			ok := known && (tv.State == proto.TaskAssigned || tv.State == proto.TaskRunning) && tv.Node == n.id && tv.Attempt == h.attempt
			key := "held|" + n.id + "|" + h.lease
			seen[key] = true
			if !ok {
				what := fmt.Sprintf("node %s holds task %s attempt %d (phase %s, reporting %v); hive: %s attempt %d on %s",
					n.id, h.id, h.attempt, h.phase, h.reporting, tv.State, tv.Attempt, tv.Node)
				ck.inconsistent(key, disturb, what, "held-forever")
			}
		}
		// The hive's assignments to this node are held by it.
		held := map[string]int{}
		for _, h := range a.chaosHeld() {
			held[h.id] = h.attempt
		}
		ck.mu.Lock()
		var active []proto.TaskView
		for _, tv := range ck.views {
			if (tv.State == proto.TaskAssigned || tv.State == proto.TaskRunning) && tv.Node == n.id {
				active = append(active, tv)
			}
		}
		ck.mu.Unlock()
		for _, tv := range active {
			if held[tv.ID] == tv.Attempt {
				continue
			}
			key := fmt.Sprintf("assigned|%s|%s|%d", n.id, tv.ID, tv.Attempt)
			seen[key] = true
			ck.inconsistent(key, disturb, fmt.Sprintf("hive has task %s attempt %d %s on %s; the node holds attempt %d",
				tv.ID, tv.Attempt, tv.State, n.id, held[tv.ID]), "assigned-forever")
		}
	}
	ck.mu.Lock()
	for k := range ck.incons {
		if !seen[k] {
			delete(ck.incons, k)
		}
	}
	ck.mu.Unlock()
	ck.checkCanceledProcs()
}

// convergeBound is how long a node and the hive may disagree about an
// assignment while neither the node nor the hive is being disturbed.
const convergeBound = 45 * time.Second

// cancelBound is how long after a job's cancel was acknowledged its tasks
// may keep running on nodes (a partitioned node learns of it only after
// it re-registers).
const cancelBound = 90 * time.Second

// cancelQuick bounds the same on a node whose link to the hive was not
// disturbed since the cancel: a heartbeat (1 s) carries the cancel.
const cancelQuick = 15 * time.Second

func (ck *chaosChecker) inconsistent(key string, disturb int, what, id string) {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	in := ck.incons[key]
	if in == nil || in.epoch != disturb {
		ck.incons[key] = &inconsistency{since: time.Now(), epoch: disturb, what: what}
		return
	}
	if time.Since(in.since) > convergeBound {
		ck.c.viol.add(id, "%s, for %s", what, time.Since(in.since).Round(time.Second))
		in.since = time.Now() // report again only after another bound
	}
}

// checkCanceledProcs looks for task processes of canceled jobs, and
// records how long after its cancel each job's tasks stopped.
func (ck *chaosChecker) checkCanceledProcs() {
	procs := taskProcs()
	now := time.Now()
	running := map[string]bool{}
	for _, n := range ck.c.nodes {
		if a, up := n.current(); up {
			for _, h := range a.chaosHeld() {
				if !h.reporting {
					running[h.job] = true
				}
			}
		}
	}
	for _, j := range ck.jobList() {
		at, ok := j.canceled()
		if !ok {
			continue
		}
		ck.mu.Lock()
		_, stopped := ck.cancelStop[j.id]
		if !stopped && !running[j.id] && len(procs[j.id]) == 0 {
			ck.cancelStop[j.id] = now.Sub(at)
		}
		ck.mu.Unlock()
		if now.Sub(at) < cancelBound+15*time.Second {
			continue
		}
		if pids := procs[j.id]; len(pids) > 0 {
			ck.c.viol.once(j.id, "cancel-procs", "job %s was canceled %s ago but processes %v still run", j.id, now.Sub(at).Round(time.Second), pids)
		}
	}
}

func (ck *chaosChecker) maxCancelLatency() time.Duration {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	var m time.Duration
	for _, d := range ck.cancelStop {
		m = max(m, d)
	}
	return m
}

// taskProcs maps job IDs to the PIDs of processes running their tasks
// (found by SAVIOR_JOB_ID in the environment).
func taskProcs() map[string][]int {
	out := map[string][]int{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(string(b), "\x00") {
			if id, ok := strings.CutPrefix(kv, "SAVIOR_JOB_ID="); ok {
				out[id] = append(out[id], pid)
			}
		}
	}
	return out
}

// checkHistoryJustified: every attempt's outcome in a task's history was
// caused by a report the hive accepted for that attempt's own lease, or by
// the hive itself (lost; hive deadlines; an admin cancel). A report for one
// lease must never decide the outcome of another (DESIGN 8.4).
func (ck *chaosChecker) checkHistoryJustified(views map[string]proto.TaskView) {
	p := ck.c.proxy
	type key struct {
		lease string
		state proto.TaskState
	}
	accepted := map[key][]*reportRec{}
	rejected400 := map[string]bool{}
	for _, r := range p.acceptedReports(true) {
		accepted[key{r.lease, r.state}] = append(accepted[key{r.lease, r.state}], r)
	}
	p.mu.Lock()
	for _, r := range p.reports {
		if r.status == http.StatusBadRequest {
			rejected400[r.lease] = true
		}
	}
	leaseOf := map[string]map[int]string{}
	for task, ls := range p.byTask {
		leaseOf[task] = map[int]string{}
		for _, l := range ls {
			leaseOf[task][l.attempt] = l.lease
		}
	}
	p.mu.Unlock()
	for id, tv := range views {
		j := ck.jobByID(tv.JobID)
		canceled := false
		if j != nil {
			_, canceled = j.canceled()
		}
		for _, h := range tv.History {
			lease, ok := leaseOf[id][h.Attempt]
			if !ok || h.FinishedAt.IsZero() {
				continue // dispatched before the proxy saw it, or still open
			}
			has := func(st proto.TaskState, kinds ...string) bool {
				for _, r := range accepted[key{lease, st}] {
					if len(kinds) == 0 || containsStr(kinds, r.kind) {
						return true
					}
				}
				return false
			}
			ok = true
			switch h.Outcome {
			case "succeeded":
				ok = has(proto.TaskSucceeded)
			case "failed":
				ok = has(proto.TaskFailed, proto.ErrExit)
			case "timeout":
				ok = has(proto.TaskFailed, proto.ErrTimeout) || strings.HasPrefix(h.Error, "hive deadline")
			case "node_error":
				ok = has(proto.TaskFailed, proto.ErrInput, proto.ErrSandbox, proto.ErrOutput, proto.ErrInternal, "") || rejected400[lease]
			case "preempted":
				ok = has(proto.TaskPreempted)
			case "canceled":
				ok = canceled || has(proto.TaskCanceled)
			}
			if !ok {
				ck.c.viol.once(id+"|"+strconv.Itoa(h.Attempt), "history-unjustified",
					"task %s attempt %d (lease %s) ended %q but the hive accepted no such report for that lease: %s",
					id, h.Attempt, short(lease), h.Outcome, historyString(tv.History))
			}
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// checkLogsAndZips: a succeeded task's stored log holds only its own
// lines from the attempt that succeeded (DESIGN 8.5: every dispatch starts
// a new stream; logs are best effort, so lines may be missing), and each
// finished job's outputs.zip holds exactly its succeeded tasks' outputs.
func (ck *chaosChecker) checkLogsAndZips(views map[string]proto.TaskView) {
	c := ck.c
	v := c.viol
	logs := 0
	for id, tv := range views {
		if tv.State != proto.TaskSucceeded || logs >= 300 {
			continue
		}
		j := ck.jobByID(tv.JobID)
		if j == nil {
			continue
		}
		logs++
		req, _ := http.NewRequest(http.MethodGet, "https://"+c.addr+"/api/v1/admin/tasks/"+id+"/log", nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.admin.Do(req)
		if err != nil {
			v.add("final", "task log: %v", err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if att := resp.Header.Get("X-Savior-Log-Attempt"); att != "" && att != strconv.Itoa(tv.Attempt) {
			v.once(id, "log-attempt", "task %s succeeded as attempt %d but its log is attempt %s", id, tv.Attempt, att)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "chaos start ") && !strings.HasPrefix(line, "chaos done ") {
				continue
			}
			want := fmt.Sprintf(" %s index=%d attempt=%d", j.name, tv.Index, tv.Attempt)
			if !strings.Contains(line+" ", want+" ") {
				v.once(id, "log-foreign-line", "task %s (%s index %d, succeeded as attempt %d) has a log line of another run: %q",
					id, j.name, tv.Index, tv.Attempt, line)
			}
		}
	}
	zips := 0
	for _, j := range ck.jobList() {
		if zips >= 40 {
			break
		}
		want := map[string]string{} // task-<index>/<name> -> sha256
		for _, tv := range views {
			if tv.JobID != j.id || len(tv.Outputs) == 0 {
				continue
			}
			for _, o := range tv.Outputs {
				want[fmt.Sprintf("task-%d/%s", tv.Index, o.Name)] = o.Blob
			}
		}
		if len(want) == 0 {
			continue
		}
		zips++
		req, _ := http.NewRequest(http.MethodGet, "https://"+c.addr+"/api/v1/admin/jobs/"+j.id+"/outputs.zip", nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.admin.Do(req)
		if err != nil {
			v.add("final", "outputs.zip: %v", err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if resp.StatusCode != 200 || err != nil {
			v.add("outputs-zip", "job %s outputs.zip: %d %v", j.id, resp.StatusCode, err)
			continue
		}
		got := map[string]string{}
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			h := sha256.New()
			io.Copy(h, rc)
			rc.Close()
			got[f.Name] = hex.EncodeToString(h.Sum(nil))
		}
		if len(got) != len(want) {
			v.add("outputs-zip", "job %s outputs.zip has %d entries, its tasks %d outputs", j.id, len(got), len(want))
		}
		for name, sum := range want {
			if got[name] != sum {
				v.add("outputs-zip", "job %s outputs.zip %s: sha256 %s, want %s", j.id, name, short(got[name]), short(sum))
			}
		}
	}
}

// --- end-of-run checks ---

// finalChecks runs once everything should have settled.
func (ck *chaosChecker) finalChecks() {
	c := ck.c
	v := c.viol
	// Every uncanceled job finished; every canceled job is canceled.
	var jobs []proto.JobView
	if err := c.adminGet("/api/v1/admin/jobs?limit=1000", &jobs); err != nil {
		v.add("final", "list jobs: %v", err)
		return
	}
	views := map[string]proto.JobView{}
	for _, jv := range jobs {
		views[jv.ID] = jv
	}
	for _, j := range ck.jobList() {
		jv := views[j.id]
		_, canceled := j.canceled()
		switch {
		case canceled && jv.State != proto.JobCanceled:
			v.add("final-job-state", "canceled job %s is %s %+v", j.id, jv.State, jv.Counts)
		case !jv.State.Terminal():
			v.add("not-terminal", "job %s (%s) is still %s %+v after the faults stopped", j.id, j.name, jv.State, jv.Counts)
		}
	}
	// Accepted success reports agree with the hive's final view.
	succ := c.proxy.succeededLeases()
	ck.mu.Lock()
	views2 := map[string]proto.TaskView{}
	for id, tv := range ck.views {
		views2[id] = tv
	}
	ck.mu.Unlock()
	for id, tv := range views2 {
		leases := succ[id]
		if tv.State == proto.TaskSucceeded {
			match := false
			for _, a := range leases {
				match = match || a == tv.Attempt
			}
			if !match {
				v.add("success-without-report", "task %s is succeeded (attempt %d) but the hive accepted no succeeded report for that attempt (accepted: %v); reports: %s",
					id, tv.Attempt, leases, c.proxy.reportTrail(id))
			}
		}
	}
	// Every accepted failure report was booked by its kind (DESIGN 8.3).
	for _, r := range c.proxy.acceptedReports(false) {
		tv, ok := views2[r.task]
		if !ok || r.state != proto.TaskFailed || r.attempt == 0 {
			continue
		}
		if l := c.proxy.leaseFor(r.lease); l == nil || l.lostMaybe {
			continue // a crash may have discarded it; its attempt number may be reused
		}
		var h *proto.AttemptView
		for i := range tv.History {
			if tv.History[i].Attempt == r.attempt {
				h = &tv.History[i]
			}
		}
		if h == nil || h.Outcome == "canceled" {
			continue
		}
		counts := proto.CountsAsAttempt(r.kind)
		if counts && h.Outcome != "failed" && h.Outcome != "timeout" || !counts && h.Outcome != "node_error" {
			v.add("failure-kind-booking", "task %s attempt %d: accepted failed report kind %q booked as %q", r.task, r.attempt, r.kind, h.Outcome)
		}
	}
	ck.checkHistoryJustified(views2)
	// Node errors: on healthy machines with sandbox = none, none must
	// happen. Transfers the faults break are retried (input, output), and a
	// task is never given back to a node still running an older lease of it
	// (internal: workdir collision after a crash).
	for _, r := range c.proxy.acceptedReports(true) {
		if r.state != proto.TaskFailed || r.src != "node" {
			continue
		}
		if tv, ok := views2[r.task]; ok && attemptOutcome(tv, r.attempt) == "canceled" {
			// The job was canceled while the task ran: e.g. its upload was
			// refused (403, no running task), and the hive accepted the
			// report without booking it (DESIGN 8.4).
			continue
		}
		switch r.kind {
		case proto.ErrInternal, proto.ErrSandbox, proto.ErrInput, proto.ErrOutput:
			v.once(r.lease, "node-error-"+r.kind, "task %s attempt %d on %s failed with a node error (%s): %s", r.task, r.attempt, r.node, r.kind, r.err)
		}
	}
	ck.checkLogsAndZips(views2)
	// Output blobs hold what their hashes say.
	checked := map[string]bool{}
	for _, tv := range views2 {
		for _, o := range tv.Outputs {
			if checked[o.Blob] {
				continue
			}
			checked[o.Blob] = true
			req, _ := http.NewRequest(http.MethodGet, "https://"+c.addr+"/api/v1/blobs/"+o.Blob, nil)
			req.Header.Set("Authorization", "Bearer "+c.token)
			resp, err := c.admin.Do(req)
			if err != nil {
				v.add("final", "get blob: %v", err)
				continue
			}
			h := sha256.New()
			n, _ := ioCopy(h, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || hex.EncodeToString(h.Sum(nil)) != o.Blob || n != o.Size {
				v.add("blob-content", "blob %s (task %s %s): status %d, %d bytes, hash %s", o.Blob, tv.ID, o.Name, resp.StatusCode, n, hex.EncodeToString(h.Sum(nil)))
			}
		}
	}
}
