package node

import (
	"context"
	"sync"
	"time"

	"github.com/platteration/ewastesavior/internal/auth"
	"github.com/platteration/ewastesavior/internal/proto"
)

// task is one assignment held by this node, from the moment the claim
// response is decoded until its final report is acknowledged.
type task struct {
	t      proto.Task
	cancel context.CancelFunc
	reason string // why we canceled it (log only)

	mu       sync.Mutex
	phase    string
	runS     float64
	xfer     int64
	report   *proto.TaskReport // final report waiting in the outbox
	logs     *logStream
	finished chan struct{}

	preempted bool // preemptAll has asked the runner to stop it
}

func (t *task) snapshot() proto.RunningTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	return proto.RunningTask{ID: t.t.ID, Lease: t.t.Lease, Phase: t.phase, RunS: t.runS, XferBytes: t.xfer}
}

// runningLocked lists held tasks (a.mu held).
func (a *Agent) runningLocked() []proto.RunningTask {
	out := make([]proto.RunningTask, 0, len(a.tasks))
	for _, t := range a.tasks {
		out = append(out, t.snapshot())
	}
	return out
}

// freeLocked is total minus what held tasks reserve, or zero when the
// power policy or drain says no (a.mu held).
func (a *Agent) freeLocked() proto.Resources {
	if a.runner == nil || a.directives.Drain || a.pending || a.decision.Pause || !a.decision.Accept && a.decision.Reason != "" {
		return proto.Resources{}
	}
	free := a.total
	for _, t := range a.tasks {
		free = free.Sub(t.t.Resources)
	}
	if free.Cores < 0 {
		free.Cores = 0
	}
	if free.MemMB < 0 {
		free.MemMB = 0
	}
	if free.DiskMB < 0 {
		free.DiskMB = 0
	}
	return free
}

// claimSlotsLocked is how many more tasks the runner can start without
// waiting (a.mu held). The runner's FreeSlots alone lags behind: a task
// takes its uid slot only when its goroutine gets into runner.Run, so a
// task claimed a moment ago may not show there yet. Every held task that
// has no final report yet is therefore counted as busy too. Tasks claimed
// beyond the slots would sit in "fetching" with no progress until the
// hive's stall deadline fails them.
func (a *Agent) claimSlotsLocked() int {
	if a.runner == nil {
		return 0
	}
	busy := 0
	for _, t := range a.tasks {
		t.mu.Lock()
		if t.report == nil {
			busy++
		}
		t.mu.Unlock()
	}
	return max(0, min(a.runner.FreeSlots(), a.runnerSlots-busy))
}

