package hive

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request, tok string) {
	var req proto.ClaimRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	count := min(max(req.Max, 1), maxClaimTasks)
	wait := maxClaimWait
	if req.WaitS < int(maxClaimWait/time.Second) {
		wait = time.Duration(max(req.WaitS, 0)) * time.Second
	}
	free := nonNegative(req.Free)
	claimID := proto.Sanitize(req.ClaimID, 128, false)
	setDeadlines(w, wait+15*time.Second)
	ctx := r.Context()

	node, status := s.startClaim(tok)
	if status != 0 {
		if status == http.StatusTooManyRequests {
			writeErr(w, status, "too many concurrent claims")
		} else {
			writeErr(w, status, "unknown node token; register again")
		}
		return
	}
	defer func() {
		s.mu.Lock()
		node.claims--
		s.mu.Unlock()
	}()

	deadline := time.Now().Add(wait)
	first := true
	for {
		tasks, wake, status := s.claimStep(ctx, tok, claimID, free, count, first)
		first = false
		switch {
		case status != 0:
			writeErr(w, status, "unknown node token; register again")
			return
		case ctx.Err() != nil:
			return
		case len(tasks) > 0:
			writeJSON(w, http.StatusOK, proto.ClaimResponse{Tasks: tasks})
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			writeJSON(w, http.StatusOK, proto.ClaimResponse{Tasks: []proto.Task{}})
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-wake:
		case <-timer.C:
		case <-ctx.Done():
		case <-s.shutdown:
			// The hive is stopping and dispatches nothing more; answer now
			// so the HTTP server's shutdown doesn't wait for the poll.
			timer.Stop()
			writeJSON(w, http.StatusOK, proto.ClaimResponse{Tasks: []proto.Task{}})
			return
		}
		timer.Stop()
	}
}

// startClaim authenticates a claim and counts it against the per-node
// limit of concurrent claims (DESIGN 6.3).
func (s *Server) startClaim(tok string) (*node, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodeByTokenLocked(tok)
	if n == nil {
		return nil, http.StatusUnauthorized
	}
	if n.claims >= maxClaimsPerNode {
		return nil, http.StatusTooManyRequests
	}
	n.claims++
	return n, 0
}

// claimStep makes one dispatch attempt. On the first step a repeated
// ClaimID gets the tasks of the earlier claim that are still assigned to
// the node (DESIGN 8.2). Without tasks it returns the channel to wait on.
func (s *Server) claimStep(ctx context.Context, tok, claimID string, free proto.Resources, max int, first bool) ([]proto.Task, <-chan struct{}, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check under the mutex that the request and node are still alive
	// before committing anything.
	if ctx.Err() != nil {
		return nil, nil, 0
	}
	n := s.nodeByTokenLocked(tok)
	if n == nil {
		return nil, nil, http.StatusUnauthorized
	}
	if first && claimID != "" && claimID == n.claimID && len(n.claimTasks) > 0 {
		var tasks []proto.Task
		for _, ref := range n.claimTasks {
			if t := s.tasks[ref.ID]; t != nil && t.liveFor(n.ID, ref.Lease) {
				tasks = append(tasks, s.taskMessage(t))
			}
		}
		if len(tasks) > 0 {
			return tasks, nil, 0
		}
	}
	tasks := s.dispatchLocked(n, free, max, time.Now())
	if len(tasks) > 0 {
		refs := make([]proto.TaskRef, len(tasks))
		for i, t := range tasks {
			refs[i] = proto.TaskRef{ID: t.ID, Lease: t.Lease}
		}
		n.claimID, n.claimTasks = claimID, refs
	}
	return tasks, s.wake, 0
}

// claimEligibleLocked reports whether a node may receive tasks at all
// (DESIGN 6.4, 8.2).
func (s *Server) claimEligibleLocked(n *node, now time.Time) bool {
	return n.Approved && n.hasRole(proto.RoleCompute) && !n.Drain && n.Quarantine == "" &&
		n.status.State != proto.NodePaused && n.isOnline(now, s.cfg.OfflineAfter)
}

// matchesLocked checks a job's requirements against a node (DESIGN 8.2).
func (s *Server) matchesLocked(n *node, spec *proto.JobSpec) bool {
	q := &spec.Requirements
	if len(q.Arch) > 0 && !containsStr(q.Arch, n.Inventory.Arch) {
		return false
	}
	if q.MinMemMB > 0 && n.Inventory.MemTotalMB < q.MinMemMB {
		return false
	}
	for _, f := range q.CPUFlags {
		if !n.Inventory.HasCPUFlag(f) {
			return false
		}
	}
	if len(q.Labels) > 0 {
		labels := n.effectiveLabels()
		for k, v := range q.Labels {
			if got, ok := labels[k]; !ok || got != v {
				return false
			}
		}
	}
	if len(q.Nodes) > 0 && !containsStr(q.Nodes, n.ID) {
		return false
	}
	if q.Isolation != proto.IsolationAny && !n.fullIsolation() {
		return false
	}
	return true
}

