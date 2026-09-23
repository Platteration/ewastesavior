package hive

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, tok string) {
	id := r.PathValue("id")
	var rep proto.TaskReport
	if !decodeJSON(w, r, &rep, false) {
		return
	}
	switch rep.State {
	case proto.TaskRunning, proto.TaskSucceeded, proto.TaskFailed, proto.TaskCanceled, proto.TaskPreempted:
	default:
		writeErr(w, http.StatusBadRequest, "invalid report state %q", proto.Sanitize(string(rep.State), 32, false))
		return
	}
	if rep.Lease == "" {
		writeErr(w, http.StatusBadRequest, "report without lease")
		return
	}
	rep.Error = proto.Sanitize(rep.Error, 4096, true)
	rep.ErrorKind = proto.Sanitize(rep.ErrorKind, 32, false)
	if rep.MaxMemMB < 0 {
		rep.MaxMemMB = 0
	}
	status, msg := func() (int, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := s.nodeByTokenLocked(tok)
		if n == nil {
			return http.StatusUnauthorized, "unknown node token; register again"
		}
		return s.applyReportLocked(n, id, rep, time.Now())
	}()
	if status != http.StatusOK {
		writeErr(w, status, "%s", msg)
		return
	}
	writeOK(w)
}

// applyReportLocked implements DESIGN 8.3/8.4 for one report.
func (s *Server) applyReportLocked(n *node, id string, rep proto.TaskReport, now time.Time) (int, string) {
	t := s.tasks[id]
	terminal := rep.State != proto.TaskRunning
	if t == nil {
		if h, ok := n.held[id]; ok && h.lease == rep.Lease && terminal {
			s.releaseLocked(n, id)
		}
		return http.StatusConflict, "stale lease"
	}
	rep.RunS, rep.CPUSeconds = boundUsage(rep.RunS, rep.CPUSeconds, t.job.Spec.TimeoutS, n.Inventory.Cores)
	if t.liveFor(n.ID, rep.Lease) {
		if !terminal {
			s.markRunningLocked(t)
			if rep.RunS > t.runS {
				t.runS = rep.RunS
			}
			return http.StatusOK, ""
		}
		t.RunS, t.CPUSeconds, t.MaxMemMB = rep.RunS, rep.CPUSeconds, rep.MaxMemMB
		s.cpuTotal += rep.CPUSeconds
		exit := rep.ExitCode
		switch rep.State {
		case proto.TaskSucceeded:
			if err := s.checkOutputsLocked(rep.Outputs); err != nil {
				msg := "outputs rejected: " + err.Error()
				s.nodeErrorLocked(n, t, proto.ErrOutput, msg, &exit, now)
				return http.StatusBadRequest, msg
			}
			s.succeedLocked(t, rep)
		case proto.TaskFailed:
			kind := rep.ErrorKind
			switch kind {
			case proto.ErrExit, proto.ErrTimeout, proto.ErrInput, proto.ErrSandbox, proto.ErrOutput, proto.ErrInternal:
			default:
				kind = proto.ErrInternal
			}
			if proto.CountsAsAttempt(kind) {
				if rep.RunS < fastFailRunS {
					// Only evidence against the node once the same task
					// succeeds elsewhere (see succeedLocked): a broken job
					// fails fast everywhere and must not quarantine the swarm.
					if t.fastFailedOn == nil {
						t.fastFailedOn = map[string]time.Time{}
					}
					t.fastFailedOn[n.ID] = now
				}
				outcome := "failed"
				if kind == proto.ErrTimeout {
					outcome = "timeout"
				}
				s.requeueLocked(t, requeueOpts{outcome: outcome, errKind: kind, err: rep.Error, exitCode: &exit, consume: true})
			} else {
				s.nodeErrorLocked(n, t, kind, rep.Error, &exit, now)
			}
		case proto.TaskPreempted, proto.TaskCanceled:
			// Nothing to cancel on our side: the node gave the task back.
			s.requeueLocked(t, requeueOpts{outcome: string(rep.State), err: rep.Error, interrupt: true})
		}
		t.DoneLease, t.DoneState = rep.Lease, rep.State
		s.notifyLocked()
		return http.StatusOK, ""
	}
	if terminal && t.DoneLease == rep.Lease && t.DoneState == rep.State && t.Node == n.ID {
		return http.StatusOK, "" // idempotent replay
	}
	if terminal && t.CancelRequested && t.active() && t.Node == n.ID && t.Lease == rep.Lease {
		// Accepted without changing the (canceled) state; keep the
		// accounting and any valid outputs.
		t.RunS, t.CPUSeconds, t.MaxMemMB = rep.RunS, rep.CPUSeconds, rep.MaxMemMB
		s.cpuTotal += rep.CPUSeconds
		if len(rep.Outputs) > 0 && s.checkOutputsLocked(rep.Outputs) == nil {
			t.Outputs = rep.Outputs
		}
		t.DoneLease, t.DoneState = rep.Lease, rep.State
		s.settleCanceledLocked(t)
		s.notifyLocked()
		return http.StatusOK, ""
	}
	if h, ok := n.held[id]; ok && h.lease == rep.Lease && terminal {
		s.releaseLocked(n, id)
		s.notifyLocked()
	}
	return http.StatusConflict, "stale lease"
}