// claimLoop long-polls the hive for work while we have capacity.
func (a *Agent) claimLoop(ctx context.Context, hc *hiveClient) {
	backoff := time.Second
	claimID := ""
	for ctx.Err() == nil {
		a.mu.Lock()
		free := a.freeLocked()
		slots := a.claimSlotsLocked()
		canClaim := slots > 0 && free.Cores >= 0.1-1e-9 && free.MemMB > 0
		a.mu.Unlock()
		if slots > 4 {
			slots = 4
		}
		if !canClaim {
			select {
			case <-ctx.Done():
				return
			case <-a.wake:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if claimID == "" {
			claimID = auth.NewID(12)
		}
		resp, err := hc.claim(ctx, &proto.ClaimRequest{ClaimID: claimID, Free: free, Max: slots, WaitS: 25})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if code := statusOf(err); code == 401 || code == 403 {
				return // the heartbeat loop re-registers
			}
			a.log.Debug("claim failed", "err", err)
			sleep(ctx, backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue // same claimID: a lost response is replayed, not doubled
		}
		backoff = time.Second
		claimID = ""
		for _, t := range resp.Tasks {
			a.startTask(ctx, hc, t)
		}
	}
}

// startTask registers the task as held (phase fetching) and runs it.
func (a *Agent) startTask(sessionCtx context.Context, hc *hiveClient, t proto.Task) {
	// Tasks outlive a hive session (the hive may restart and re-adopt
	// them), so they run under a background context.
	ctx, cancel := context.WithCancel(context.Background())
	tk := &task{t: t, cancel: cancel, phase: proto.PhaseFetching, finished: make(chan struct{})}
	tk.logs = newLogStream(a, t.ID, t.Lease)
	a.mu.Lock()
	if _, dup := a.tasks[t.Lease]; dup {
		a.mu.Unlock()
		cancel()
		return
	}
	a.tasks[t.Lease] = tk
	a.mu.Unlock()
	a.log.Info("task started", "task", t.ID, "job", t.JobID, "index", t.Index, "attempt", t.Attempt)
	go a.runTask(ctx, tk)
}

func (a *Agent) runTask(ctx context.Context, tk *task) {
	defer close(tk.finished)
	go tk.logs.run(ctx)
	progress := func(rt proto.RunningTask) {
		tk.mu.Lock()
		tk.phase, tk.runS, tk.xfer = rt.Phase, rt.RunS, rt.XferBytes
		tk.mu.Unlock()
	}
	rep := a.runner.Run(ctx, tk.t, tk.logs, progress)
	rep.Lease = tk.t.Lease
	tk.logs.close()
	tk.mu.Lock()
	tk.phase = proto.PhaseReporting
	tk.report = &rep
	tk.mu.Unlock()
	a.log.Info("task finished", "task", tk.t.ID, "state", rep.State, "exit", rep.ExitCode, "error_kind", rep.ErrorKind, "error", rep.Error)
	if !a.shuttingDown.Load() {
		// While stopping, the report can wait: a flush may block for a
		// minute on an unresponsive hive, and the hive requeues the task
		// of a node that went away anyway.
		a.flushOutbox()
	}
	a.poke()
}

// outboxLoop retries final reports until acknowledged.
func (a *Agent) outboxLoop(ctx context.Context, hc *hiveClient) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		a.sendReports(ctx, hc)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// flushOutbox tries to send final reports right away.
func (a *Agent) flushOutbox() {
	a.mu.Lock()
	hc := a.hc
	a.mu.Unlock()
	if hc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		a.sendReports(ctx, hc)
		cancel()
	}
}

func (a *Agent) sendReports(ctx context.Context, hc *hiveClient) {
	a.mu.Lock()
	var ready []*task
	for _, t := range a.tasks {
		t.mu.Lock()
		if t.report != nil {
			ready = append(ready, t)
		}
		t.mu.Unlock()
	}
	a.mu.Unlock()
	for _, t := range ready {
		// Make sure the log tail is on the hive before the final report.
		t.logs.flush(ctx)
		t.mu.Lock()
		rep := t.report
		t.mu.Unlock()
		err := hc.report(ctx, t.t.ID, rep)
		code := statusOf(err)
		if err == nil || code == 409 || code == 404 {
			// 2xx: done. 409: stale lease (hive moved on). Either way drop it.
			a.mu.Lock()
			delete(a.tasks, t.t.Lease)
			a.mu.Unlock()
			if code == 409 {
				a.log.Info("hive discarded a stale task report", "task", t.t.ID)
			}
			a.poke()
			continue
		}
		if ctx.Err() != nil {
			return
		}
		a.log.Debug("task report failed; will retry", "task", t.t.ID, "err", err)
	}
}

// cancelTask stops a held task (running or waiting to report).
func (a *Agent) cancelTask(ref proto.TaskRef, why string) {
	a.mu.Lock()
	t, ok := a.tasks[ref.Lease]
	if ok && t.t.ID != ref.ID {
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		return
	}
	t.mu.Lock()
	reporting := t.report != nil
	t.reason = why
	t.mu.Unlock()
	if reporting {
		// The hive no longer wants the result; stop listing it.
		a.mu.Lock()
		delete(a.tasks, ref.Lease)
		a.mu.Unlock()
		return
	}
	a.log.Info("canceling task", "task", ref.ID, "why", why)
	t.cancel()
}

// dropUnadopted kills held tasks a (restarted) hive didn't adopt.
func (a *Agent) dropUnadopted(adopted map[string]bool) {
	a.mu.Lock()
	var drop []proto.TaskRef
	for lease, t := range a.tasks {
		if !adopted[t.t.ID] {
			drop = append(drop, proto.TaskRef{ID: t.t.ID, Lease: lease})
		}
	}
	a.mu.Unlock()
	for _, r := range drop {
		a.cancelTask(r, "not adopted by the hive after re-registration")
		// Held but unwanted: also forget any pending report.
		a.mu.Lock()
		if t, ok := a.tasks[r.Lease]; ok {
			t.mu.Lock()
			rep := t.report != nil
			t.mu.Unlock()
			if rep {
				delete(a.tasks, r.Lease)
			}
		}
		a.mu.Unlock()
	}
}

// activeTasks lists held tasks that have no final report yet.
func (a *Agent) activeTasks() []*task {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*task
	for _, t := range a.tasks {
		t.mu.Lock()
		if t.report == nil {
			out = append(out, t)
		}
		t.mu.Unlock()
	}
	return out
}

// freezeAll freezes or thaws every active task. Both are idempotent, so
// the power loop repeats a freeze every tick to catch tasks that arrived
// after the pause began.
func (a *Agent) freezeAll(frozen bool) {
	if a.runner == nil {
		return
	}
	for _, t := range a.activeTasks() {
		if err := a.runner.Freeze(t.t.Lease, frozen); err != nil {
			a.log.Debug("freeze", "lease", t.t.Lease, "err", err)
		}
	}
}

// preemptAll stops every active task so the hive requeues it. The power
// loop repeats it every tick while the policy says so, to catch tasks that
// arrived later; each task is logged once.
func (a *Agent) preemptAll(reason string) {
	if a.runner == nil {
		return
	}
	for _, t := range a.activeTasks() {
		if err := a.runner.Preempt(t.t.Lease); err != nil {
			// Not in the runner yet: the next tick tries again.
			a.log.Debug("preempt", "lease", t.t.Lease, "err", err)
			continue
		}
		t.mu.Lock()
		first := !t.preempted
		t.preempted = true
		t.mu.Unlock()
		if first {
			a.log.Warn("preempting task", "task", t.t.ID, "lease", t.t.Lease, "reason", reason)
		}
	}
}

// shutdownGrace bounds how long shutdownTasks waits for all tasks together.
var shutdownGrace = 10 * time.Second

// shutdownTasks cancels everything still running (agent exit or reboot)
// and waits up to shutdownGrace in total for the tasks to wind down.
func (a *Agent) shutdownTasks() {
	a.shuttingDown.Store(true)
	a.mu.Lock()
	var ts []*task
	for _, t := range a.tasks {
		ts = append(ts, t)
	}
	a.mu.Unlock()
	for _, t := range ts {
		t.cancel()
	}
	deadline := time.NewTimer(shutdownGrace)
	defer deadline.Stop()
	for _, t := range ts {
		select {
		case <-t.finished:
		case <-deadline.C:
			a.log.Warn("stopping without waiting for all tasks", "grace", shutdownGrace)
			return
		}
	}
}