// freeLocked is the hive's view of a node's free resources:
// min(reported, Total - Allocated).
func freeLocked(n *node, reported proto.Resources) proto.Resources {
	return nonNegative(reported.Min(n.Total.Sub(n.allocated())))
}

// fitsFree reports whether need fits a node's free resources. Fits treats
// zero disk as "not limited", which is right for a Total but not for free
// space: a node with a disk quota whose disk is fully allocated has none
// left, not an unlimited amount.
func fitsFree(n *node, free, need proto.Resources) bool {
	return free.Fits(need, n.ScratchInRAM) && (n.ScratchInRAM || n.Total.DiskMB <= 0 || need.DiskMB <= free.DiskMB)
}

// dispatchLocked assigns up to max fitting tasks to n, walking jobs in
// queue order (priority desc, Seq asc). It costs O(jobs). A task is never
// given to a node that still holds an earlier lease of it (a hive deadline
// requeues with the resources kept charged): held and listed tasks are
// keyed by task ID. Nothing is dispatched once the hive is shutting down.
func (s *Server) dispatchLocked(n *node, reqFree proto.Resources, max int, now time.Time) []proto.Task {
	if s.closing || !s.claimEligibleLocked(n, now) {
		return nil
	}
	free := freeLocked(n, reqFree)
	var out []proto.Task
	take := func(t *task) {
		s.dispatchOneLocked(n, t, now)
		c := charge(t.job.Spec.Resources, n.ScratchInRAM)
		free = nonNegative(free.Sub(c))
		out = append(out, s.taskMessage(t))
	}

	if n.reservedFor != "" {
		t := s.tasks[n.reservedFor]
		if t == nil || t.State != proto.TaskPending || t.job.Canceled {
			s.clearReservationLocked()
		} else {
			// A reserved node receives only its task, once it fits and
			// the node has let go of any earlier lease of it.
			if _, old := n.held[t.ID]; !old && !(s.storageLow && len(t.job.Spec.Outputs) > 0) &&
				s.matchesLocked(n, &t.job.Spec) && fitsFree(n, free, t.job.Spec.Resources) {
				s.removeRequeuedLocked(t)
				take(t)
			}
			return out
		}
	}

	for _, j := range s.queue {
		if len(out) >= max {
			break
		}
		if j.Canceled || j.counts.Pending == 0 || !s.matchesLocked(n, &j.Spec) || s.storageLow && len(j.Spec.Outputs) > 0 {
			continue
		}
		need := j.Spec.Resources
		for i := 0; i < len(j.requeued) && len(out) < max; {
			if !fitsFree(n, free, need) {
				break
			}
			// A task reserved on another node may still run here if it fits;
			// dispatching it clears the reservation. Skip it while n still
			// holds an earlier lease of it, and (anti-affinity) on a node it
			// failed on while another node could run it.
			t := j.requeued[i]
			if _, old := n.held[t.ID]; old || containsStr(t.FailedNodes, n.ID) && s.otherNodeForLocked(t, n, now) {
				i++
				continue
			}
			j.requeued = append(j.requeued[:i:i], j.requeued[i+1:]...)
			take(t)
		}
		for j.NextIndex < j.Spec.Count && len(out) < max && fitsFree(n, free, need) {
			take(s.newTaskLocked(j))
		}
	}
	return out
}

// otherNodeForLocked reports whether an online eligible node other than
// self, not in the task's FailedNodes, could run it (anti-affinity).
func (s *Server) otherNodeForLocked(t *task, self *node, now time.Time) bool {
	for _, n := range s.nodes {
		if n == self || containsStr(t.FailedNodes, n.ID) || !s.claimEligibleLocked(n, now) {
			continue
		}
		if s.matchesLocked(n, &t.job.Spec) && n.Total.Fits(t.job.Spec.Resources, n.ScratchInRAM) {
			return true
		}
	}
	return false
}

// newTaskLocked creates the record for the job's next undispatched index.
func (s *Server) newTaskLocked(j *job) *task {
	id := "t" + auth.NewID(8)
	for s.tasks[id] != nil {
		id = "t" + auth.NewID(8)
	}
	t := &task{taskRecord: taskRecord{ID: id, JobID: j.ID, Index: j.NextIndex, State: proto.TaskPending}, job: j}
	j.NextIndex++
	j.tasks = append(j.tasks, t)
	s.tasks[id] = t
	s.taskRecords++
	s.dirty = true
	return t // still counted as pending
}