// boundUsage sanitizes a report's node-supplied accounting: negative and
// NaN values become 0, run_s is capped at 10x the task's timeout plus an
// hour, and cpu_seconds at run_s (or that cap, when run_s is missing) times
// the node's cores plus 10 % and a second. Without the caps a node could
// push the swarm's cpu_seconds_total to +Inf, which JSON can't encode.
func boundUsage(runS, cpuS float64, timeoutS, cores int) (float64, float64) {
	clamp := func(v, hi float64) float64 {
		if v < 0 || math.IsNaN(v) {
			return 0
		}
		return math.Min(v, hi)
	}
	maxRun := float64(max(timeoutS, 1))*10 + 3600
	runS = clamp(runS, maxRun)
	if cores <= 0 || cores > proto.MaxCores {
		cores = proto.MaxCores
	}
	run := runS
	if run == 0 {
		run = maxRun
	}
	return runS, clamp(cpuS, run*float64(cores)*1.1+1)
}

// checkOutputsLocked validates a report's outputs (DESIGN 8.5).
func (s *Server) checkOutputsLocked(outs []proto.Output) error {
	if len(outs) > proto.MaxOutputFiles {
		return fmt.Errorf("more than %d output files", proto.MaxOutputFiles)
	}
	seen := make(map[string]bool, len(outs))
	var total int64
	for _, o := range outs {
		if !proto.ValidRelPath(o.Name) {
			return fmt.Errorf("invalid output name %q", proto.Sanitize(o.Name, 64, false))
		}
		if seen[o.Name] {
			return fmt.Errorf("duplicate output %q", o.Name)
		}
		seen[o.Name] = true
		if !proto.ValidSHA256(o.Blob) {
			return fmt.Errorf("output %q: invalid blob hash", o.Name)
		}
		b := s.blobMeta[o.Blob]
		if b == nil {
			return fmt.Errorf("output %q: blob %s was not uploaded", o.Name, o.Blob)
		}
		if o.Size != b.Size {
			return fmt.Errorf("output %q: size %d does not match the blob (%d)", o.Name, o.Size, b.Size)
		}
		total += o.Size
		if total > proto.MaxOutputBytes {
			return fmt.Errorf("outputs exceed %d bytes", int64(proto.MaxOutputBytes))
		}
	}
	return nil
}

// requeueOpts describes how an assignment ended.
type requeueOpts struct {
	outcome   string // history outcome: lost, preempted, node_error, failed, timeout
	errKind   string
	err       string
	exitCode  *int
	consume   bool // consumes an attempt (exit, timeout)
	interrupt bool // counts as an interruption (it had been seen running)
	nodeError bool // input/sandbox/output/internal: anti-affinity + NodeErrors
	keepHeld  bool // resources stay charged until the node stops listing it
}

// requeueLocked ends the current assignment of an active task and puts it
// back in the queue, or fails it for good when it has no attempts left
// (DESIGN 8.3).
func (s *Server) requeueLocked(t *task, o requeueOpts) {
	if !t.active() {
		return
	}
	j := t.job
	s.endAttemptLocked(t, o.outcome, o.exitCode, o.err)
	if n := s.nodes[t.Node]; n != nil && !o.keepHeld {
		s.releaseLocked(n, t.ID)
	}
	j.adjust(t.bucket(), -1)
	t.ErrorKind, t.Error, t.ExitCode = o.errKind, o.err, o.exitCode
	if o.nodeError {
		t.NodeErrors++
		if !containsStr(t.FailedNodes, t.Node) {
			t.FailedNodes = append(t.FailedNodes, t.Node)
		}
	}
	if o.interrupt {
		t.Interruptions++
	}
	final := false
	if o.consume {
		t.Failures++
		final = t.Failures >= 1+j.retries()
	}
	if !final && t.Attempt >= 1+j.retries()+proto.MaxInterruptions {
		// Hard cap on dispatches, whatever ended them (DESIGN 8.3).
		final = true
		t.Error = "too many interruptions"
		if o.err != "" {
			t.Error += " (last: " + o.err + ")"
		}
	}
	t.Lease = ""
	t.CancelRequested = false
	t.unconfirmed = false
	switch {
	case j.Canceled:
		t.State = proto.TaskCanceled
		t.FinishedAt = timePtr(s.now())
	case final:
		t.State = proto.TaskFailed
		t.FinishedAt = timePtr(s.now())
	default:
		t.State = proto.TaskPending
		j.requeued = append(j.requeued, t)
		s.notifyLocked()
	}
	j.adjust(t.bucket(), 1)
	s.dirty = true
	s.checkJobDoneLocked(j)
}

// nodeErrorLocked handles input/sandbox/output/internal failures: requeue
// elsewhere (the node joins FailedNodes) and remember the error on the
// task. Like a fast failure it is evidence against the node only once the
// same task succeeds on another node (see succeedLocked): a job whose input
// URL is broken, or whose outputs the hive rejects, fails with node errors
// everywhere and must not quarantine the swarm.
func (s *Server) nodeErrorLocked(n *node, t *task, kind, msg string, exit *int, now time.Time) {
	s.requeueLocked(t, requeueOpts{outcome: "node_error", errKind: kind, err: msg, exitCode: exit, nodeError: true})
	if t.nodeErrOn == nil {
		t.nodeErrOn = map[string]nodeErr{}
	}
	t.nodeErrOn[n.ID] = nodeErr{at: now, kind: kind, msg: msg}
}

// noteNodeErrorLocked counts a node error on n whose task then succeeded
// on another node; quarantineErrors of them within the window quarantine n.
func (s *Server) noteNodeErrorLocked(n *node, e nodeErr) {
	win := s.cfg.tune.quarantineWin
	kept := n.nodeErrs[:0:0]
	for _, at := range n.nodeErrs {
		if e.at.Sub(at) < win {
			kept = append(kept, at)
		}
	}
	n.nodeErrs = append(kept, e.at)
	if len(n.nodeErrs) >= quarantineErrors && n.Quarantine == "" {
		s.quarantineLocked(n, fmt.Sprintf("%d node errors within %s on tasks that then succeeded on other nodes (last: %s: %s)",
			len(n.nodeErrs), win, e.kind, proto.Sanitize(e.msg, 200, false)))
	}
}