func (s *Server) removeRequeuedLocked(t *task) {
	j := t.job
	for i, x := range j.requeued {
		if x == t {
			j.requeued = append(j.requeued[:i:i], j.requeued[i+1:]...)
			return
		}
	}
}

// dispatchOneLocked starts a new assignment: new lease, Attempt++ and a
// history entry (DESIGN 8.2).
func (s *Server) dispatchOneLocked(n *node, t *task, now time.Time) {
	j := t.job
	wall := s.now()
	j.adjust(t.bucket(), -1)
	t.Attempt++
	t.Lease = auth.NewID(16)
	t.Node = n.ID
	t.State = proto.TaskAssigned
	t.CancelRequested = false
	t.AssignedAt = timePtr(wall)
	t.StartedAt, t.FinishedAt = nil, nil
	t.ExitCode, t.ErrorKind, t.Error = nil, "", ""
	t.LogBase, t.LogEnd = 0, 0
	t.History = append(t.History, proto.AttemptView{Attempt: t.Attempt, Node: n.ID, AssignedAt: wall})
	if len(t.History) > maxHistory {
		t.History = append([]proto.AttemptView(nil), t.History[len(t.History)-maxHistory:]...)
	}
	t.assignedMono, t.xferMono = now, now
	t.seen, t.unconfirmed = false, false
	t.phase, t.runS, t.xfer, t.uploaded = "", 0, 0, 0
	t.log, t.tail = nil, nil
	j.adjust(t.bucket(), 1)
	if j.StartedAt == nil {
		j.StartedAt = timePtr(wall)
	}
	n.held[t.ID] = heldTask{lease: t.Lease, charge: charge(j.Spec.Resources, n.ScratchInRAM)}
	if s.reservation != nil && s.reservation.taskID == t.ID {
		s.clearReservationLocked()
	}
	s.dirty = true
}

// taskMessage builds the task as delivered to a node (DESIGN 8.1).
func (s *Server) taskMessage(t *task) proto.Task {
	spec := &t.job.Spec
	env := make(map[string]string, len(spec.Env)+5)
	for k, v := range spec.Env {
		env[k] = v
	}
	env["SAVIOR_TASK_INDEX"] = strconv.Itoa(t.Index)
	env["SAVIOR_TASK_COUNT"] = strconv.Itoa(spec.Count)
	env["SAVIOR_JOB_ID"] = t.JobID
	env["SAVIOR_TASK_ID"] = t.ID
	env["SAVIOR_ATTEMPT"] = strconv.Itoa(t.Attempt)
	var cmd []string
	if len(spec.Command) > 0 {
		cmd = make([]string, len(spec.Command))
		for i, a := range spec.Command {
			cmd[i] = proto.ExpandTemplate(a, t.Index, spec.Count)
		}
	}
	return proto.Task{
		ID:        t.ID,
		Lease:     t.Lease,
		JobID:     t.JobID,
		JobName:   spec.Name,
		Index:     t.Index,
		Count:     spec.Count,
		Attempt:   t.Attempt,
		Kind:      spec.Kind,
		Command:   cmd,
		Script:    proto.ExpandTemplate(spec.Script, t.Index, spec.Count),
		Env:       env,
		Inputs:    spec.Inputs,
		Outputs:   spec.Outputs,
		Resources: spec.Resources,
		TimeoutS:  spec.TimeoutS,
		Network:   spec.Network,
	}
}

// fitsKnownNodeLocked reports whether any known compute node (online or
// not) could ever run the job's tasks (DESIGN 8.2 warnings).
func (s *Server) fitsKnownNodeLocked(j *job) bool {
	for _, n := range s.nodes {
		if n.hasRole(proto.RoleCompute) && s.matchesLocked(n, &j.Spec) && n.Total.Fits(j.Spec.Resources, n.ScratchInRAM) {
			return true
		}
	}
	return false
}

func (s *Server) jobWarningLocked(j *job) string {
	if j.finished() || j.Canceled {
		return ""
	}
	if s.storageLow && len(j.Spec.Outputs) > 0 && j.counts.Pending > 0 {
		return "the hive's storage is nearly full: tasks with outputs wait until space is freed (savior ctl gc, or delete old jobs)"
	}
	if s.fitsKnownNodeLocked(j) {
		return ""
	}
	r := j.Spec.Resources
	return fmt.Sprintf("no known node can run these tasks (%g cores, %d MB memory, %d MB disk plus requirements); they stay queued until one joins",
		r.Cores, r.MemMB, r.DiskMB)
}