// noteFastFailLocked counts distinct tasks that failed within 10 s on n
// but then succeeded on another node.
func (s *Server) noteFastFailLocked(n *node, t *task, now time.Time) {
	win := s.cfg.tune.quarantineWin
	for id, at := range n.fastFails {
		if now.Sub(at) >= win {
			delete(n.fastFails, id)
		}
	}
	n.fastFails[t.ID] = now
	if len(n.fastFails) >= quarantineFast && n.Quarantine == "" {
		s.quarantineLocked(n, fmt.Sprintf("%d different tasks failed within %.0f s of starting here but succeeded on other nodes (in %s)", len(n.fastFails), fastFailRunS, win))
	}
}

func (s *Server) quarantineLocked(n *node, reason string) {
	n.Quarantine = reason
	if s.reservation != nil && s.reservation.nodeID == n.ID {
		s.clearReservationLocked()
	}
	s.dirty = true
	s.log.Warn("node quarantined", "node_id", n.ID, "name", n.Name, "reason", reason)
}

// succeedLocked finishes a task successfully.
func (s *Server) succeedLocked(t *task, rep proto.TaskReport) {
	j := t.job
	// Earlier failures of this task on other nodes are now evidence
	// against those nodes.
	for id, at := range t.fastFailedOn {
		if n := s.nodes[id]; n != nil && id != t.Node {
			s.noteFastFailLocked(n, t, at)
		}
	}
	for id, e := range t.nodeErrOn {
		if n := s.nodes[id]; n != nil && id != t.Node {
			s.noteNodeErrorLocked(n, e)
		}
	}
	t.fastFailedOn, t.nodeErrOn = nil, nil
	exit := rep.ExitCode
	s.endAttemptLocked(t, string(proto.TaskSucceeded), &exit, "")
	if n := s.nodes[t.Node]; n != nil {
		s.releaseLocked(n, t.ID)
	}
	j.adjust(t.bucket(), -1)
	t.State = proto.TaskSucceeded
	t.CancelRequested = false
	t.FinishedAt = timePtr(s.now())
	t.ExitCode = &exit
	t.ErrorKind, t.Error = "", ""
	t.Outputs = rep.Outputs
	t.Lease = ""
	j.adjust(t.bucket(), 1)
	s.completed++
	s.dirty = true
	s.checkJobDoneLocked(j)
}

// settleCanceledLocked turns a cancel-requested assignment into a plain
// canceled task once the node has let go of it.
func (s *Server) settleCanceledLocked(t *task) {
	if !t.active() || !t.CancelRequested {
		return
	}
	if n := s.nodes[t.Node]; n != nil {
		s.releaseLocked(n, t.ID)
	}
	s.finishLogLocked(t)
	t.State = proto.TaskCanceled // bucket unchanged: it was shown as canceled
	t.CancelRequested = false
	t.Lease = ""
	s.dirty = true
}

// releaseLocked stops charging a task's resources to a node.
func (s *Server) releaseLocked(n *node, taskID string) {
	if _, ok := n.held[taskID]; ok {
		delete(n.held, taskID)
		s.notifyLocked()
	}
}

// endAttemptLocked closes the current history entry and saves the log tail.
func (s *Server) endAttemptLocked(t *task, outcome string, exit *int, msg string) {
	if k := len(t.History) - 1; k >= 0 && t.History[k].Attempt == t.Attempt && t.History[k].FinishedAt.IsZero() {
		h := &t.History[k]
		h.FinishedAt = s.now()
		h.Outcome = outcome
		if exit != nil {
			h.ExitCode = *exit
		}
		h.Error = proto.Sanitize(msg, 256, false)
	}
	s.finishLogLocked(t)
}

// checkJobDoneLocked finishes a job whose tasks are all terminal.
func (s *Server) checkJobDoneLocked(j *job) {
	if !j.finished() || j.FinishedAt != nil {
		return
	}
	j.FinishedAt = timePtr(s.now())
	s.markDoneLocked(j)
	s.removeFromQueueLocked(j)
	s.log.Info("job finished", "job", j.ID, "name", j.Spec.Name, "state", j.state())
	s.enforceRetentionLocked(j)
}