// waitReasonLocked explains why a pending task isn't running.
func (s *Server) waitReasonLocked(t *task, now time.Time) string {
	if t.State != proto.TaskPending {
		return ""
	}
	if s.storageLow && len(t.job.Spec.Outputs) > 0 {
		return "waiting for free space on the hive"
	}
	if r := s.reservation; r != nil && r.taskID == t.ID {
		name := r.nodeID
		if n := s.nodes[r.nodeID]; n != nil {
			name = n.Name
		}
		return "reserved for " + name
	}
	need := t.job.Spec.Resources
	var best *node
	var bestFree proto.Resources
	for _, n := range s.nodes {
		if !s.claimEligibleLocked(n, now) || !s.matchesLocked(n, &t.job.Spec) || !n.Total.Fits(need, n.ScratchInRAM) {
			continue
		}
		f := freeLocked(n, n.status.Free)
		if best == nil || f.Cores > bestFree.Cores {
			best, bestFree = n, f
		}
	}
	switch {
	case best == nil:
		return "no eligible node"
	case bestFree.Cores+1e-9 < need.Cores:
		return fmt.Sprintf("waiting for %g cores", need.Cores)
	case !best.ScratchInRAM && need.MemMB <= bestFree.MemMB && !fitsFree(best, bestFree, need):
		return fmt.Sprintf("waiting for %d MB disk", need.DiskMB)
	case !fitsFree(best, bestFree, need):
		return fmt.Sprintf("waiting for %d MB memory", need.MemMB)
	}
	return "waiting for a node to claim it"
}

// headTaskLocked finds the first job in queue order that has pending tasks
// and could run on some eligible online node's Total, and returns it with
// its next task (nil when the next one is still undispatched).
func (s *Server) headTaskLocked(now time.Time) (*job, *task, []*node) {
	for _, j := range s.queue {
		if j.Canceled || j.counts.Pending == 0 {
			continue
		}
		var cands []*node
		for _, n := range s.nodes {
			if s.claimEligibleLocked(n, now) && s.matchesLocked(n, &j.Spec) && n.Total.Fits(j.Spec.Resources, n.ScratchInRAM) {
				cands = append(cands, n)
			}
		}
		if len(cands) == 0 {
			continue
		}
		if len(j.requeued) > 0 {
			return j, j.requeued[0], cands
		}
		return j, nil, cands
	}
	return nil, nil, nil
}

// updateReservationLocked implements DESIGN 8.2 "Reservation": a head task
// that fits some eligible node's Total but no node's free resources for
// ReserveAfter gets the eligible node with the most free cores reserved.
func (s *Server) updateReservationLocked(now time.Time) {
	if r := s.reservation; r != nil {
		t, n := s.tasks[r.taskID], s.nodes[r.nodeID]
		if t == nil || t.State != proto.TaskPending || t.job.Canceled || n == nil ||
			!s.claimEligibleLocked(n, now) || now.Sub(r.since) >= s.cfg.tune.reserveMax ||
			!s.matchesLocked(n, &t.job.Spec) || !n.Total.Fits(t.job.Spec.Resources, n.ScratchInRAM) {
			s.clearReservationLocked()
		}
		return // at most one reservation at a time
	}
	j, t, cands := s.headTaskLocked(now)
	if j == nil {
		s.headKey = ""
		return
	}
	need := j.Spec.Resources
	for _, n := range cands {
		if fitsFree(n, freeLocked(n, n.status.Free), need) {
			s.headKey = "" // it fits somewhere now; the next claim takes it
			return
		}
	}
	key := j.ID + "#" + strconv.Itoa(j.NextIndex)
	if t != nil {
		key = t.ID
	}
	if key != s.headKey {
		s.headKey, s.headSince = key, now
		return
	}
	if now.Sub(s.headSince) < s.cfg.ReserveAfter {
		return
	}
	sort.Slice(cands, func(a, b int) bool {
		fa, fb := freeLocked(cands[a], cands[a].status.Free), freeLocked(cands[b], cands[b].status.Free)
		if fa.Cores != fb.Cores {
			return fa.Cores > fb.Cores
		}
		return cands[a].ID < cands[b].ID
	})
	n := cands[0]
	if t == nil {
		// Materialize the record so the reservation can name it.
		t = s.newTaskLocked(j)
		j.requeued = append([]*task{t}, j.requeued...)
	}
	s.reservation = &reservation{taskID: t.ID, nodeID: n.ID, since: now}
	n.reservedFor = t.ID
	s.headKey = ""
	s.log.Info("reserved a node for a wide task", "task", t.ID, "node", n.Name, "cores", need.Cores)
}

func (s *Server) clearReservationLocked() {
	if r := s.reservation; r != nil {
		if n := s.nodes[r.nodeID]; n != nil && n.reservedFor == r.taskID {
			n.reservedFor = ""
		}
		s.reservation = nil
		s.notifyLocked()
	}
}