// markDoneLocked stamps a job that just finished or was canceled with the
// next DoneSeq, the order in which retention deletes finished jobs.
func (s *Server) markDoneLocked(j *job) {
	j.DoneSeq = s.nextDoneSeq
	s.nextDoneSeq++
}

func (s *Server) removeFromQueueLocked(j *job) {
	for i, x := range s.queue {
		if x == j {
			s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
			return
		}
	}
}

// cancelJobLocked cancels pending tasks immediately and asks nodes to stop
// running ones (DESIGN 8.4 "Cancel").
func (s *Server) cancelJobLocked(j *job) {
	if j.Canceled || j.finished() {
		return
	}
	wall := s.now()
	for _, t := range j.tasks {
		switch {
		case t.State == proto.TaskPending:
			j.adjust(t.bucket(), -1)
			t.State = proto.TaskCanceled
			t.FinishedAt = timePtr(wall)
			j.adjust(t.bucket(), 1)
		case t.active() && !t.CancelRequested:
			j.adjust(t.bucket(), -1)
			t.CancelRequested = true
			t.FinishedAt = timePtr(wall)
			if k := len(t.History) - 1; k >= 0 && t.History[k].FinishedAt.IsZero() {
				t.History[k].FinishedAt, t.History[k].Outcome = wall, string(proto.TaskCanceled)
			}
			j.adjust(t.bucket(), 1)
		}
	}
	j.requeued = nil
	u := j.undispatched()
	j.counts.Pending -= u
	j.counts.Canceled += u
	j.Canceled = true
	j.FinishedAt = timePtr(wall)
	s.markDoneLocked(j)
	if r := s.reservation; r != nil {
		if t := s.tasks[r.taskID]; t == nil || t.job == j {
			s.clearReservationLocked()
		}
	}
	s.removeFromQueueLocked(j)
	s.dirty = true
	s.enforceRetentionLocked(j)
}

// enforceRetentionLocked keeps at most 500 finished jobs and 200k task
// records, deleting the jobs that finished first (by DoneSeq, then Seq), so
// a long job submitted before many short ones keeps its results when it
// finishes last. just, the job that has just finished (or nil), is never
// deleted here.
func (s *Server) enforceRetentionLocked(just *job) {
	var finished []*job
	for _, j := range s.jobs {
		if j.state().Terminal() {
			finished = append(finished, j)
		}
	}
	if len(finished) <= maxFinishedJobs && s.taskRecords <= maxTaskRecords {
		return
	}
	sort.Slice(finished, func(a, b int) bool {
		x, y := finished[a], finished[b]
		if x.DoneSeq != y.DoneSeq {
			return x.DoneSeq < y.DoneSeq // 0 (saved before DoneSeq existed) first
		}
		return x.Seq < y.Seq
	})
	left := len(finished)
	for _, j := range finished {
		if left <= maxFinishedJobs && s.taskRecords <= maxTaskRecords {
			break
		}
		if j == just {
			continue
		}
		s.deleteJobLocked(j)
		left--
	}
}

// deleteJobLocked removes a finished job, its task records and log tails.
// Blobs it referenced become garbage for the next GC.
func (s *Server) deleteJobLocked(j *job) {
	var logs []string
	for _, t := range j.tasks {
		delete(s.tasks, t.ID)
		// Any dispatched task may have a tail file, even when its last
		// attempt wrote no output (LogEnd is reset on every dispatch). The
		// removal goes through the io queue, after any pending tail write.
		if t.Attempt > 0 || t.LogEnd > 0 || t.tail != nil {
			logs = append(logs, s.logPath(t.ID))
		}
		t.log, t.tail = nil, nil
	}
	s.taskRecords -= len(j.tasks)
	delete(s.jobs, j.ID)
	s.removeFromQueueLocked(j)
	s.dirty = true
	if len(logs) > 0 {
		s.io.push(func() {
			for _, p := range logs {
				os.Remove(p)
			}
		})
	}
}

func (s *Server) logPath(taskID string) string {
	return filepath.Join(s.data.path, "logs", filepath.Base(taskID)+".log")
}
